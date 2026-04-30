package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/haakotsm/triage-worker/internal/activity"
	"github.com/haakotsm/triage-worker/internal/types"
)

const (
	AlertSignalName      = "alert"
	CorrelationDebounce  = 60 * time.Second
	CorrelationHardCap   = 5 * time.Minute
)

// TriageWorkflow orchestrates alert correlation, enrichment, and AI diagnosis.
//
// Lifecycle:
//  1. Receives alerts via signals (first alert delivered by SignalWithStart)
//  2. Correlates: waits for related alerts (60s debounce, 5min hard cap)
//  3. Enriches: parallel queries to Prometheus, K8s API, Loki
//  4. Triages: invokes kagent error-triage-agent via A2A protocol
//  5. Reports: stores structured diagnosis to PostgreSQL
func TriageWorkflow(ctx workflow.Context, params types.TriageParams) (types.TriageResult, error) {
	logger := workflow.GetLogger(ctx)
	startedAt := workflow.Now(ctx)

	// Activity references (nil pointer — Temporal uses method name for dispatch)
	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity

	// --- Step 1: Correlate alerts ---
	alerts, err := correlateAlerts(ctx)
	if err != nil {
		return types.TriageResult{}, err
	}
	logger.Info("correlation complete", "alert_count", len(alerts))

	// --- Upsert search attributes (must be in workflow code, not activity) ---
	_ = workflow.UpsertTypedSearchAttributes(ctx,
		NamespaceKey.ValueSet(params.Identity.Namespace),
		WorkloadKey.ValueSet(params.Identity.Name),
	)

	// --- Step 2: Parallel enrichment ---
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
	k8sFuture := workflow.ExecuteActivity(enrichCtx, activity.QueryKubernetesAPI, params.Identity, alerts)
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
	agentOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 120 * time.Second,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"ParseError",
				"AuthError",
				"ClientError",
			},
		},
	}
	agentCtx := workflow.WithActivityOptions(ctx, agentOpts)

	var report types.TriageReport
	err = workflow.ExecuteActivity(agentCtx, agentAct.InvokeTriageAgent, alerts, enrichment).Get(ctx, &report)
	if err != nil {
		logger.Error("agent invocation failed", "error", err)
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

	// --- Step 4: Store report ---
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
		WorkflowID:     workflow.GetInfo(ctx).WorkflowExecution.ID,
		Identity:       params.Identity,
		AlertCount:     len(alerts),
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
func correlateAlerts(ctx workflow.Context) ([]types.Alert, error) {
	alertCh := workflow.GetSignalChannel(ctx, AlertSignalName)
	alerts := make([]types.Alert, 0, 8)
	seen := make(map[string]bool)
	deadline := workflow.Now(ctx).Add(CorrelationHardCap)

	for {
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			break
		}

		debounce := CorrelationDebounce
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
