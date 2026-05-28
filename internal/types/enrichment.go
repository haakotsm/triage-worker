package types

import "time"

// EnrichmentResult holds context gathered from observability sources.
type EnrichmentResult struct {
	Prometheus PrometheusResult `json:"prometheus"`
	Kubernetes KubernetesResult `json:"kubernetes"`
	Loki       LokiResult       `json:"loki"`
}

// PrometheusResult holds metrics enrichment data.
type PrometheusResult struct {
	Available    bool              `json:"available"`
	RestartRate  float64           `json:"restart_rate_5m"`
	MemoryPct    float64           `json:"memory_usage_pct"`
	CPUUsage     float64           `json:"cpu_usage_cores"`
	CustomMetrics map[string]float64 `json:"custom_metrics,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// KubernetesResult holds K8s API enrichment data.
type KubernetesResult struct {
	Available       bool     `json:"available"`
	PodPhase        string   `json:"pod_phase,omitempty"`
	ContainerStates []string `json:"container_states,omitempty"`
	RecentEvents    []string `json:"recent_events,omitempty"`
	ExitCodes       []int32  `json:"exit_codes,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// LokiResult holds log enrichment data.
type LokiResult struct {
	Available  bool     `json:"available"`
	ErrorLines []string `json:"error_lines,omitempty"`
	LogCount   int      `json:"log_count"`
	Error      string   `json:"error,omitempty"`
}

// TriageParams are the initial parameters for the TriageWorkflow.
type TriageParams struct {
	Identity IncidentIdentity `json:"identity"`
	// CorrelationDebounce overrides the default 60s debounce window.
	// Zero means use the workflow default (60s).
	CorrelationDebounce time.Duration `json:"correlation_debounce,omitempty"`
	// CorrelationHardCap overrides the default 5m hard cap.
	// Zero means use the workflow default (5m).
	CorrelationHardCap time.Duration `json:"correlation_hard_cap,omitempty"`
}

// TriageResult is the final output of the TriageWorkflow.
type TriageResult struct {
	WorkflowID     string           `json:"workflow_id"`
	Identity       IncidentIdentity `json:"identity"`
	AlertCount     int              `json:"alert_count"`
	Classification string           `json:"classification"`
	Severity       string           `json:"severity"`
	RootCause      string           `json:"root_cause"`
	Report         *TriageReport    `json:"report"`
	StartedAt      time.Time        `json:"started_at"`
	CompletedAt    time.Time        `json:"completed_at"`
}
