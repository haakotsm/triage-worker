package types

import "testing"

func TestBuildSummary(t *testing.T) {
	identity := IncidentIdentity{
		Kind:      "Deployment",
		Name:      "api-server",
		Namespace: "production",
		AlertName: "HighErrorRate",
	}
	report := &TriageReport{
		Classification: "CrashLoop",
		RootCause:      "OOM kill due to memory leak in connection pool",
	}

	got := BuildSummary(identity, report)
	want := "Deployment/api-server in production: CrashLoop — OOM kill due to memory leak in connection pool"
	if got != want {
		t.Errorf("BuildSummary() = %q, want %q", got, want)
	}
}

func TestBuildSummary_TruncatesLongRootCause(t *testing.T) {
	identity := IncidentIdentity{
		Kind:      "App",
		Name:      "linkerd-proxy",
		Namespace: "linkerd",
		AlertName: "LinkerdControlPlaneDown",
	}
	longRC := "This is a very long root cause that exceeds the 120 character limit and should be truncated with an ellipsis at the end of the string for readability purposes"
	report := &TriageReport{
		Classification: "Network",
		RootCause:      longRC,
	}

	got := BuildSummary(identity, report)
	if len(got) > 300 {
		t.Errorf("BuildSummary() too long: %d chars", len(got))
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("BuildSummary() should end with '...' for long root cause, got ending: %q", got[len(got)-10:])
	}
}

func TestNormalizeSummary_SetsSummaryWhenEmpty(t *testing.T) {
	identity := IncidentIdentity{
		Kind:      "Deployment",
		Name:      "web",
		Namespace: "default",
		AlertName: "PodDown",
	}
	report := &TriageReport{
		Classification: "CrashLoop",
		RootCause:      "missing config",
	}

	NormalizeSummary(identity, report)
	if report.Summary == "" {
		t.Error("NormalizeSummary() left Summary empty")
	}
	want := "Deployment/web in default: CrashLoop — missing config"
	if report.Summary != want {
		t.Errorf("NormalizeSummary() = %q, want %q", report.Summary, want)
	}
}

func TestNormalizeSummary_KeepsShortExistingSummary(t *testing.T) {
	identity := IncidentIdentity{Kind: "Pod", Name: "x", Namespace: "ns", AlertName: "A"}
	report := &TriageReport{
		Summary:        "My custom summary",
		Classification: "Config",
		RootCause:      "bad env var",
	}

	NormalizeSummary(identity, report)
	if report.Summary != "My custom summary" {
		t.Errorf("NormalizeSummary() changed existing summary to %q", report.Summary)
	}
}

func TestNormalizeRecommendations_TagsAndSorts(t *testing.T) {
	report := &TriageReport{
		Recommendations: []Recommendation{
			{Action: "Agent rec 1", Risk: "low"},
			{Action: "Agent rec 2", Risk: "medium"},
			{Action: "L1 cmd 1", Risk: "none", Source: "l1", Expected: "Check pods"},
			{Action: "L1 cmd 2", Risk: "none", Source: "l1", Expected: "Check logs"},
		},
	}

	NormalizeRecommendations(report)

	// Verify agent recs are tagged
	for _, r := range report.Recommendations {
		if r.Source == "" {
			t.Errorf("Recommendation %q has empty Source", r.Action)
		}
	}

	// Verify L1 commands come first
	if report.Recommendations[0].Source != "l1" {
		t.Errorf("First recommendation should be l1, got %q", report.Recommendations[0].Source)
	}
	if report.Recommendations[1].Source != "l1" {
		t.Errorf("Second recommendation should be l1, got %q", report.Recommendations[1].Source)
	}
	if report.Recommendations[2].Source != "agent" {
		t.Errorf("Third recommendation should be agent, got %q", report.Recommendations[2].Source)
	}
	if report.Recommendations[3].Source != "agent" {
		t.Errorf("Fourth recommendation should be agent, got %q", report.Recommendations[3].Source)
	}
}

func TestNormalizeRecommendations_AllAgent(t *testing.T) {
	report := &TriageReport{
		Recommendations: []Recommendation{
			{Action: "Fix it", Risk: "low"},
		},
	}

	NormalizeRecommendations(report)
	if report.Recommendations[0].Source != "agent" {
		t.Errorf("Expected 'agent' source, got %q", report.Recommendations[0].Source)
	}
}

func TestNormalizeRecommendations_AllL1(t *testing.T) {
	report := &TriageReport{
		Recommendations: []Recommendation{
			{Action: "Get pods", Risk: "none", Source: "l1"},
			{Action: "Check logs", Risk: "none", Source: "l1"},
		},
	}

	NormalizeRecommendations(report)
	if len(report.Recommendations) != 2 {
		t.Errorf("Expected 2 recommendations, got %d", len(report.Recommendations))
	}
	for _, r := range report.Recommendations {
		if r.Source != "l1" {
			t.Errorf("Expected all l1, got %q", r.Source)
		}
	}
}
