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
	// Templates should parse even with nil DB (DB is only used at request time)
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

func TestIncidentLivePageRendersForInFlightWorkflow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	workflowID := "triage/default/Deployment/catalog-api/KubePodCrashLooping"
	createdAt := time.Now().Add(-2 * time.Minute)
	rows := sqlmock.NewRows([]string{"id", "workflow_id", "namespace", "workload", "kind", "alert_name", "state", "severity", "created_at", "updated_at"}).
		AddRow(int64(7), workflowID, "default", "catalog-api", "Deployment", "KubePodCrashLooping", "triaging", "critical", createdAt, createdAt)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, workflow_id, namespace, workload, kind, alert_name, state, severity, created_at,
		COALESCE(completed_at, created_at) AS updated_at
		FROM triage.reports
		WHERE workflow_id = $1`)).
		WithArgs(workflowID).
		WillReturnRows(rows)

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	h.SetTemporalClient(fakeTemporalClient{status: triageworkflow.WorkflowStatus{Step: "triaging", AlertCount: 3, ElapsedMs: 120000}})

	req := httptest.NewRequest("GET", "/incidents/"+encodeWorkflowPath(workflowID), nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/... = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Live workflow updates every 2 seconds.") {
		t.Fatalf("expected live incident body, got %q", body)
	}
	if !strings.Contains(body, "/incidents/"+encodeWorkflowPath(workflowID)+"/status") {
		t.Fatalf("expected polling endpoint in body, got %q", body)
	}
	if !strings.Contains(body, "KubePodCrashLooping") {
		t.Fatalf("expected alert name in body, got %q", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestIncidentStatusRedirectsToReportWhenTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	workflowID := "triage/default/Deployment/catalog-api/KubePodCrashLooping"
	createdAt := time.Now().Add(-2 * time.Minute)
	rows := sqlmock.NewRows([]string{"id", "workflow_id", "namespace", "workload", "kind", "alert_name", "state", "severity", "created_at", "updated_at"}).
		AddRow(int64(9), workflowID, "default", "catalog-api", "Deployment", "KubePodCrashLooping", "reported", "critical", createdAt, createdAt)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, workflow_id, namespace, workload, kind, alert_name, state, severity, created_at,
		COALESCE(completed_at, created_at) AS updated_at
		FROM triage.reports
		WHERE workflow_id = $1`)).
		WithArgs(workflowID).
		WillReturnRows(rows)

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/incidents/"+encodeWorkflowPath(workflowID)+"/status", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /incidents/.../status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("HX-Redirect"); got != "/reports/9" {
		t.Fatalf("HX-Redirect = %q, want /reports/9", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}
