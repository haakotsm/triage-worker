package types

import (
	"strings"
	"testing"
)

func alertWithPod(pod string) Alert {
	return Alert{Status: "firing", Labels: map[string]string{"pod": pod}}
}

func findCmd(cmds []Recommendation, substr string) (Recommendation, bool) {
	for _, c := range cmds {
		if strings.Contains(c.Command, substr) {
			return c, true
		}
	}
	return Recommendation{}, false
}

func TestVerificationCommands_AllReadOnlyTaggedAndExplained(t *testing.T) {
	id := IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "app"}
	for _, c := range []string{"CrashLoop", "OOM", "Network", "ImagePull", "ResourceExhaustion", "Config", "Scheduling", "Unknown"} {
		cmds := VerificationCommands(TriageReport{Classification: c}, id, EnrichmentResult{}, nil)
		if len(cmds) == 0 {
			t.Fatalf("classification %q produced no commands", c)
		}
		for _, cmd := range cmds {
			if cmd.Risk != "none" && cmd.Risk != "low" {
				t.Errorf("%s/%q: risk=%q, want none/low (read-only)", c, cmd.Action, cmd.Risk)
			}
			if cmd.Source != "l1" {
				t.Errorf("%s/%q: source=%q, want l1", c, cmd.Action, cmd.Source)
			}
			if cmd.Expected == "" {
				t.Errorf("%s/%q: Expected is empty (every verification step should say what it confirms)", c, cmd.Action)
			}
		}
	}
}

func TestVerificationCommands_TargetsRealPods(t *testing.T) {
	cmds := VerificationCommands(
		TriageReport{Classification: "CrashLoop"},
		IncidentIdentity{Namespace: "prod", Kind: "Deployment", Name: "api"},
		EnrichmentResult{},
		[]Alert{alertWithPod("api-7c9f-abcde"), alertWithPod("api-7c9f-fghij")},
	)

	status, ok := findCmd(cmds, "kubectl get pods")
	if !ok {
		t.Fatal("missing pod status command")
	}
	if !strings.Contains(status.Command, "api-7c9f-abcde") || !strings.Contains(status.Command, "api-7c9f-fghij") {
		t.Errorf("status command should target both real pods, got %q", status.Command)
	}
	if strings.Contains(status.Command, "-l app=") {
		t.Errorf("should not fall back to a selector when pods are known: %q", status.Command)
	}

	logs, ok := findCmd(cmds, "kubectl logs")
	if !ok {
		t.Fatal("CrashLoop should include a logs command")
	}
	if !strings.Contains(logs.Command, "api-7c9f-abcde") {
		t.Errorf("logs should target the first real pod (logs takes one), got %q", logs.Command)
	}
	if !strings.Contains(logs.Command, "--previous") {
		t.Errorf("CrashLoop logs should use --previous, got %q", logs.Command)
	}
}

func TestVerificationCommands_FallsBackToSelector(t *testing.T) {
	cmds := VerificationCommands(
		TriageReport{Classification: "OOM"},
		IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "worker"},
		EnrichmentResult{},
		nil, // no alerts → no real pod names
	)
	status, ok := findCmd(cmds, "kubectl get pods")
	if !ok {
		t.Fatal("missing pod status command")
	}
	if !strings.Contains(status.Command, "-l app=worker") {
		t.Errorf("with no real pods, should fall back to label selector, got %q", status.Command)
	}
}

func TestVerificationCommands_ReferencesObservedFacts(t *testing.T) {
	cmds := VerificationCommands(
		TriageReport{Classification: "OOM"},
		IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "cache"},
		EnrichmentResult{
			Kubernetes: KubernetesResult{Available: true, PodPhase: "Running", ExitCodes: []int32{137}},
			Prometheus: PrometheusResult{Available: true, MemoryPct: 92, RestartRate: 3.5},
		},
		[]Alert{alertWithPod("cache-0")},
	)

	exit, ok := findCmd(cmds, "lastState.terminated.exitCode")
	if !ok {
		t.Fatal("expected an exit-code verification command")
	}
	if !strings.Contains(exit.Expected, "137") {
		t.Errorf("exit-code Expected should cite the observed code 137, got %q", exit.Expected)
	}

	usage, ok := findCmd(cmds, "top pods")
	if !ok {
		t.Fatal("expected a memory-usage command for OOM")
	}
	if !strings.Contains(usage.Expected, "92%") {
		t.Errorf("memory Expected should cite the observed 92%%, got %q", usage.Expected)
	}

	restart, ok := findCmd(cmds, "restartCount")
	if !ok {
		t.Fatal("expected a restart-count command when restart rate observed")
	}
	if !strings.Contains(restart.Expected, "3.5") {
		t.Errorf("restart Expected should cite the observed rate 3.5, got %q", restart.Expected)
	}

	status, _ := findCmd(cmds, "kubectl get pods")
	if !strings.Contains(status.Expected, "Running") {
		t.Errorf("status Expected should cite the observed pod phase, got %q", status.Expected)
	}
}

func TestVerificationCommands_NetworkCorroboration(t *testing.T) {
	cmds := VerificationCommands(
		TriageReport{Classification: "Network"},
		IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "gw"},
		EnrichmentResult{},
		nil,
	)
	if _, ok := findCmd(cmds, "networkpolicy"); !ok {
		t.Error("Network classification should include a NetworkPolicy check")
	}
	if _, ok := findCmd(cmds, "endpoints"); !ok {
		t.Error("Network classification should include an endpoints check")
	}
}

func TestVerificationCommands_NoLogsWhenNoTarget(t *testing.T) {
	// Pod/Namespace kind with no alert pods → no selector and no pod names, so
	// `kubectl logs` (which needs a target) must NOT be emitted with a blank one.
	cmds := VerificationCommands(
		TriageReport{Classification: "CrashLoop"},
		IncidentIdentity{Namespace: "default", Kind: "Namespace", Name: "default"},
		EnrichmentResult{},
		nil,
	)
	if c, ok := findCmd(cmds, "kubectl logs"); ok {
		t.Errorf("should not emit a logs command with no pod target, got %q", c.Command)
	}
}

func TestVerificationCommands_SanitizesPodLabel(t *testing.T) {
	// A hostile/garbled alert pod label must not become a runnable shell command
	// when copy-pasted.
	cmds := VerificationCommands(
		TriageReport{Classification: "CrashLoop"},
		IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "api"},
		EnrichmentResult{},
		[]Alert{{Status: "firing", Labels: map[string]string{"pod": "api; rm -rf /"}}},
	)
	status, _ := findCmd(cmds, "kubectl get pods")
	if strings.Contains(status.Command, ";") || strings.Contains(status.Command, "rm -rf") {
		t.Errorf("pod label was not sanitized out of the command: %q", status.Command)
	}
}

func TestKindToResource(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"Deployment", "deployment"},
		{"StatefulSet", "statefulset"},
		{"DaemonSet", "daemonset"},
		{"Job", "job"},
		{"Pod", "pod"},
		{"Unknown", "pod"},
	}
	for _, tt := range tests {
		if got := kindToResource(tt.kind); got != tt.want {
			t.Errorf("kindToResource(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}
