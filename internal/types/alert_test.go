package types

import (
	"testing"
	"time"
)

func TestDeriveIdentity_Deployment(t *testing.T) {
	labels := map[string]string{
		"namespace":  "default",
		"deployment": "my-app",
		"pod":        "my-app-abc123",
		"alertname":  "KubePodCrashLooping",
	}

	id := DeriveIdentity(labels)

	if id.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", id.Namespace, "default")
	}
	if id.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", id.Kind, "Deployment")
	}
	if id.Name != "my-app" {
		t.Errorf("Name = %q, want %q", id.Name, "my-app")
	}
	if id.AlertName != "KubePodCrashLooping" {
		t.Errorf("AlertName = %q, want %q", id.AlertName, "KubePodCrashLooping")
	}
}

func TestDeriveIdentity_StatefulSet(t *testing.T) {
	labels := map[string]string{
		"namespace":   "db",
		"statefulset": "postgresql",
		"pod":         "postgresql-0",
		"alertname":   "KubePodNotReady",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "StatefulSet" {
		t.Errorf("Kind = %q, want %q", id.Kind, "StatefulSet")
	}
	if id.Name != "postgresql" {
		t.Errorf("Name = %q, want %q", id.Name, "postgresql")
	}
}

func TestDeriveIdentity_DaemonSet(t *testing.T) {
	labels := map[string]string{
		"namespace": "kube-system",
		"daemonset": "cilium",
		"pod":       "cilium-abc",
		"alertname": "CiliumEndpointNotReady",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "DaemonSet" {
		t.Errorf("Kind = %q, want %q", id.Kind, "DaemonSet")
	}
	if id.Name != "cilium" {
		t.Errorf("Name = %q, want %q", id.Name, "cilium")
	}
}

func TestDeriveIdentity_Job(t *testing.T) {
	labels := map[string]string{
		"namespace": "batch",
		"job_name":  "data-import",
		"alertname": "KubeJobFailed",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "Job" {
		t.Errorf("Kind = %q, want %q", id.Kind, "Job")
	}
	if id.Name != "data-import" {
		t.Errorf("Name = %q, want %q", id.Name, "data-import")
	}
}

func TestDeriveIdentity_PodOnly(t *testing.T) {
	labels := map[string]string{
		"namespace": "default",
		"pod":       "standalone-pod",
		"alertname": "KubePodCrashLooping",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "Pod" {
		t.Errorf("Kind = %q, want %q", id.Kind, "Pod")
	}
	if id.Name != "standalone-pod" {
		t.Errorf("Name = %q, want %q", id.Name, "standalone-pod")
	}
}

func TestDeriveIdentity_NamespaceFallback(t *testing.T) {
	labels := map[string]string{
		"namespace": "monitoring",
		"alertname": "PrometheusTargetDown",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "Namespace" {
		t.Errorf("Kind = %q, want %q", id.Kind, "Namespace")
	}
	if id.Name != "monitoring" {
		t.Errorf("Name = %q, want %q", id.Name, "monitoring")
	}
}

func TestDeriveIdentity_NoNamespace(t *testing.T) {
	labels := map[string]string{
		"alertname": "Watchdog",
	}

	id := DeriveIdentity(labels)
	if id.Namespace != "cluster" {
		t.Errorf("Namespace = %q, want %q", id.Namespace, "cluster")
	}
}

func TestDeriveIdentity_PriorityOrder(t *testing.T) {
	// When both deployment and statefulset are present, deployment wins
	labels := map[string]string{
		"namespace":   "default",
		"deployment":  "my-deploy",
		"statefulset": "my-sts",
		"pod":         "my-pod",
		"alertname":   "TestAlert",
	}

	id := DeriveIdentity(labels)
	if id.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q (deployment should take priority)", id.Kind, "Deployment")
	}
}

func TestWorkflowID(t *testing.T) {
	id := IncidentIdentity{
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "my-app",
		AlertName: "KubePodCrashLooping",
	}

	got := id.WorkflowID()
	want := "triage/default/Deployment/my-app/KubePodCrashLooping"
	if got != want {
		t.Errorf("WorkflowID() = %q, want %q", got, want)
	}
}

func TestFiringAlerts(t *testing.T) {
	now := time.Now()
	group := AlertGroup{
		Alerts: []Alert{
			{Status: "firing", Fingerprint: "a", StartsAt: now},
			{Status: "resolved", Fingerprint: "b", StartsAt: now},
			{Status: "firing", Fingerprint: "c", StartsAt: now},
		},
	}

	firing := group.FiringAlerts()
	if len(firing) != 2 {
		t.Fatalf("FiringAlerts() returned %d alerts, want 2", len(firing))
	}
	if firing[0].Fingerprint != "a" {
		t.Errorf("firing[0].Fingerprint = %q, want %q", firing[0].Fingerprint, "a")
	}
	if firing[1].Fingerprint != "c" {
		t.Errorf("firing[1].Fingerprint = %q, want %q", firing[1].Fingerprint, "c")
	}
}

func TestFiringAlerts_AllResolved(t *testing.T) {
	group := AlertGroup{
		Alerts: []Alert{
			{Status: "resolved", Fingerprint: "a"},
			{Status: "resolved", Fingerprint: "b"},
		},
	}

	firing := group.FiringAlerts()
	if len(firing) != 0 {
		t.Errorf("FiringAlerts() returned %d alerts, want 0", len(firing))
	}
}

func TestFiringAlerts_Empty(t *testing.T) {
	group := AlertGroup{}

	firing := group.FiringAlerts()
	if len(firing) != 0 {
		t.Errorf("FiringAlerts() returned %d alerts, want 0", len(firing))
	}
}

func TestSanitizeK8sName_ValidNames(t *testing.T) {
	valid := []string{"my-app", "postgresql", "cilium", "data-import", "app-0", "a"}
	for _, name := range valid {
		got := SanitizeK8sName(name)
		if got != name {
			t.Errorf("SanitizeK8sName(%q) = %q, want unchanged", name, got)
		}
	}
}

func TestSanitizeK8sName_Sanitizes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"MyApp", "myapp"},
		{"my_app", "my-app"},
		{`my";DROP TABLE`, "my--drop-table"},
		{"", "unknown"},
		{"---", "unknown"},
		{"UPPER-CASE", "upper-case"},
	}
	for _, tc := range tests {
		got := SanitizeK8sName(tc.input)
		if got != tc.want {
			t.Errorf("SanitizeK8sName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSanitizeLabelValue_ValidValues(t *testing.T) {
	valid := []string{"KubePodCrashLooping", "HighErrorRate", "my-metric.total", "alert_name"}
	for _, val := range valid {
		got := SanitizeLabelValue(val)
		if got != val {
			t.Errorf("SanitizeLabelValue(%q) = %q, want unchanged", val, got)
		}
	}
}

func TestSanitizeLabelValue_Sanitizes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`alert"} or 1=1 {`, "alertor11"},
		{"", "unknown"},
		{`$(curl evil.com)`, "curlevil.com"},
	}
	for _, tc := range tests {
		got := SanitizeLabelValue(tc.input)
		if got != tc.want {
			t.Errorf("SanitizeLabelValue(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDeriveIdentity_InjectionPrevented(t *testing.T) {
	labels := map[string]string{
		"namespace":  `default"} or vector(1) {ns="`,
		"deployment": `app"; kubectl delete ns --all`,
		"alertname":  `FakeAlert"} / ignoring() group_left() up{`,
	}

	id := DeriveIdentity(labels)

	// All values should be sanitized — no query/shell metacharacters
	if id.Namespace == labels["namespace"] {
		t.Error("namespace should have been sanitized")
	}
	if id.Name == labels["deployment"] {
		t.Error("name should have been sanitized")
	}
	if id.AlertName == labels["alertname"] {
		t.Error("alertname should have been sanitized")
	}
}
