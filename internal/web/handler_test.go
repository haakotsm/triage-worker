package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	triageworkflow "github.com/haakotsm/triage-worker/internal/workflow"
)

type fakeTemporalClient struct {
	client.Client
	status triageworkflow.WorkflowStatus
	err    error
}

func (f fakeTemporalClient) QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...interface{}) (converter.EncodedValue, error) {
	if f.err != nil {
		return nil, f.err
	}
	payloads, err := converter.GetDefaultDataConverter().ToPayloads(f.status)
	if err != nil {
		return nil, err
	}
	return client.NewValue(payloads), nil
}

func TestNewHandler_TemplatesParse(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	if h == nil {
		t.Fatal("NewHandler() returned nil handler")
	}
}

func TestStaticAssets(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	tests := []struct {
		path string
	}{
		{"/static/htmx.min.js"},
		{"/static/alpine.min.js"},
		{"/static/output.css"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", tt.path, w.Code)
			}
			if w.Body.Len() == 0 {
				t.Errorf("GET %s returned empty body", tt.path)
			}
		})
	}
}

func TestUnknownPath_Returns404(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /nonexistent = %d, want 404", w.Code)
	}
}

func TestIncidentDetailReturns200ForInFlightIncident(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	report := testReport(7, "triaging")
	mock.ExpectQuery(regexp.QuoteMeta("FROM triage.reports WHERE id = $1")).
		WithArgs(int64(7)).
		WillReturnRows(reportRows(report))

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	h.SetTemporalClient(fakeTemporalClient{status: triageworkflow.WorkflowStatus{Step: "triaging", AlertCount: 3, ElapsedMs: 120000}})

	req := httptest.NewRequest("GET", "/incidents/7", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/7 = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "hx-get=\"/incidents/7/status\"") {
		t.Fatalf("expected polling target in body, got %q", body)
	}
	if !strings.Contains(body, "In flight") {
		t.Fatalf("expected in-flight badge in body, got %q", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestIncidentDetailReturns200ForCompletedIncident(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	report := testReport(9, "reported")
	resolvedAt := time.Now().Add(-5 * time.Minute)
	report.RootCause = "Pod OOM due to memory leak"
	report.ResolvedAt = &resolvedAt
	report.Recommendations = []Recommendation{{Action: "Check logs", Command: "kubectl logs deploy/catalog-api", Source: "l1"}}
	mock.ExpectQuery(regexp.QuoteMeta("FROM triage.reports WHERE id = $1")).
		WithArgs(int64(9)).
		WillReturnRows(reportRows(report))

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/incidents/9", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/9 = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Root Cause") || !strings.Contains(body, report.RootCause) {
		t.Fatalf("expected completed incident analysis in body, got %q", body)
	}
	if strings.Contains(body, "hx-get=\"/incidents/9/status\"") {
		t.Fatalf("expected completed incident page to stop polling, got %q", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestIncidentStatusPollReturnsProgressPartialForInFlightIncident(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	report := testReport(11, "triaging")
	mock.ExpectQuery(regexp.QuoteMeta("FROM triage.reports WHERE id = $1")).
		WithArgs(int64(11)).
		WillReturnRows(reportRows(report))

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	h.SetTemporalClient(fakeTemporalClient{status: triageworkflow.WorkflowStatus{Step: "triaging", AlertCount: 4, ElapsedMs: 30000}})

	req := httptest.NewRequest("GET", "/incidents/11/status", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/11/status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "hx-trigger=\"every 2s") {
		t.Fatalf("expected polling trigger in body, got %q", body)
	}
	if !strings.Contains(body, "Correlated") {
		t.Fatalf("expected progress stats in body, got %q", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestIncidentStatusPollReturnsCompletePartialForTerminalIncident(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	report := testReport(12, "reported")
	report.RootCause = "Node pressure evicted critical pod"
	report.Recommendations = []Recommendation{{Action: "Inspect events", Command: "kubectl get events -n default", Source: "l1"}}
	mock.ExpectQuery(regexp.QuoteMeta("FROM triage.reports WHERE id = $1")).
		WithArgs(int64(12)).
		WillReturnRows(reportRows(report))

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/incidents/12/status", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/12/status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Fatalf("expected no HX-Redirect, got %q", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "hx-trigger=") {
		t.Fatalf("expected completed partial to stop polling, got %q", body)
	}
	if !strings.Contains(body, report.RootCause) {
		t.Fatalf("expected completed partial content, got %q", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestReportRedirectsToUnifiedIncidentPath(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/reports/15", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /reports/15 = %d, want 301", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/incidents/15" {
		t.Fatalf("Location = %q, want /incidents/15", got)
	}
}

func testReport(id int64, state string) Report {
	now := time.Now().Add(-2 * time.Minute).Round(time.Second)
	return Report{
		ID:              id,
		WorkflowID:      "triage/default/Deployment/catalog-api/KubePodCrashLooping",
		Namespace:       "default",
		Workload:        "catalog-api",
		Kind:            "Deployment",
		AlertName:       "KubePodCrashLooping",
		Classification:  "availability",
		Severity:        "critical",
		Summary:         "Catalog API is restarting",
		BlastRadius:     "deployment",
		State:           state,
		RootCause:       "",
		CausalChain:     []string{"Pod restarted", "Readiness probe failed"},
		Evidence:        []Evidence{{Observation: "CrashLoopBackOff", Source: "kubernetes", Strength: "strong"}},
		Recommendations: []Recommendation{{Action: "Check rollout", Command: "kubectl rollout status deploy/catalog-api", Source: "agent", Risk: "low"}},
		Confidence:      87,
		AlertCount:      2,
		StartedAt:       now,
		CreatedAt:       now,
	}
}

func reportRows(report Report) *sqlmock.Rows {
	var completedAt interface{}
	if report.CompletedAt != nil {
		completedAt = *report.CompletedAt
	}
	var resolvedAt interface{}
	if report.ResolvedAt != nil {
		resolvedAt = *report.ResolvedAt
	}
	return sqlmock.NewRows([]string{
		"id", "workflow_id", "namespace", "workload", "kind", "alert_name",
		"classification", "severity", "root_cause", "causal_chain", "evidence",
		"recommendations", "confidence", "escalation_needed", "alert_count",
		"started_at", "completed_at", "created_at", "resolved_at", "summary", "blast_radius", "state",
	}).AddRow(
		report.ID,
		report.WorkflowID,
		report.Namespace,
		report.Workload,
		report.Kind,
		report.AlertName,
		report.Classification,
		report.Severity,
		report.RootCause,
		`["Pod restarted","Readiness probe failed"]`,
		`[{"observation":"CrashLoopBackOff","source":"kubernetes","strength":"strong"}]`,
		recommendationJSON(report.Recommendations),
		report.Confidence,
		report.EscalationNeeded,
		report.AlertCount,
		report.StartedAt,
		completedAt,
		report.CreatedAt,
		resolvedAt,
		report.Summary,
		report.BlastRadius,
		report.State,
	)
}

func recommendationJSON(recs []Recommendation) string {
	parts := make([]string, 0, len(recs))
	for _, rec := range recs {
		parts = append(parts, `{"action":"`+rec.Action+`","command":"`+rec.Command+`","risk":"`+rec.Risk+`","source":"`+rec.Source+`","expected":"`+rec.Expected+`"}`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
