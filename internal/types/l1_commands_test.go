package types

import (
	"strings"
	"testing"
)

func TestL1Commands_CrashLoop(t *testing.T) {
	id := IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "my-app", AlertName: "KubePodCrashLooping"}
	cmds := L1Commands("CrashLoop", id)

	if len(cmds) < 4 {
		t.Fatalf("expected at least 4 commands for CrashLoop, got %d", len(cmds))
	}

	// All should have "none" risk (L1 = read-only)
	for _, cmd := range cmds {
		if cmd.Risk != "none" {
			t.Errorf("L1 command %q has risk=%q, want 'none'", cmd.Action, cmd.Risk)
		}
	}

	// Should include --previous flag for crash loop
	found := false
	for _, cmd := range cmds {
		if strings.Contains(cmd.Command, "--previous") {
			found = true
		}
	}
	if !found {
		t.Error("CrashLoop commands should include --previous log flag")
	}
}

func TestL1Commands_OOM(t *testing.T) {
	id := IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "app"}
	cmds := L1Commands("OOM", id)

	found := false
	for _, cmd := range cmds {
		if strings.Contains(cmd.Command, "resources.limits.memory") || strings.Contains(cmd.Command, "top pods") {
			found = true
		}
	}
	if !found {
		t.Error("OOM commands should include memory limit check")
	}
}

func TestL1Commands_Network(t *testing.T) {
	id := IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "app"}
	cmds := L1Commands("Network", id)

	found := false
	for _, cmd := range cmds {
		if strings.Contains(cmd.Command, "networkpolicy") {
			found = true
		}
	}
	if !found {
		t.Error("Network commands should check NetworkPolicies")
	}
}

func TestL1Commands_ImagePull(t *testing.T) {
	id := IncidentIdentity{Namespace: "staging", Kind: "Deployment", Name: "frontend"}
	cmds := L1Commands("ImagePull", id)

	found := false
	for _, cmd := range cmds {
		if strings.Contains(cmd.Command, "image") {
			found = true
		}
	}
	if !found {
		t.Error("ImagePull commands should verify image reference")
	}
}

func TestL1Commands_Unknown(t *testing.T) {
	id := IncidentIdentity{Namespace: "default", Kind: "Pod", Name: "orphan"}
	cmds := L1Commands("Unknown", id)

	// Should still return base commands
	if len(cmds) < 3 {
		t.Fatalf("expected at least 3 base commands, got %d", len(cmds))
	}
}

func TestL1Commands_AllRiskNone(t *testing.T) {
	classifications := []string{"CrashLoop", "OOM", "Network", "ImagePull", "ResourceExhaustion", "Config", "Scheduling", "Unknown"}
	id := IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "app"}

	for _, c := range classifications {
		cmds := L1Commands(c, id)
		for _, cmd := range cmds {
			if cmd.Risk != "none" {
				t.Errorf("classification=%q action=%q: risk=%q, want 'none'", c, cmd.Action, cmd.Risk)
			}
		}
	}
}

func TestL1Commands_AllHaveSourceAndExpected(t *testing.T) {
	classifications := []string{"CrashLoop", "OOM", "Network", "ImagePull", "ResourceExhaustion", "Config", "Scheduling", "Unknown"}
	id := IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "app"}

	for _, c := range classifications {
		cmds := L1Commands(c, id)
		for _, cmd := range cmds {
			if cmd.Source != "l1" {
				t.Errorf("classification=%q action=%q: source=%q, want 'l1'", c, cmd.Action, cmd.Source)
			}
			if cmd.Expected == "" {
				t.Errorf("classification=%q action=%q: expected is empty", c, cmd.Action)
			}
		}
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
		got := kindToResource(tt.kind)
		if got != tt.want {
			t.Errorf("kindToResource(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}
