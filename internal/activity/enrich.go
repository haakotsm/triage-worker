package activity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/haakotsm/triage-worker/internal/types"
)

// Activities holds shared dependencies for activity implementations.
type Activities struct {
	PrometheusURL string
	LokiURL       string
	HTTPClient    *http.Client
}

// QueryPrometheus fetches relevant metrics for the incident.
func (a *Activities) QueryPrometheus(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.PrometheusResult, error) {
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
	if identity.Kind == "Deployment" || identity.Kind == "StatefulSet" {
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

// QueryKubernetesAPI fetches pod states, events, and exit codes.
func QueryKubernetesAPI(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.KubernetesResult, error) {
	// Implementation uses client-go — see client/k8s.go
	// Stub: returns unavailable if not wired
	return types.KubernetesResult{Available: false, Error: "not implemented"}, nil
}

// QueryLoki fetches recent error logs for the affected workload.
func (a *Activities) QueryLoki(ctx context.Context, identity types.IncidentIdentity, alerts []types.Alert) (types.LokiResult, error) {
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

	// Parse Loki response (simplified — extract log lines)
	// Full implementation would parse the JSON stream response
	return types.LokiResult{
		Available: true,
		LogCount:  0, // TODO: parse response
	}, nil
}
