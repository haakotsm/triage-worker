package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/haakotsm/triage-worker/internal/activity"
	"github.com/haakotsm/triage-worker/internal/types"
)

func TestTriageWorkflow_SingleAlert(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	// Register activities
	var enrichAct *activity.Activities
	var agentAct *activity.AgentActivity
	var reportAct *activity.ReportActivity
	var k8sAct *activity.K8sActivity

	env.RegisterActivity(enrichAct)
	env.RegisterActivity(agentAct)
	env.RegisterActivity(reportAct)
	env.RegisterActivity(k8sAct)

	// Send alert signal before workflow starts (simulates SignalWithStart)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "test-fp-001",
			Labels: map[string]string{
				"alertname":  "KubePodCrashLooping",
				"namespace":  "default",
				"deployment": "catalog-api",
				"severity":   "critical",
			},
			Annotations: map[string]string{
				"description": "Pod is crash looping",
			},
			StartsAt: time.Now(),
		})
	}, 0)

	// Mock enrichment activities
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{
			Available:   true,
			RestartRate: 5.0,
			MemoryPct:   45.0,
			CPUUsage:    0.2,
		}, nil)

	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{
			Available:    true,
			PodPhase:     "Running",
			ExitCodes:    []int32{1},
			RecentEvents: []string{"BackOff restarting failed container"},
		}, nil)

	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{
			Available:  true,
			ErrorLines: []string{"Error: database connection failed"},
			LogCount:   1,
		}, nil)

	// Mock agent invocation
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{
			Classification:   "CrashLoop",
			Severity:         "critical",
			RootCause:        "Application crashing due to database connection failure",
			CausalChain:      []string{"PostgreSQL unreachable", "Connection pool exhausted", "Pod exit code 1"},
			Confidence:       0.85,
			EscalationNeeded: false,
			Evidence: []types.EvidenceItem{
				{Observation: "5 restarts in 5 minutes", Source: "prometheus", Strength: "strong"},
			},
			Impact: types.Impact{
				AffectedServices: []string{"catalog-api"},
				BlastRadius:      "deployment",
			},
			Recommendations: []types.Recommendation{
				{Action: "Check PostgreSQL connectivity", Command: "kubectl exec -n default catalog-api -- pg_isready", Risk: "none"},
			},
		}, nil)

	// Mock report storage
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	// Execute workflow
	params := types.TriageParams{
		Identity: types.IncidentIdentity{
			Namespace: "default",
			Kind:      "Deployment",
			Name:      "catalog-api",
			AlertName: "KubePodCrashLooping",
		},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))

	// Verify result
	require.Equal(t, "CrashLoop", result.Classification)
	require.Equal(t, "critical", result.Severity)
	require.Equal(t, 1, result.AlertCount)
	require.Equal(t, "default", result.Identity.Namespace)
	require.Equal(t, "catalog-api", result.Identity.Name)
	require.NotEmpty(t, result.RootCause)
}

func TestTriageWorkflow_MultipleAlerts(t *testing.T) {
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

	// Send first alert immediately
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "fp-1",
			Labels:      map[string]string{"alertname": "Alert1", "namespace": "default", "deployment": "app"},
			StartsAt:    time.Now(),
		})
	}, 0)

	// Send second alert 10s later (within debounce window)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "fp-2",
			Labels:      map[string]string{"alertname": "Alert2", "namespace": "default", "deployment": "app"},
			StartsAt:    time.Now(),
		})
	}, 10*time.Second)

	// Mock activities
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{Available: true}, nil)
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{Available: true}, nil)
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{
			Classification: "CrashLoop",
			Severity:       "critical",
			RootCause:      "Multi-alert root cause",
		}, nil)
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{
			Namespace: "default",
			Kind:      "Deployment",
			Name:      "app",
			AlertName: "Alert1",
		},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))

	// Should have collected both alerts
	require.Equal(t, 2, result.AlertCount)
}

func TestTriageWorkflow_DeduplicatesAlerts(t *testing.T) {
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

	// Send same fingerprint twice
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "same-fp",
			Labels:      map[string]string{"alertname": "Dup", "namespace": "ns"},
			StartsAt:    time.Now(),
		})
	}, 0)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(AlertSignalName, types.Alert{
			Status:      "firing",
			Fingerprint: "same-fp", // Duplicate!
			Labels:      map[string]string{"alertname": "Dup", "namespace": "ns"},
			StartsAt:    time.Now(),
		})
	}, 5*time.Second)

	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{Available: true}, nil)
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{Available: true}, nil)
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{Classification: "CrashLoop", Severity: "warning", RootCause: "test"}, nil)
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "ns", Kind: "Namespace", Name: "ns", AlertName: "Dup"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.TriageResult
	require.NoError(t, env.GetWorkflowResult(&result))

	// Should deduplicate to 1 alert
	require.Equal(t, 1, result.AlertCount)
}

func TestTriageWorkflow_EnrichmentPartialFailure(t *testing.T) {
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
			Fingerprint: "fp-partial",
			Labels:      map[string]string{"alertname": "Test", "namespace": "ns"},
			StartsAt:    time.Now(),
		})
	}, 0)

	// Prometheus succeeds, K8s fails, Loki fails
	env.OnActivity(enrichAct.QueryPrometheus, mock.Anything, mock.Anything, mock.Anything).Return(
		types.PrometheusResult{Available: true, RestartRate: 3.0}, nil)
	env.OnActivity(k8sAct.QueryKubernetesAPI, mock.Anything, mock.Anything, mock.Anything).Return(
		types.KubernetesResult{}, fmt.Errorf("k8s api timeout"))
	env.OnActivity(enrichAct.QueryLoki, mock.Anything, mock.Anything, mock.Anything).Return(
		types.LokiResult{}, fmt.Errorf("loki connection refused"))

	// Agent should still be called with partial enrichment
	env.OnActivity(agentAct.InvokeTriageAgent, mock.Anything, mock.Anything, mock.Anything).Return(
		types.TriageReport{Classification: "Unknown", Severity: "warning", RootCause: "insufficient data"}, nil)
	env.OnActivity(reportAct.StoreTriageReport, mock.Anything, mock.Anything).Return(nil)

	params := types.TriageParams{
		Identity: types.IncidentIdentity{Namespace: "ns", Kind: "Namespace", Name: "ns", AlertName: "Test"},
	}
	env.ExecuteWorkflow(TriageWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError()) // Workflow should succeed despite partial enrichment failure
}
