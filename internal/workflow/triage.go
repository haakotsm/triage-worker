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

// TriageWorkflow orchestrates alert correlation, multi-agent investigation, and AI diagnosis.
//
// Lifecycle:
//  1. Receives alerts via signals (first alert delivered by SignalWithStart)
//  2. Correlates: waits for related alerts (60s debounce, 5min hard cap)
//  3. Investigates: 3 parallel investigator agents (K8s, Logs, Metrics) each with MCP tools
//  4. Consolidates: synthesizer agent cross-references findings → TriageReport
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
	var multiAgent *activity.MultiAgentActivity
	var reportAct *activity.ReportActivity

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

	// --- Step 2: Parallel investigation (replaces old enrichment + single agent) ---
	// Each investigator has its own MCP tools and investigates independently.
	// This replaces the old pattern of: enrich(Prometheus, K8s, Loki) → single agent.
	// Benefits: each agent can make multiple tool calls, reason about results, and
	// follow up — unlike the old enrichment activities which did a single query each.
	currentStep = "investigating"
	_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
		wfID, "investigating", "").Get(ctx, nil)

	// Check if resolved before starting expensive agent calls
	if resolved {
		logger.Info("auto-resolved before investigation")
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

	// Investigator activity options:
	// - StartToCloseTimeout: 5 min (agents may make 6-8 tool calls, each takes 5-15s)
	// - HeartbeatTimeout: 180s (agents make multi-turn tool calls via LLM; each turn takes 15-60s)
	// - Retries: 2 attempts (handles transient 429/5xx from agent gateway)
	investigateOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 300 * time.Second,
		HeartbeatTimeout:    180 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    2,
			NonRetryableErrorTypes: []string{
				"AuthError",
				"ClientError",
			},
		},
	}
	investigateCtx := workflow.WithActivityOptions(ctx, investigateOpts)

	// Launch all investigators in parallel.
	// Note: InvokeInvestigator returns (InvestigatorOutput, nil) even on agent failure —
	// the output.Available=false and output.Error fields indicate degraded results.
	// Only transient infrastructure errors (429, 5xx) return an activity error for retry.
	investigators := types.DefaultInvestigators()
	futures := make([]workflow.Future, len(investigators))
	for i, inv := range investigators {
		futures[i] = workflow.ExecuteActivity(investigateCtx, multiAgent.InvokeInvestigator,
			inv.Name, alerts, params.Identity)
	}

	// Collect results with partial failure tolerance.
	// Even if 2 of 3 investigators fail, we proceed with whatever data we have.
	investigations := make([]types.InvestigatorOutput, len(investigators))
	availableCount := 0
	for i, f := range futures {
		var result types.InvestigatorOutput
		if err := f.Get(ctx, &result); err != nil {
			// Activity-level failure (all retries exhausted) — mark as unavailable
			logger.Warn("investigator activity failed",
				"agent", investigators[i].Name, "error", err)
			investigations[i] = types.InvestigatorOutput{
				AgentName: investigators[i].Name,
				Available: false,
				Error:     err.Error(),
			}
		} else {
			investigations[i] = result
			if result.Available {
				availableCount++
			}
		}
	}

	logger.Info("investigation phase complete",
		"total", len(investigators),
		"available", availableCount,
	)

	// --- Step 3: Consolidate findings ---
	// Check if resolved during investigation (avoid expensive consolidation call)
	if resolved {
		logger.Info("auto-resolved during investigation")
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

	currentStep = "consolidating"
	_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
		wfID, "consolidating", "").Get(ctx, nil)

	// If ALL investigators failed, produce a minimal failure report without calling consolidator.
	// This avoids wasting an LLM call with zero useful input.
	var report types.TriageReport
	if availableCount == 0 {
		logger.Warn("all investigators failed — producing minimal report")
		report = types.TriageReport{
			Classification:   "Unknown",
			Severity:         "warning",
			RootCause:        "Investigation failed: all investigator agents were unavailable",
			CausalChain:      []string{"All investigator agents failed or timed out"},
			Confidence:       0.1,
			EscalationNeeded: true,
		}
	} else {
		// Consolidator activity options:
		// - Shorter timeout: consolidator has no tools, just LLM reasoning
		// - HeartbeatTimeout: 180s (LLM synthesis of multi-agent findings takes 60-120s)
		consolidateOpts := workflow.ActivityOptions{
			StartToCloseTimeout: 300 * time.Second,
			HeartbeatTimeout:    180 * time.Second,
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
		consolidateCtx := workflow.WithActivityOptions(ctx, consolidateOpts)

		// Use a cancellable context so we can abort on resolve signal.
		cancelCtx, cancelConsolidator := workflow.WithCancel(consolidateCtx)
		consolidateFuture := workflow.ExecuteActivity(cancelCtx, multiAgent.InvokeConsolidator, alerts, investigations)

		// Wait for either consolidation or resolve signal
		sel := workflow.NewSelector(ctx)
		consolidateDone := false

		sel.AddFuture(consolidateFuture, func(f workflow.Future) {
			consolidateDone = true
			err = f.Get(ctx, &report)
		})

		sel.AddReceive(resolveCh, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, nil)
			resolved = true
		})

		sel.Select(ctx)

		if resolved && !consolidateDone {
			cancelConsolidator()
			logger.Info("auto-resolved during consolidation — cancelling")
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

		if err != nil {
			logger.Error("consolidator invocation failed", "error", err)
			_ = workflow.ExecuteActivity(stateCtx, reportAct.UpdateIncidentState,
				wfID, "failed", "").Get(ctx, nil)
			return types.TriageResult{}, err
		}
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
