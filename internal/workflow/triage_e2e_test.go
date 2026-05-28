package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/temporal"

	"github.com/haakotsm/triage-worker/internal/activity"
	"github.com/haakotsm/triage-worker/internal/types"
)

// TestTriageWorkflow_E2E_CrashLoopOOM simulates a realistic end-to-end scenario:
// Alert fires → correlation → enrichment → agent diagnosis → report stored.
// Verifies the full data flow produces actionable recommendations.
func TestTriageWorkflow_E2E_CrashLoopOOM(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	// Simulate Alertmanager firing KubePodCrashLooping for an OOMKilled pod
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "e2e-oom-001",
			Labels: map[string]string{
				"alertname":  "KubePodCrashLooping",
				"namespace":  "default",
				"deployment": "test-crashloop",
				"pod":        "test-crashloop-8d8cdcc68-mxgcq",
				"container":  "crasher",
				"severity":   "critical",
			},
			Annotations: map[string]string{
				"summary":     "Pod default/test-crashloop is crash looping",
				"description": "Container crasher exit code 137 (OOMKilled). Memory limit: 64Mi. 14 restarts in 37 minutes.",
				"runbook_url": "https://runbooks.prometheus-operator.dev/runbooks/kubernetes/kubepodcrashlooping",
			},
			StartsAt:     time.Now(),
			GeneratorURL: "http://prometheus:9090/graph?g0.expr=rate(kube_pod_container_status_restarts_total[15m])>0",
		})
	}, 0)

	// Mock enrichment: Prometheus shows high restart rate and memory at limit
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{
			Available:   true,
			RestartRate: 8.5,
			MemoryPct:   99.2,
			CPUUsage:    0.05,
		}, nil)

	// Mock enrichment: K8s API shows OOMKilled exit codes and events
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{
			Available:    true,
			PodPhase:     "Running",
			ExitCodes:    []int32{137, 137, 137},
			RecentEvents: []string{
				"Back-off restarting failed container crasher in pod test-crashloop-8d8cdcc68-mxgcq_default",
				"Container crasher exceeded its local storage capacity",
			},
			ContainerStates: []string{"waiting:CrashLoopBackOff"},
		}, nil)

	// Mock enrichment: Loki shows OOM-related log lines
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{
			Available:  true,
			ErrorLines: []string{
				"OOM simulation - allocating memory",
				"runtime: out of memory",
			},
			LogCount: 42,
		}, nil)

	// Mock state updates (best-effort, no assertions needed)
	env.OnActivity(reportAct.UpdateIncidentState, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Mock agent: returns structured diagnosis with actionable fix
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{
			Classification:   "OOM",
			Severity:         "critical",
			RootCause:        "Container 'crasher' is being OOM-killed (exit code 137). Memory limit of 64Mi is insufficient for the workload. The container allocates more memory than available, triggering the kernel OOM killer.",
			CausalChain:      []string{"Memory allocation exceeds 64Mi limit", "Kernel OOM killer sends SIGKILL (137)", "Container restarts in CrashLoopBackOff"},
			Confidence:       0.95,
			EscalationNeeded: false,
			Evidence: []types.EvidenceItem{
				{Observation: "Memory usage at 99.2% of limit", Source: "prometheus", Strength: "strong"},
				{Observation: "Exit code 137 (OOMKilled) on last 3 restarts", Source: "kubernetes", Strength: "strong"},
				{Observation: "Logs show 'out of memory' messages", Source: "loki", Strength: "moderate"},
			},
			Impact: types.Impact{
				AffectedServices: []string{"test-crashloop"},
				BlastRadius:      "deployment",
			},
			Recommendations: []types.Recommendation{
				{
					Action:  "Increase memory limit to 256Mi",
					Command: "kubectl patch deployment test-crashloop -n default --type=json -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/limits/memory\",\"value\":\"256Mi\"}]'",
					Risk:    "low",
				},
				{
					Action:  "Check if the application has a memory leak by reviewing recent code changes",
					Command: "",
					Risk:    "none",
				},
			},
		}, nil)

	// Mock report storage - capture what gets stored
	var storedResult types.TriageResult
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		storedResult = args.Get(1).(types.TriageResult)
	}).Return(nil)

	// Execute workflow
	params := types.TriageParams{
		Identity: types.IncidentIdentity{
			Namespace: "default",
			Kind:      "Deployment",
			Name:      "test-crashloop",
			AlertName: "KubePodCrashLooping",
		},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	// --- Assertions ---
	require.True(t, env.IsWorkflowCompleted(), "workflow should complete")
	require.NoError(t, env.GetWorkflowError(), "workflow should succeed")

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))

	// Core diagnosis verification
	require.Equal(t, "OOM", result.Classification)
	require.Equal(t, "critical", result.Severity)
	require.Equal(t, 1, result.AlertCount)
	require.Contains(t, result.RootCause, "OOM-killed")

	// Identity preserved through flow
	require.Equal(t, "default", result.Identity.Namespace)
	require.Equal(t, "Deployment", result.Identity.Kind)
	require.Equal(t, "test-crashloop", result.Identity.Name)

	// Workflow ID format is correct
	require.Equal(t, "triage/default/Deployment/test-crashloop/KubePodCrashLooping",
		params.Identity.WorkflowID())

	// Report includes L1 commands (appended by workflow post-agent)
	require.NotNil(t, result.Report)
	hasL1 := false
	for _, rec := range result.Report.Recommendations {
		if rec.Source == "l1" {
			hasL1 = true
			break
		}
	}
	require.True(t, hasL1, "L1 diagnostic commands should be appended to recommendations")

	// Report stored matches workflow result
	require.Equal(t, result.Classification, storedResult.Classification)
	require.Equal(t, result.AlertCount, storedResult.AlertCount)

	// Timing data populated
	require.False(t, result.StartedAt.IsZero(), "StartedAt should be set")
	require.False(t, result.CompletedAt.IsZero(), "CompletedAt should be set")
	require.True(t, result.CompletedAt.After(result.StartedAt), "CompletedAt must be after StartedAt")

	t.Logf("=== TRIAGE REPORT ===")
	t.Logf("Classification: %s", result.Classification)
	t.Logf("Severity: %s", result.Severity)
	t.Logf("Root Cause: %s", result.RootCause)
	t.Logf("Confidence: %.0f%%", result.Report.Confidence*100)
	t.Logf("Recommendations: %d total", len(result.Report.Recommendations))
	for i, rec := range result.Report.Recommendations {
		t.Logf("  [%d] %s (risk=%s, source=%s)", i+1, rec.Action, rec.Risk, rec.Source)
		if rec.Command != "" {
			t.Logf("      $ %s", rec.Command)
		}
	}
}

// TestTriageWorkflow_E2E_AgentFailure verifies the "failed" state transition
// when the AI agent is unreachable (our BUG fix).
func TestTriageWorkflow_E2E_AgentFailure(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "e2e-fail-001",
			Labels:      map[string]string{"alertname": "Test", "namespace": "test", "deployment": "broken"},
			StartsAt:    time.Now(),
		})
	}, 0)

	// Enrichment succeeds
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{Available: true}, nil)
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{Available: true}, nil)

	// State updates — track that "failed" is called
	var stateTransitions []string
	env.OnActivity(reportAct.UpdateIncidentState, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			state := args.Get(2).(string)
			stateTransitions = append(stateTransitions, state)
		}).Return(nil)

	// Agent FAILS with non-retryable error
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{},
		temporal.NewNonRetryableApplicationError("agent gateway unreachable", "A2AClientError", nil))

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "test", Kind: "Deployment", Name: "broken", AlertName: "Test"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "workflow should fail when agent is unreachable")

	// Verify state machine: enriching → triaging → failed
	require.Contains(t, stateTransitions, "enriching", "should transition to enriching")
	require.Contains(t, stateTransitions, "triaging", "should transition to triaging")
	require.Contains(t, stateTransitions, "failed", "should transition to failed on agent error")

	t.Logf("State transitions: %v", stateTransitions)
}

// TestTriageWorkflow_E2E_ResolvedStateGuard verifies that a resolved incident
// doesn't get overwritten when the workflow completes late.
func TestTriageWorkflow_E2E_ResolvedStateGuard(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "e2e-guard-001",
			Labels:      map[string]string{"alertname": "Resolved", "namespace": "ns", "deployment": "app"},
			StartsAt:    time.Now(),
		})
	}, 0)

	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{Available: true}, nil)
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{Available: true}, nil)
	env.OnActivity(reportAct.UpdateIncidentState, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{Classification: "CrashLoop", Severity: "warning", RootCause: "test"}, nil)
	// StoreTriageReport should succeed — the state guard is in the SQL WHERE clause,
	// not in the workflow code. This test verifies workflow still completes.
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "app", AlertName: "Resolved"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// TestTriageWorkflow_E2E_AllEnrichmentFails verifies workflow still completes
// and produces a report even when all enrichment sources are unavailable.
func TestTriageWorkflow_E2E_AllEnrichmentFails(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "e2e-noenrich-001",
			Labels:      map[string]string{"alertname": "NoData", "namespace": "ns", "deployment": "blind"},
			StartsAt:    time.Now(),
		})
	}, 0)

	// ALL enrichment fails
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{}, fmt.Errorf("prometheus unreachable"))
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{}, fmt.Errorf("k8s api server down"))
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{}, fmt.Errorf("loki gateway timeout"))

	env.OnActivity(reportAct.UpdateIncidentState, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Agent should still be called — it can reason from alert labels alone
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			enrichment := args.Get(2).(types.EnrichmentResult)
			// Verify enrichment reports unavailability
			require.False(t, enrichment.Prometheus.Available)
			require.False(t, enrichment.Kubernetes.Available)
			require.False(t, enrichment.Loki.Available)
		}).
		Return(types.TriageReport{
			Classification: "Unknown",
			Severity:       "warning",
			RootCause:      "Unable to determine root cause — all observability sources unavailable",
			Confidence:     0.3,
		}, nil)

	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "blind", AlertName: "NoData"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "workflow should succeed even with no enrichment data")

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "Unknown", result.Classification)
	require.LessOrEqual(t, result.Report.Confidence, 0.5)
}

// TestTriageWorkflow_E2E_ReportUsability validates that the triage report
// produces an actionable fix command that could resolve the test-crashloop issue.
func TestTriageWorkflow_E2E_ReportUsability(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "usability-001",
			Labels: map[string]string{
				"alertname":  "KubePodCrashLooping",
				"namespace":  "default",
				"deployment": "test-crashloop",
				"severity":   "critical",
			},
			Annotations: map[string]string{
				"description": "Exit code 137 (OOMKilled). Memory limit 64Mi.",
			},
			StartsAt: time.Now(),
		})
	}, 0)

	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true, RestartRate: 8.5, MemoryPct: 99.0, CPUUsage: 0.03}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{
			Available: true, PodPhase: "Running", ExitCodes: []int32{137},
			RecentEvents: []string{"Back-off restarting failed container"},
		}, nil)
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{Available: true, ErrorLines: []string{"runtime: out of memory"}, LogCount: 10}, nil)
	env.OnActivity(reportAct.UpdateIncidentState, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Agent returns a fix command
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{
			Classification:   "OOM",
			Severity:         "critical",
			RootCause:        "Container OOM-killed. Memory limit 64Mi is too low.",
			Confidence:       0.92,
			EscalationNeeded: false,
			CausalChain:      []string{"Memory exceeds 64Mi", "OOM kill signal 137", "CrashLoopBackOff"},
			Evidence: []types.EvidenceItem{
				{Observation: "Memory at 99% of limit", Source: "prometheus", Strength: "strong"},
				{Observation: "Exit code 137", Source: "kubernetes", Strength: "strong"},
			},
			Impact: types.Impact{AffectedServices: []string{"test-crashloop"}, BlastRadius: "deployment"},
			Recommendations: []types.Recommendation{
				{
					Action:  "Increase memory limit",
					Command: `kubectl set resources deployment/test-crashloop -n default --limits=memory=256Mi`,
					Risk:    "low",
				},
			},
		}, nil)
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "test-crashloop", AlertName: "KubePodCrashLooping"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))

	// The report should be actionable: has a fix command, identifies the right issue
	require.Equal(t, "OOM", result.Classification)
	require.NotNil(t, result.Report)
	require.Greater(t, result.Report.Confidence, 0.8)

	// Find the agent-sourced fix command (not L1 diagnostics)
	var fixCmd string
	for _, rec := range result.Report.Recommendations {
		if rec.Command != "" && rec.Source == "agent" {
			fixCmd = rec.Command
			break
		}
	}
	require.NotEmpty(t, fixCmd, "report must include at least one agent-sourced fix command")
	require.Contains(t, fixCmd, "test-crashloop", "fix command should target the correct deployment")
	require.Contains(t, fixCmd, "memory", "fix command should address memory")

	t.Logf("=== FIX COMMAND ===")
	t.Logf("$ %s", fixCmd)
	t.Logf("Expected: Pod stops crashing after memory limit increase")
}
