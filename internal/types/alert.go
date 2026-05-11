package types

import (
	"regexp"
	"strings"
	"time"
)

var (
	// safeK8sName matches valid Kubernetes DNS label names (RFC 1123).
	safeK8sName = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$`)
	// safeLabelValue matches values safe for use in PromQL/LogQL queries and shell commands.
	safeLabelValue = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,252}$`)
)

// SanitizeK8sName validates a Kubernetes resource name against DNS label rules.
// Returns the name unchanged if valid, otherwise lowercases and replaces
// invalid characters with hyphens to prevent PromQL/LogQL and command injection.
func SanitizeK8sName(name string) string {
	if safeK8sName.MatchString(name) {
		return name
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, name)
	safe = strings.Trim(safe, "-")
	if len(safe) > 63 {
		safe = safe[:63]
		safe = strings.TrimRight(safe, "-")
	}
	if safe == "" {
		return "unknown"
	}
	return safe
}

// SanitizeLabelValue validates a label-derived value for safe use in PromQL
// queries, LogQL queries, and shell commands. Allows alphanumeric, hyphen,
// underscore, and dot. Drops all other characters.
func SanitizeLabelValue(value string) string {
	if safeLabelValue.MatchString(value) {
		return value
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			return r
		default:
			return -1
		}
	}, value)
	if len(safe) > 253 {
		safe = safe[:253]
	}
	if safe == "" {
		return "unknown"
	}
	return safe
}

// normalizeCronJobName strips the CronJob timestamp suffix from a Job name.
// CronJob-spawned Jobs are named "{cronjob}-{unix-minutes}" where the suffix
// is scheduledTime.Unix()/60. For 2020–2040, this is 26000000–37000000.
// Returns the base CronJob name for stable grouping.
func normalizeCronJobName(jobName string) string {
	lastDash := strings.LastIndex(jobName, "-")
	if lastDash <= 0 || lastDash == len(jobName)-1 {
		return jobName
	}
	suffix := jobName[lastDash+1:]
	// CronJob timestamps are exactly 8 digits in the range 26000000–40000000
	if len(suffix) != 8 {
		return jobName
	}
	val := 0
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return jobName
		}
		val = val*10 + int(c-'0')
	}
	// Validate plausible CronJob timestamp range (2020-01-01 to ~2046)
	if val >= 26000000 && val <= 40000000 {
		return jobName[:lastDash]
	}
	return jobName
}

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
// All label-derived values are sanitized to prevent PromQL/LogQL and command injection.
func DeriveIdentity(labels map[string]string) IncidentIdentity {
	ns := labels["namespace"]
	if ns == "" {
		ns = "cluster"
	} else {
		ns = SanitizeK8sName(ns)
	}

	alertName := SanitizeLabelValue(labels["alertname"])

	// Prefer workload-level labels (set by kube-state-metrics)
	if name := labels["deployment"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "Deployment", Name: SanitizeK8sName(name), AlertName: alertName}
	}
	if name := labels["statefulset"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "StatefulSet", Name: SanitizeK8sName(name), AlertName: alertName}
	}
	if name := labels["daemonset"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "DaemonSet", Name: SanitizeK8sName(name), AlertName: alertName}
	}
	// CronJob label (set by kube-state-metrics) takes priority over job_name
	// to group all runs of the same CronJob into one incident.
	if name := labels["cronjob"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "CronJob", Name: SanitizeK8sName(name), AlertName: alertName}
	}
	if name := labels["job_name"]; name != "" {
		// Strip CronJob timestamp suffix if present (e.g. "canary-28840860" → "canary")
		return IncidentIdentity{Namespace: ns, Kind: "Job", Name: SanitizeK8sName(normalizeCronJobName(name)), AlertName: alertName}
	}
	// Fall back to pod
	if name := labels["pod"]; name != "" {
		return IncidentIdentity{Namespace: ns, Kind: "Pod", Name: SanitizeK8sName(name), AlertName: alertName}
	}
	// Namespace-level fallback
	return IncidentIdentity{Namespace: ns, Kind: "Namespace", Name: ns, AlertName: alertName}
}
