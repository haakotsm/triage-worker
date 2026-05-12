package types

import "fmt"

// BuildSummary generates a deterministic one-line summary from report fields.
// Format: "{Kind}/{Name} in {Namespace}: {Classification} — {root_cause_truncated}"
// This is computed in Go, never by the LLM.
func BuildSummary(identity IncidentIdentity, report *TriageReport) string {
	rc := report.RootCause
	if len(rc) > 120 {
		rc = rc[:117] + "..."
	}
	return fmt.Sprintf("%s/%s in %s: %s — %s",
		identity.Kind, identity.Name, identity.Namespace,
		report.Classification, rc,
	)
}

// NormalizeSummary ensures the report has a summary.
// Prefers agent-generated if present and short; falls back to computed.
func NormalizeSummary(identity IncidentIdentity, report *TriageReport) {
	if report.Summary == "" || len(report.Summary) > 200 {
		report.Summary = BuildSummary(identity, report)
	}
}

// NormalizeRecommendations tags agent-generated recommendations with Source
// and sorts all recommendations so L1 commands (deterministic) come first.
func NormalizeRecommendations(report *TriageReport) {
	for i := range report.Recommendations {
		if report.Recommendations[i].Source == "" {
			report.Recommendations[i].Source = "agent"
		}
	}
	// Stable sort: l1 commands before agent recommendations.
	// L1 commands are appended after agent recs, so we partition in-place.
	l1 := make([]Recommendation, 0, len(report.Recommendations))
	agent := make([]Recommendation, 0, len(report.Recommendations))
	for _, r := range report.Recommendations {
		if r.Source == "l1" {
			l1 = append(l1, r)
		} else {
			agent = append(agent, r)
		}
	}
	report.Recommendations = append(l1, agent...)
}
