package types

// TriageReport is the structured diagnosis from the AI agent.
// Must match the JSON schema in the agent's system prompt.
type TriageReport struct {
	Classification   string           `json:"classification"`
	Severity         string           `json:"severity"`
	RootCause        string           `json:"root_cause"`
	CausalChain      []string         `json:"causal_chain"`
	Evidence         []EvidenceItem   `json:"evidence"`
	Impact           Impact           `json:"impact"`
	Recommendations  []Recommendation `json:"recommendations"`
	Confidence       float64          `json:"confidence"`
	EscalationNeeded bool             `json:"escalation_needed"`
}

// EvidenceItem represents one piece of evidence supporting the diagnosis.
type EvidenceItem struct {
	Observation string `json:"observation"`
	Source      string `json:"source"`
	Strength    string `json:"strength"` // strong, moderate, weak
}

// Impact describes the blast radius of the incident.
type Impact struct {
	AffectedServices []string `json:"affected_services"`
	BlastRadius      string   `json:"blast_radius"` // pod, deployment, namespace, cluster
}

// Recommendation is a specific remediation action.
type Recommendation struct {
	Action  string `json:"action"`
	Command string `json:"command,omitempty"`
	Risk    string `json:"risk"` // none, low, medium
}

// Valid classifications (matches agent system prompt taxonomy).
var ValidClassifications = map[string]bool{
	"CrashLoop":          true,
	"OOM":                true,
	"Network":            true,
	"ImagePull":          true,
	"ResourceExhaustion": true,
	"Config":             true,
	"Scheduling":         true,
	"Unknown":            true,
}

// Valid severities.
var ValidSeverities = map[string]bool{
	"critical": true,
	"warning":  true,
	"info":     true,
}
