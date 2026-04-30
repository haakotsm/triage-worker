package webhook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/haakotsm/triage-worker/internal/types"
)

// TestScenarioFixtures validates that all test scenario files are valid Alertmanager payloads
// and produce the expected workflow groupings.
func TestScenarioFixtures(t *testing.T) {
	scenarios := []struct {
		file             string
		expectedFiring   int
		expectedWorkflows int
		description      string
	}{
		{"s1_crashloop.json", 1, 1, "Single CrashLoopBackOff alert"},
		{"s2_oom.json", 1, 1, "Single OOMKilled alert"},
		{"s3_network_policy.json", 1, 1, "Single NetworkPolicy block alert"},
		{"s4_cascade.json", 4, 4, "Cascading failure: 4 alerts across 4 workloads (pg, catalog, members, gateway — each has unique owner+alertname)"},
		{"s5_imagepull.json", 1, 1, "Single ImagePullBackOff alert"},
		{"s6_resource_exhaustion.json", 2, 2, "Node-level: 2 alerts with different identity (node vs namespace)"},
	}

	for _, sc := range scenarios {
		t.Run(sc.file, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "scenarios", sc.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			var alertGroup types.AlertGroup
			if err := json.Unmarshal(data, &alertGroup); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}

			// Validate firing alert count
			firing := alertGroup.FiringAlerts()
			if len(firing) != sc.expectedFiring {
				t.Errorf("firing alerts = %d, want %d", len(firing), sc.expectedFiring)
			}

			// Validate workflow grouping
			alertsByWorkflow := make(map[string][]types.Alert)
			for _, alert := range firing {
				id := types.DeriveIdentity(alert.Labels)
				wfID := id.WorkflowID()
				alertsByWorkflow[wfID] = append(alertsByWorkflow[wfID], alert)
			}

			if len(alertsByWorkflow) != sc.expectedWorkflows {
				t.Errorf("distinct workflows = %d, want %d (%s)", len(alertsByWorkflow), sc.expectedWorkflows, sc.description)
				for wfID, alerts := range alertsByWorkflow {
					t.Logf("  workflow %s: %d alerts", wfID, len(alerts))
				}
			}
		})
	}
}

// TestS4CascadeWorkflowIDs verifies the specific workflow grouping for the cascade scenario.
// This is the CRITICAL test — multiple alerts should map to different workloads.
func TestS4CascadeWorkflowIDs(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "scenarios", "s4_cascade.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var alertGroup types.AlertGroup
	if err := json.Unmarshal(data, &alertGroup); err != nil {
		t.Fatal(err)
	}

	firing := alertGroup.FiringAlerts()
	alertsByWorkflow := make(map[string][]types.Alert)
	for _, alert := range firing {
		id := types.DeriveIdentity(alert.Labels)
		wfID := id.WorkflowID()
		alertsByWorkflow[wfID] = append(alertsByWorkflow[wfID], alert)
	}

	// Expected workflow IDs for the cascade scenario
	// Each workload gets its own workflow — correlation happens WITHIN a workflow via signal aggregation
	// In a real cascade, the AGENT identifies the root cause across reports
	expectedIDs := map[string]bool{
		"triage/default/StatefulSet/postgresql/PostgreSQLDown":       true,
		"triage/default/Deployment/catalog-api/KubePodCrashLooping": true,
		"triage/default/Deployment/members-api/KubePodCrashLooping": true,
		"triage/default/Deployment/api-gateway/HighErrorRate":       true,
	}

	for wfID := range alertsByWorkflow {
		if !expectedIDs[wfID] {
			t.Errorf("unexpected workflow ID: %s", wfID)
		}
	}

	for expected := range expectedIDs {
		if _, ok := alertsByWorkflow[expected]; !ok {
			t.Errorf("missing expected workflow ID: %s", expected)
		}
	}
}

// TestS6ResourceExhaustionWorkflowIDs verifies node-level alert grouping.
func TestS6ResourceExhaustionWorkflowIDs(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "scenarios", "s6_resource_exhaustion.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var alertGroup types.AlertGroup
	if err := json.Unmarshal(data, &alertGroup); err != nil {
		t.Fatal(err)
	}

	firing := alertGroup.FiringAlerts()
	alertsByWorkflow := make(map[string][]types.Alert)
	for _, alert := range firing {
		id := types.DeriveIdentity(alert.Labels)
		wfID := id.WorkflowID()
		alertsByWorkflow[wfID] = append(alertsByWorkflow[wfID], alert)
	}

	// Node-level alert has no namespace — falls to "cluster" namespace
	// Namespace-level alert has namespace "monitoring"
	if len(alertsByWorkflow) != 2 {
		t.Errorf("distinct workflows = %d, want 2", len(alertsByWorkflow))
		for wfID := range alertsByWorkflow {
			t.Logf("  %s", wfID)
		}
	}
}
