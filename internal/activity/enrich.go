package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/haakotsm/triage-worker/internal/metrics"
	"github.com/haakotsm/triage-worker/internal/types"
)

// Activities holds shared dependencies for activity implementations.
type Activities struct {
	PrometheusURL string
	LokiURL       string
	HTTPClient    *http.Client
}

// QueryPrometheus fetches relevant metrics for the incident. It times the call
// and records the per-source enrichment metric; the heavy lifting is in the
// unexported queryPrometheus so the recorded method name stays stable for
// Temporal activity registration.
func (a *Activities) QueryPrometheus(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.PrometheusResult, error) {
	start := time.Now()
	res, err := a.queryPrometheus(ctx, identity, alerts)
	recordEnrichment(metrics.SourcePrometheus, res.Available, err, start)
	return res, err
}

func (a *Activities) queryPrometheus(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.PrometheusResult, error) {
	client, err := promapi.NewClient(promapi.Config{
		Address: a.PrometheusURL,
	})
	if err != nil {
		return types.PrometheusResult{Available: false, Error: err.Error()}, nil
	}

	api := promv1.NewAPI(client)
	result := types.PrometheusResult{Available: true}
	now := time.Now()

	// Query restart rate over last 5 minutes
	restartQuery := fmt.Sprintf(
		`increase(kube_pod_container_status_restarts_total{namespace="%s"}[5m])`,
		identity.Namespace,
	)
	// Narrow restart query to pods matching the workload name prefix.
	// For App kind this is best-effort — assumes pod names share the app
	// label value as a prefix, which is true for most Helm-managed workloads.
	if identity.Kind == "Deployment" || identity.Kind == "StatefulSet" || identity.Kind == "App" || identity.Kind == "Pod" {
		restartQuery = fmt.Sprintf(
			`increase(kube_pod_container_status_restarts_total{namespace="%s", pod=~"%s-.*"}[5m])`,
			identity.Namespace, identity.Name,
		)
	}

	val, _, err := api.Query(ctx, restartQuery, now)
	if err == nil && val.Type() == model.ValVector {
		vector := val.(model.Vector)
		for _, sample := range vector {
			result.RestartRate += float64(sample.Value)
		}
	}

	// Query memory usage percentage
	memQuery := fmt.Sprintf(
		`sum(container_memory_working_set_bytes{namespace="%s"}) / sum(kube_pod_container_resource_limits{namespace="%s", resource="memory"}) * 100`,
		identity.Namespace, identity.Namespace,
	)
	val, _, err = api.Query(ctx, memQuery, now)
	if err == nil && val.Type() == model.ValVector {
		vector := val.(model.Vector)
		if len(vector) > 0 {
			result.MemoryPct = float64(vector[0].Value)
		}
	}

	// Query CPU usage
	cpuQuery := fmt.Sprintf(
		`sum(rate(container_cpu_usage_seconds_total{namespace="%s"}[5m]))`,
		identity.Namespace,
	)
	val, _, err = api.Query(ctx, cpuQuery, now)
	if err == nil && val.Type() == model.ValVector {
		vector := val.(model.Vector)
		if len(vector) > 0 {
			result.CPUUsage = float64(vector[0].Value)
		}
	}

	return result, nil
}

// K8sActivity holds a reusable Kubernetes clientset to avoid per-call TLS/discovery overhead.
type K8sActivity struct {
	Clientset kubernetes.Interface
}

// QueryKubernetesAPI fetches pod states, events, and exit codes using client-go.
func (k *K8sActivity) QueryKubernetesAPI(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.KubernetesResult, error) {
	start := time.Now()
	res, err := k.queryKubernetesAPI(ctx, identity, alerts)
	recordEnrichment(metrics.SourceK8s, res.Available, err, start)
	return res, err
}

func (k *K8sActivity) queryKubernetesAPI(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.KubernetesResult, error) {
	clientset := k.Clientset

	result := types.KubernetesResult{Available: true}

	// List pods matching the workload
	var labelSelector string
	switch identity.Kind {
	case "Deployment":
		labelSelector = fmt.Sprintf("app=%s", identity.Name)
	case "StatefulSet":
		labelSelector = fmt.Sprintf("app=%s", identity.Name)
	case "DaemonSet":
		labelSelector = fmt.Sprintf("app=%s", identity.Name)
	case "App":
		// Try app.kubernetes.io/name first (standard), fall back to app
		labelSelector = fmt.Sprintf("app.kubernetes.io/name=%s", identity.Name)
	default:
		// Pod/Namespace/Job kinds — no reliable label selector, list all
		// pods in namespace. Event matching downstream filters by prefix.
		labelSelector = ""
	}

	pods, err := clientset.CoreV1().Pods(identity.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		Limit:         10,
	})
	if err != nil {
		return types.KubernetesResult{Available: false, Error: fmt.Sprintf("list pods: %v", err)}, nil
	}

	for _, pod := range pods.Items {
		result.PodPhase = string(pod.Status.Phase)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				result.ContainerStates = append(result.ContainerStates,
					fmt.Sprintf("%s: waiting (%s: %s)", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
			} else if cs.State.Terminated != nil {
				result.ContainerStates = append(result.ContainerStates,
					fmt.Sprintf("%s: terminated (exit=%d, reason=%s)", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason))
				result.ExitCodes = append(result.ExitCodes, cs.State.Terminated.ExitCode)
			} else if cs.State.Running != nil && cs.RestartCount > 0 {
				result.ContainerStates = append(result.ContainerStates,
					fmt.Sprintf("%s: running (restarts=%d)", cs.Name, cs.RestartCount))
			}
			if cs.LastTerminationState.Terminated != nil {
				result.ExitCodes = append(result.ExitCodes, cs.LastTerminationState.Terminated.ExitCode)
			}
		}
	}

	// Get recent events for the workload
	events, err := clientset.CoreV1().Events(identity.Namespace).List(ctx, metav1.ListOptions{
		Limit: 20,
	})
	if err == nil {
		fiveMinAgo := time.Now().Add(-5 * time.Minute)
		for _, event := range events.Items {
			// Filter to relevant events (by time and involvement)
			eventTime := event.LastTimestamp.Time
			if eventTime.IsZero() {
				eventTime = event.CreationTimestamp.Time
			}
			if eventTime.Before(fiveMinAgo) {
				continue
			}
			if event.Type == "Normal" {
				continue
			}
			// Match by involved object or namespace-wide
			if event.InvolvedObject.Name == identity.Name ||
				(identity.Kind != "Namespace" && matchesPodPrefix(event.InvolvedObject.Name, identity.Name)) {
				result.RecentEvents = append(result.RecentEvents,
					fmt.Sprintf("[%s] %s: %s", event.Type, event.Reason, event.Message))
			}
		}
	}

	return result, nil
}

// matchesPodPrefix checks if a pod name could belong to the workload (prefix match).
func matchesPodPrefix(podName, workloadName string) bool {
	if len(podName) < len(workloadName) {
		return false
	}
	return podName[:len(workloadName)] == workloadName && (len(podName) == len(workloadName) || podName[len(workloadName)] == '-')
}

// QueryLoki fetches recent error logs for the affected workload.
func (a *Activities) QueryLoki(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.LokiResult, error) {
	start := time.Now()
	res, err := a.queryLoki(ctx, identity, alerts)
	recordEnrichment(metrics.SourceLoki, res.Available, err, start)
	return res, err
}

// recordEnrichment maps an enrichment sub-source call to its outcome metric. A
// call counts as an error if it returned a non-nil error or reported the source
// as unavailable (graceful-degradation returns nil error but Available=false).
func recordEnrichment(source string, available bool, err error, start time.Time) {
	outcome := metrics.OutcomeSuccess
	if err != nil || !available {
		outcome = metrics.OutcomeError
	}
	metrics.RecordEnrichment(source, outcome, time.Since(start))
}

func (a *Activities) queryLoki(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.LokiResult, error) {
	// LogQL query for errors in the last 30 minutes
	query := fmt.Sprintf(
		`{namespace="%s"} |~ "(?i)(error|fatal|panic|exception)" | line_format "{{.message}}"`,
		identity.Namespace,
	)
	if identity.Kind != "Namespace" {
		query = fmt.Sprintf(
			`{namespace="%s", pod=~"%s.*"} |~ "(?i)(error|fatal|panic|exception)"`,
			identity.Namespace, identity.Name,
		)
	}

	// Query Loki HTTP API
	lokiURL := fmt.Sprintf("%s/loki/api/v1/query_range", a.LokiURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lokiURL, nil)
	if err != nil {
		return types.LokiResult{Available: false, Error: err.Error()}, nil
	}

	params := req.URL.Query()
	params.Set("query", query)
	params.Set("start", fmt.Sprintf("%d", time.Now().Add(-30*time.Minute).UnixNano()))
	params.Set("end", fmt.Sprintf("%d", time.Now().UnixNano()))
	params.Set("limit", "50")
	req.URL.RawQuery = params.Encode()

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return types.LokiResult{Available: false, Error: err.Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return types.LokiResult{Available: false, Error: fmt.Sprintf("loki returned %d", resp.StatusCode)}, nil
	}

	// Parse Loki query_range JSON response
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return types.LokiResult{Available: false, Error: fmt.Sprintf("read body: %v", err)}, nil
	}

	var lokiResp lokiQueryResponse
	if err := json.Unmarshal(body, &lokiResp); err != nil {
		return types.LokiResult{Available: false, Error: fmt.Sprintf("parse response: %v", err)}, nil
	}

	if lokiResp.Status != "success" {
		return types.LokiResult{Available: false, Error: fmt.Sprintf("loki query failed: %s", lokiResp.Status)}, nil
	}

	var errorLines []string
	for _, stream := range lokiResp.Data.Result {
		for _, entry := range stream.Values {
			if len(entry) >= 2 {
				line := entry[1]
				errorLines = append(errorLines, line)
				if len(errorLines) >= 50 {
					break
				}
			}
		}
		if len(errorLines) >= 50 {
			break
		}
	}

	return types.LokiResult{
		Available:  true,
		LogCount:   len(errorLines),
		ErrorLines: errorLines,
	}, nil
}

// lokiQueryResponse models the Loki query_range response.
type lokiQueryResponse struct {
	Status string   `json:"status"`
	Data   lokiData `json:"data"`
}

type lokiData struct {
	ResultType string       `json:"resultType"`
	Result     []lokiStream `json:"result"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}
