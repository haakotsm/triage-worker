package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/haakotsm/triage-worker/internal/activity"
	"github.com/haakotsm/triage-worker/internal/types"
)

const (
	AlertSignalName      = "alert"
	ResolveSignalName    = "resolve"
	StatusQueryName      = "get-status"
	CorrelationDebounce  = 60 * time.Second
	CorrelationHardCap   = 5 * time.Minute
)

// WorkflowStatus is the query response for get-status.
type WorkflowStatus struct {
	Step       string `json:"step"`
	AlertCount int    `json:"alert_count"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	Resolved   bool   `json:"resolved"`
}

// TriageWorkflow orchestrates alert correlation, enrichment, and AI diagnosis.
//
// Lifecycle:
//  1. Receives alerts via signals (first alert delivered by SignalWithStart)
//  2. Correlates: waits for related alerts (60s debounce, 5min hard cap)
//  3. Enriches: parallel queries to Prometheus, K8s API, Loki
//  4. Triages: invokes kagent error-triage-agent via A2A protocol
//  5. Reports: stores structured diagnosis to PostgreSQL
//
// The workflow also listens for a "resolve" signal. If received at any point
// before the agent invocation completes, the workflow exits early with
// AutoResolved=true, avoiding wasteful LLM inference on already-resolved issues.
func TriageWorkflow(ctx workflow.Context, params types.TriageParams) (types.TriageResult, error) {
	logger := workflow.GetLogger(ctx)
	startedAt := workflow.Now(ctx)
	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID

	// Activity references (nil pointer — Temporal uses method name for dispatch)
	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	// Short-lived activity options for state updates (non-critical, best-effort).
	stateOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	stateCtx := workflow.WithActivityOptions(ctx, stateOpts)

	// --- Query handler: exposes current workflow step for observability ---
	currentStep := "correlating"
	alertCount := 0
	resolved := false

	err := workflow.SetQueryHandler(ctx, StatusQueryName, func() (WorkflowStatus, error) {
		return WorkflowStatus{
			Step:       currentStep,
			AlertCount: alertCount,
			ElapsedMs:  workflow.Now(ctx).Sub(startedAt).Milliseconds(),
			Resolved:   resolved,
		}, nil
	})
	if err != nil {
		return types.TriageResult{}, fmt.Errorf("register query handler: %w", err)
	}

	// --- Resolve signal listener ---
	// Drains all resolve signals in a loop to prevent buffering.
	resolveCh := workflow.GetSignalChannel(ctx, ResolveSignalName)
	workflow.Go(ctx, func(gCtx workflow.Context) {
		for {
			resolveCh.Receive(gCtx, nil)
			resolved = true
		}
	})

	// --- Step 1: Correlate alerts ---
	debounceWindow := CorrelationDebounce
	if params.CorrelationDebounce > 0 {
		debounceWindow = params.CorrelationDebounce
	}
	hardCap := CorrelationHardCap
	if params.CorrelationHardCap > 0 {
		hardCap = params.CorrelationHardCap
	}
	alerts, err := correlateAlerts(ctx, debounceWindow, hardCap)
	if err != nil {
		return types.TriageResult{}, err
	}
	alertCount = len(alerts)
	logger.Info("correlation complete", "alert_count", alertCount)

	// Check if resolved during correlation window
	if resolved {
		logger.Info("auto-resolved during correlation", "alert_count", alertCount)
		_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
			wfID, "resolved", "").Get(ctx, nil)
		return types.TriageResult{
			WorkflowID:   wfID,
			Identity:     params.Identity,
			AlertCount:   alertCount,
			StartedAt:    startedAt,
			CompletedAt:  workflow.Now(ctx),
			AutoResolved: true,
		}, nil
	}

	// --- Upsert search attributes (must be in workflow code, not activity) ---
	_ = workflow.UpsertTypedSearchAttributes(ctx,
		NamespaceKey.ValueSet(params.Identity.Namespace),
		WorkloadKey.ValueSet(params.Identity.Name),
	)

	// --- Step 2: Parallel enrichment ---
	currentStep = "enriching"
	_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
		wfID, "enriching", "").Get(ctx, nil)

	enrichOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	}
	enrichCtx := workflow.WithActivityOptions(ctx, enrichOpts)

	var promResult types.PrometheusResult
	var k8sResult types.KubernetesResult
	var lokiResult types.LokiResult

	promFuture := workflow.ExecuteActivity(enrichCtx, enrichAct.QueryPrometheus, params.Identity, alerts)
	k8sFuture := workflow.ExecuteActivity(enrichCtx, k8sAct.QueryKubernetesAPI, params.Identity, alerts)
	lokiFuture := workflow.ExecuteActivity(enrichCtx, enrichAct.QueryLoki, params.Identity, alerts)

	// Collect results; partial failures are acceptable
	if err := promFuture.Get(ctx, &promResult); err != nil {
		logger.Warn("prometheus enrichment failed", "error", err)
		promResult = types.PrometheusResult{Available: false, Error: err.Error()}
	}
	if err := k8sFuture.Get(ctx, &k8sResult); err != nil {
		logger.Warn("kubernetes enrichment failed", "error", err)
		k8sResult = types.KubernetesResult{Available: false, Error: err.Error()}
	}
	if err := lokiFuture.Get(ctx, &lokiResult); err != nil {
		logger.Warn("loki enrichment failed", "error", err)
		lokiResult = types.LokiResult{Available: false, Error: err.Error()}
	}

	enrichment := types.EnrichmentResult{
		Prometheus: promResult,
		Kubernetes: k8sResult,
		Loki:       lokiResult,
	}

	// --- Step 3: Invoke triage agent ---
	// Check if resolved during enrichment (avoid expensive LLM call)
	if resolved {
		logger.Info("auto-resolved during enrichment")
		_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
			wfID, "resolved", "").Get(ctx, nil)
		return types.TriageResult{
			WorkflowID:   wfID,
			Identity:     params.Identity,
			AlertCount:   alertCount,
			StartedAt:    startedAt,
			CompletedAt:  workflow.Now(ctx),
			AutoResolved: true,
		}, nil
	}

	currentStep = "triaging"
	_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
		wfID, "triaging", "").Get(ctx, nil)

	agentOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 300 * time.Second,
		HeartbeatTimeout:    90 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"AuthError",
				"ClientError",
				"A2AClientError",
			},
		},
	}
	agentCtx := workflow.WithActivityOptions(ctx, agentOpts)

	// Use a cancellable context for the agent activity so we can abort on resolve.
	agentCancelCtx, cancelAgent := workflow.WithCancel(agentCtx)
	agentFuture := workflow.ExecuteActivity(agentCancelCtx, agentAct.InvokeTriageAgent, alerts, enrichment)

	// Wait for either agent completion or resolve signal
	var report types.TriageReport
	sel := workflow.NewSelector(ctx)
	agentDone := false

	sel.AddFuture(agentFuture, func(f workflow.Future) {
		agentDone = true
		err = f.Get(ctx, &report)
	})

	sel.AddReceive(resolveCh, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, nil)
		resolved = true
	})

	sel.Select(ctx)

	if resolved && !agentDone {
		cancelAgent()
		logger.Info("auto-resolved during triage — cancelling agent activity")
		_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
			wfID, "resolved", "").Get(ctx, nil)
		return types.TriageResult{
			WorkflowID:   wfID,
			Identity:     params.Identity,
			AlertCount:   alertCount,
			StartedAt:    startedAt,
			CompletedAt:  workflow.Now(ctx),
			AutoResolved: true,
		}, nil
	}

	// Agent completed (possibly with error)
	if err != nil {
		logger.Error("agent invocation failed", "error", err)
		_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
			wfID, "failed", "").Get(ctx, nil)
		return types.TriageResult{}, err
	}

	// Upsert classification search attributes after agent response
	_ = workflow.UpsertTypedSearchAttributes(ctx,
		ClassificationKey.ValueSet(report.Classification),
		SeverityKey.ValueSet(report.Severity),
	)

	// --- Step 3.5: Append L1 diagnostic commands ---
	l1Cmds := types.L1Commands(report.Classification, params.Identity)
	report.Recommendations = append(report.Recommendations, l1Cmds...)

	// --- Step 3.6: Compute summary and normalize recommendations ---
	types.NormalizeSummary(params.Identity, &report)
	types.NormalizeRecommendations(&report)

	// --- Step 4: Store report ---
	currentStep = "reporting"
	reportOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	}
	reportCtx := workflow.WithActivityOptions(ctx, reportOpts)

	result := types.TriageResult{
		WorkflowID:     wfID,
		Identity:       params.Identity,
		AlertCount:     alertCount,
		Classification: report.Classification,
		Severity:       report.Severity,
		RootCause:      report.RootCause,
		Report:         &report,
		StartedAt:      startedAt,
		CompletedAt:    workflow.Now(ctx),
	}

	err = workflow.ExecuteActivity(reportCtx, reportAct.StoreTriageReport, result).Get(ctx, nil)
	if err != nil {
		logger.Error("report storage failed", "error", err)
		// Non-fatal: report is in workflow history even if DB write fails
	}

	currentStep = "complete"
	logger.Info("triage complete",
		"classification", report.Classification,
		"severity", report.Severity,
		"confidence", report.Confidence,
		"escalation_needed", report.EscalationNeeded,
	)

	return result, nil
}

// correlateAlerts collects alerts from the signal channel with a debounce window.
// First alert arrives via SignalWithStart. Subsequent alerts arrive as signals.
func correlateAlerts(ctx workflow.Context, debounceWindow, hardCap time.Duration) ([]types.Alert, error) {
	alertCh := workflow.GetSignalChannel(ctx, AlertSignalName)
	alerts := make([]types.Alert, 0, 8)
	seen := make(map[string]bool)
	deadline := workflow.Now(ctx).Add(hardCap)

	for {
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			break
		}

		debounce := debounceWindow
		if debounce > remaining {
			debounce = remaining
		}

		// Wait for new alert or timeout
		timedOut, _ := workflow.AwaitWithTimeout(ctx, debounce, func() bool {
			return alertCh.Len() > 0
		})
		if !timedOut {
			// Debounce expired with no new alerts — proceed
			break
		}

		// Drain all pending signals
		for alertCh.Len() > 0 {
			var alert types.Alert
			alertCh.Receive(ctx, &alert)
			if !seen[alert.Fingerprint] {
				alerts = append(alerts, alert)
				seen[alert.Fingerprint] = true
			}
		}
	}

	if len(alerts) == 0 {
		// Should not happen — at least one alert triggered the workflow
		return nil, temporal.NewNonRetryableApplicationError(
			"no alerts received during correlation window",
			"NoAlerts", nil)
	}

	return alerts, nil
}
