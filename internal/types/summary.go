package types

import "fmt"

// validBlastRadii constrains blast_radius to known values for sort correctness.
var validBlastRadii = map[string]bool{
	"pod": true, "deployment": true, "namespace": true, "cluster": true,
}

// ValidateBlastRadius returns a known blast radius value, defaulting to "pod".
func ValidateBlastRadius(raw string) string {
	if validBlastRadii[raw] {
		return raw
	}
	return "pod"
}

// BuildSummary generates a deterministic one-line summary from report fields.
// Format: "{Kind}/{Name} in {Namespace}: {Classification} — {root_cause_truncated}"
// This is computed in Go, never by the LLM.
func BuildSummary(identity IncidentIdentity, report *TriageReport) string {
	rc := report.RootCause
	runes := []rune(rc)
	if len(runes) > 120 {
		rc = string(runes[:117]) + "..."
	}
	return fmt.Sprintf("%s/%s in %s: %s — %s",
		identity.Kind, identity.Name, identity.Namespace,
		report.Classification, rc,
	)
}

// NormalizeSummary unconditionally sets the report summary to a computed value.
// The LLM never controls this field — always overwrite.
func NormalizeSummary(identity IncidentIdentity, report *TriageReport) {
	report.Summary = BuildSummary(identity, report)
}

// NormalizeRecommendations scrubs LLM-controlled fields, tags agent-generated
// recommendations with Source, and sorts L1 commands (deterministic) first.
func NormalizeRecommendations(report *TriageReport) {
	for i := range report.Recommendations {
		if report.Recommendations[i].Source == "" {
			report.Recommendations[i].Source = "agent"
		}
		// Scrub Expected on agent recs — only L1 commands set this in Go.
		if report.Recommendations[i].Source == "agent" {
			report.Recommendations[i].Expected = ""
		}
	}
	// Stable partition: l1 commands before agent recommendations.
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
