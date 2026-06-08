package types

// InvestigatorOutput is the structured result from one investigator agent.
// Each investigator runs independently with its own MCP tools (K8s, Prometheus, Loki)
// and reports findings in a uniform schema that the consolidator can cross-reference.
type InvestigatorOutput struct {
	AgentName   string    `json:"agent_name"`
	Findings    []Finding `json:"findings"`
	RawResponse string    `json:"-"`              // for debugging; excluded from serialization
	Error       string    `json:"error,omitempty"` // non-empty if the agent failed
	Available   bool      `json:"available"`       // false if agent was unreachable or timed out
	DurationMs  int64     `json:"duration_ms"`
}

// Finding represents a single observation from an investigator agent.
// The consolidator uses Category and Confidence to weight and cross-reference findings.
type Finding struct {
	Category    string  `json:"category"`    // resource_state, error_pattern, metric_anomaly, config_drift, network_issue
	Description string  `json:"description"` // human-readable explanation
	Evidence    string  `json:"evidence"`    // raw data supporting this finding (pod events, log lines, metric values)
	Severity    string  `json:"severity"`    // critical, warning, info
	Confidence  float64 `json:"confidence"`  // 0.0–1.0
}

// ValidFindingCategories constrains the category taxonomy for findings.
// Agents may produce arbitrary values; the activity normalizes them.
var ValidFindingCategories = map[string]bool{
	"resource_state":    true, // pod phase, container status, restart counts
	"error_pattern":     true, // OOMKilled, CrashLoopBackOff, ImagePullBackOff
	"metric_anomaly":    true, // CPU/memory spikes, request latency, error rate
	"config_drift":      true, // missing configmap, wrong resource limits, bad env var
	"network_issue":     true, // DNS failures, connection timeouts, service mesh errors
	"scheduling":        true, // insufficient resources, node affinity, taints
	"storage":           true, // PVC pending, volume mount failures
	"dependency":        true, // upstream service down, database unreachable
	"deployment_state":  true, // ArgoCD sync failures, OutOfSync, degraded apps
}

// InvestigatorConfig defines the agents to invoke during the investigation phase.
// This is configuration-driven to allow adding/removing investigators without code changes.
type InvestigatorConfig struct {
	Name        string // agent name as registered in kagent (e.g., "triage-k8s-investigator")
	Description string // human-readable description for logging
}

// DefaultInvestigators returns the standard set of investigator agents.
// Each agent has its own MCP tool access configured in kagent:
//   - triage-k8s-investigator:      kubectl, k8s API (pod describe, events, logs)
//   - triage-logs-investigator:     Loki queries (error patterns, stack traces)
//   - triage-metrics-investigator:  Prometheus queries (resource usage, rate changes)
//   - triage-argocd-investigator:   ArgoCD application sync state and deployment health
func DefaultInvestigators() []InvestigatorConfig {
	return []InvestigatorConfig{
		{Name: "triage-k8s-investigator", Description: "Kubernetes resource state and events"},
		{Name: "triage-logs-investigator", Description: "Log analysis via Loki"},
		{Name: "triage-metrics-investigator", Description: "Metric analysis via Prometheus"},
		{Name: "triage-argocd-investigator", Description: "ArgoCD sync state and deployment health"},
	}
}
