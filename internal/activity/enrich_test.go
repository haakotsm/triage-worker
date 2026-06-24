package activity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/haakotsm/triage-worker/internal/types"
)

func TestQueryLoki_ParsesStreamsResponse(t *testing.T) {
	lokiResp := lokiQueryResponse{
		Status: "success",
		Data: lokiData{
			ResultType: "streams",
			Result: []lokiStream{
				{
					Stream: map[string]string{"namespace": "default", "pod": "api-server-abc123"},
					Values: [][]string{
						{"1700000000000000000", "ERROR: connection refused to database"},
						{"1700000001000000000", "FATAL: unable to acquire lock"},
					},
				},
				{
					Stream: map[string]string{"namespace": "default", "pod": "api-server-def456"},
					Values: [][]string{
						{"1700000002000000000", "panic: runtime error: nil pointer dereference"},
					},
				},
			},
		},
	}

	respBody, _ := json.Marshal(lokiResp)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query().Get("query")
		if q == "" {
			t.Error("expected query parameter")
		}
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))
	defer server.Close()

	a := &Activities{
		LokiURL:    server.URL,
		HTTPClient: server.Client(),
	}

	identity := types.IncidentIdentity{
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "api-server",
	}

	result, err := a.QueryLoki(context.Background(), identity, nil)
	if err != nil {
		t.Fatalf("QueryLoki() error = %v", err)
	}

	if !result.Available {
		t.Fatalf("expected Available=true, got error: %s", result.Error)
	}
	if result.LogCount != 3 {
		t.Errorf("LogCount = %d, want 3", result.LogCount)
	}
	if len(result.ErrorLines) != 3 {
		t.Fatalf("ErrorLines len = %d, want 3", len(result.ErrorLines))
	}
	if result.ErrorLines[0] != "ERROR: connection refused to database" {
		t.Errorf("ErrorLines[0] = %q, want 'ERROR: connection refused to database'", result.ErrorLines[0])
	}
}

func TestQueryPrometheus_AllQueriesFailUnavailable(t *testing.T) {
	// Every Prometheus query returns 500 → the source is effectively
	// unreachable and must be reported as unavailable, not a clean empty result.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	a := &Activities{
		PrometheusURL: server.URL,
		HTTPClient:    server.Client(),
	}

	identity := types.IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "api"}
	result, err := a.QueryPrometheus(context.Background(), identity, nil)
	if err != nil {
		t.Fatalf("QueryPrometheus() error = %v", err)
	}
	if result.Available {
		t.Error("expected Available=false when all queries fail")
	}
	if result.Error == "" {
		t.Error("expected Error to be populated when all queries fail")
	}
}

func TestQueryPrometheus_SuccessAvailable(t *testing.T) {
	// A valid empty-vector response for every query → Prometheus is reachable,
	// so the result is available even though no samples came back.
	body, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": []any{}},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer server.Close()

	a := &Activities{
		PrometheusURL: server.URL,
		HTTPClient:    server.Client(),
	}

	identity := types.IncidentIdentity{Namespace: "default", Kind: "Deployment", Name: "api"}
	result, err := a.QueryPrometheus(context.Background(), identity, nil)
	if err != nil {
		t.Fatalf("QueryPrometheus() error = %v", err)
	}
	if !result.Available {
		t.Errorf("expected Available=true on reachable Prometheus, got error: %s", result.Error)
	}
}

func TestQueryLoki_HandlesErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	a := &Activities{
		LokiURL:    server.URL,
		HTTPClient: server.Client(),
	}

	identity := types.IncidentIdentity{Namespace: "default", Kind: "Namespace"}
	result, err := a.QueryLoki(context.Background(), identity, nil)
	if err != nil {
		t.Fatalf("QueryLoki() error = %v", err)
	}
	if result.Available {
		t.Error("expected Available=false for error status")
	}
}

func TestQueryLoki_HandlesFailedQueryStatus(t *testing.T) {
	respBody, _ := json.Marshal(lokiQueryResponse{Status: "error"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))
	defer server.Close()

	a := &Activities{
		LokiURL:    server.URL,
		HTTPClient: server.Client(),
	}

	identity := types.IncidentIdentity{Namespace: "default", Kind: "Namespace"}
	result, err := a.QueryLoki(context.Background(), identity, nil)
	if err != nil {
		t.Fatalf("QueryLoki() error = %v", err)
	}
	if result.Available {
		t.Error("expected Available=false for failed query status")
	}
}

func TestQueryLoki_LimitsTo50Lines(t *testing.T) {
	// Build a response with 100 log lines
	values := make([][]string, 100)
	for i := range values {
		values[i] = []string{"1700000000000000000", "error line"}
	}
	lokiResp := lokiQueryResponse{
		Status: "success",
		Data: lokiData{
			ResultType: "streams",
			Result:     []lokiStream{{Values: values}},
		},
	}

	respBody, _ := json.Marshal(lokiResp)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))
	defer server.Close()

	a := &Activities{
		LokiURL:    server.URL,
		HTTPClient: server.Client(),
	}

	identity := types.IncidentIdentity{Namespace: "default", Kind: "Namespace"}
	result, _ := a.QueryLoki(context.Background(), identity, nil)
	if result.LogCount > 50 {
		t.Errorf("LogCount = %d, should be capped at 50", result.LogCount)
	}
}

func TestMatchesPodPrefix(t *testing.T) {
	tests := []struct {
		podName      string
		workloadName string
		want         bool
	}{
		{"api-server-abc123", "api-server", true},
		{"api-server-7f8d9-abc12", "api-server", true},
		{"other-pod-abc", "api-server", false},
		{"api", "api-server", false},
		{"api-server", "api-server", true},
		{"api-serverextra", "api-server", false}, // no dash separator
	}

	for _, tt := range tests {
		t.Run(tt.podName+"_"+tt.workloadName, func(t *testing.T) {
			got := matchesPodPrefix(tt.podName, tt.workloadName)
			if got != tt.want {
				t.Errorf("matchesPodPrefix(%q, %q) = %v, want %v", tt.podName, tt.workloadName, got, tt.want)
			}
		})
	}
}

func TestStoreTriageReport_NilDB(t *testing.T) {
	r := &ReportActivity{DB: nil}
	err := r.StoreTriageReport(context.Background(), types.TriageResult{
		Report: &types.TriageReport{
			Classification: "CrashLoop",
			Severity:       "warning",
			RootCause:      "test",
		},
	})
	if err != nil {
		t.Errorf("StoreTriageReport with nil DB should be no-op, got error: %v", err)
	}
}
