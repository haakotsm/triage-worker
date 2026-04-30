package types

import "time"

// Alert represents a single Alertmanager alert.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// AlertGroup is the Alertmanager webhook payload.
type AlertGroup struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// FiringAlerts returns only alerts with status "firing".
func (g *AlertGroup) FiringAlerts() []Alert {
	var firing []Alert
	for _, a := range g.Alerts {
		if a.Status == "firing" {
			firing = append(firing, a)
		}
	}
	return firing
}

// IncidentIdentity derives a stable workflow ID from alert labels.
// Priority: owner workload > pod > namespace-level.
type IncidentIdentity struct {
	Namespace string
	Kind      string
	Name      string
	AlertName string
}

// WorkflowID returns the Temporal workflow ID for this incident.
func (id IncidentIdentity) WorkflowID() string {
	return "triage/" + id.Namespace + "/" + id.Kind + "/" + id.Name + "/" + id.AlertName
}

// DeriveIdentity extracts incident identity from alert labels.
// Prefers owner-level labels over pod-level to avoid over-fragmentation.
func DeriveIdentity(labels map[string]string) IncidentIdentity {
	ns := labels["namespace"]
	if ns == "" {
		ns = "cluster"
	}

	alertName := labels["alertname"]

	// Prefer workload-level labels (set by kube-state-metrics)
	if name := labels["deployment"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "Deployment", Name: name, AlertName: alertName}
	}
	if name := labels["statefulset"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "StatefulSet", Name: name, AlertName: alertName}
	}
	if name := labels["daemonset"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "DaemonSet", Name: name, AlertName: alertName}
	}
	if name := labels["job_name"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "Job", Name: name, AlertName: alertName}
	}
	// Fall back to pod
	if name := labels["pod"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "Pod", Name: name, AlertName: alertName}
	}
	// Namespace-level fallback
	return IncidentIdentity{Namespace: ns, Kind: "Namespace", Name: ns, AlertName: alertName}
}
