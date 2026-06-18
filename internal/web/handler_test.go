package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

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
		{"/static/alpine-focus.min.js"},
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

func TestTemplateFunctions(t *testing.T) {
	// Exercise template funcs by calling them directly
	tests := []struct {
		state     string
		wantIcon  string
		wantClass string
		wantLabel string
	}{
		{"processing", "⏳", "badge-ghost text-base-content/50", "Processing"},
		{"reported", "🔔", "badge-error animate-pulse motion-reduce:animate-none", "Reported"},
		{"acknowledged", "👤", "badge-info", "Acknowledged"},
		{"resolved", "✓", "badge-success opacity-70", "Resolved"},
		{"unknown", "❓", "badge-ghost", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			// Access template functions indirectly by calling them
			iconFn := templateFuncs()["stateIcon"].(func(string) string)
			classFn := templateFuncs()["incidentStateClass"].(func(string) string)
			labelFn := templateFuncs()["stateLabel"].(func(string) string)

			if got := iconFn(tt.state); got != tt.wantIcon {
				t.Errorf("stateIcon(%q) = %q, want %q", tt.state, got, tt.wantIcon)
			}
			if got := classFn(tt.state); got != tt.wantClass {
				t.Errorf("incidentStateClass(%q) = %q, want %q", tt.state, got, tt.wantClass)
			}
			if got := labelFn(tt.state); got != tt.wantLabel {
				t.Errorf("stateLabel(%q) = %q, want %q", tt.state, got, tt.wantLabel)
			}
		})
	}
}

func TestAwaitingDiagnosis(t *testing.T) {
	fn := templateFuncs()["awaitingDiagnosis"].(func(Report) bool)

	tests := []struct {
		name string
		r    Report
		want bool
	}{
		{"processing no data", Report{State: "processing", RootCause: ""}, true},
		{"correlating no data", Report{State: "correlating", RootCause: ""}, true},
		{"processing with data", Report{State: "processing", RootCause: "OOM killed"}, false},
		{"reported", Report{State: "reported", RootCause: "OOM killed"}, false},
		{"reported no data", Report{State: "reported", RootCause: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fn(tt.r); got != tt.want {
				t.Errorf("awaitingDiagnosis(%+v) = %v, want %v", tt.r, got, tt.want)
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

func TestToastTrigger(t *testing.T) {
	got := toastTrigger("error", "already acknowledged")
	want := `{"toast":{"level":"error","message":"already acknowledged"}}`
	if got != want {
		t.Errorf("toastTrigger() = %q, want %q", got, want)
	}
}

func TestRenderError_HTMXReturns200(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	// htmx requests must get 200 so the fragment is swapped (htmx discards
	// non-2xx bodies); full-page loads keep the 500.
	t.Run("htmx", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/partials/stats", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.renderError(w, req, "Failed to load stats")
		if w.Code != http.StatusOK {
			t.Errorf("renderError(htmx) status = %d, want 200", w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, "Failed to load stats") {
			t.Errorf("renderError(htmx) body missing message: %q", body)
		}
	})

	t.Run("full-page", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		h.renderError(w, req, "Failed to load stats")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("renderError(full-page) status = %d, want 500", w.Code)
		}
	})
}

func TestHXError(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	t.Run("htmx toast with no swap", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/incidents/1/acknowledge", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.hxError(w, req, http.StatusConflict, "already acknowledged")
		if w.Code != http.StatusOK {
			t.Errorf("hxError(htmx) status = %d, want 200 (so htmx processes headers)", w.Code)
		}
		if got := w.Header().Get("HX-Reswap"); got != "none" {
			t.Errorf("hxError(htmx) HX-Reswap = %q, want none", got)
		}
		if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "already acknowledged") {
			t.Errorf("hxError(htmx) HX-Trigger = %q, want it to carry the message", got)
		}
	})

	t.Run("non-htmx keeps status and json", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/incidents/1/acknowledge", nil)
		w := httptest.NewRecorder()
		h.hxError(w, req, http.StatusConflict, "already acknowledged")
		if w.Code != http.StatusConflict {
			t.Errorf("hxError(non-htmx) status = %d, want 409", w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, `"error"`) {
			t.Errorf("hxError(non-htmx) body = %q, want json error", body)
		}
	})
}

func TestHXStateConflict_NonHTMXReturns409(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// Non-htmx clients must still get a real 409 (no DB touched on this path).
	req := httptest.NewRequest("POST", "/api/incidents/1/acknowledge", nil)
	w := httptest.NewRecorder()
	h.hxStateConflict(w, req, 1, "already acknowledged")
	if w.Code != http.StatusConflict {
		t.Errorf("hxStateConflict(non-htmx) status = %d, want 409", w.Code)
	}
}

func TestHandleAcknowledge_InvalidIDHTMX(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// Invalid ID fails before any DB access; htmx must get 200 + toast, not a
	// discarded 400.
	req := httptest.NewRequest("POST", "/api/incidents/not-a-number/acknowledge", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("HX-Reswap"); got != "none" {
		t.Errorf("HX-Reswap = %q, want none", got)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "invalid incident ID") {
		t.Errorf("HX-Trigger = %q, want it to carry the error message", got)
	}
}

func TestHandleAcknowledge_ConflictHTMX(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	// UPDATE matches 0 rows (already acknowledged/resolved), then fetchReport
	// finds no row → hxStateConflict falls back to a toast with no swap.
	mock.ExpectExec("UPDATE triage.reports").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT id, workflow_id").
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty → report == nil

	req := httptest.NewRequest("POST", "/api/incidents/5/acknowledge",
		strings.NewReader(`{"assignee":"alice@example.com"}`))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (conflict must not be a discarded 409)", w.Code)
	}
	if got := w.Header().Get("HX-Reswap"); got != "none" {
		t.Errorf("HX-Reswap = %q, want none", got)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "already acknowledged") {
		t.Errorf("HX-Trigger = %q, want the conflict message", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestDashboardContent_IsRealDashboardNotReportTable(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	// The HX branch of handleDashboard (e.g. the detail breadcrumb's hx-get="/")
	// must render the real dashboard content — stats, incidents and SSE wiring —
	// not the orphaned report-table fragment. Render the partial directly so the
	// assertion needs no DB.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.render(w, req, "dashboard-content", DashboardData{SSEEnabled: true})

	body := w.Body.String()
	for _, want := range []string{`id="stats-panel"`, `id="incidents-panel"`, `sse-connect="/events"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard-content missing %q — HX dashboard nav would render a degraded page", want)
		}
	}
}

func TestHandleRetriage_NotConfiguredHTMX(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// retriage starter is not wired (h.retriage == nil). For htmx this must be a
	// 200 + toast, not a discarded 503.
	req := httptest.NewRequest("POST", "/api/incidents/1/retriage", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "not configured") {
		t.Errorf("HX-Trigger = %q, want the not-configured message", got)
	}
}

// stubRetrieveStarter is a test double for the re-triage workflow starter.
type stubRetrieveStarter struct {
	newWfID string
	err     error
	called  bool
}

func (s *stubRetrieveStarter) StartRetriage(_ context.Context, _, _, _, _, _ string) (string, error) {
	s.called = true
	return s.newWfID, s.err
}

func TestHandleRetriage_SuccessHTMX(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	h, err := NewHandler(db, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	stub := &stubRetrieveStarter{newWfID: "triage/ns/Deployment/web/OOMKilled" + "::2"}
	h.SetRetrieveStarter(stub)

	// Incident lookup → a resolved (non-processing) incident.
	mock.ExpectQuery("SELECT workflow_id, namespace, workload, kind, alert_name, state").
		WithArgs(int64(5)).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "namespace", "workload", "kind", "alert_name", "state"}).
			AddRow("triage/ns/Deployment/web/OOMKilled", "ns", "web", "Deployment", "OOMKilled", "resolved"))
	// Cap query → 1 active version (< 3), so re-triage proceeds.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	req := httptest.NewRequest("POST", "/api/incidents/5/retriage", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !stub.called {
		t.Error("StartRetriage was not called")
	}
	if got := w.Header().Get("HX-Reswap"); got != "none" {
		t.Errorf("HX-Reswap = %q, want none (original incident is unchanged)", got)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "processing") {
		t.Errorf("HX-Trigger = %q, want the success toast", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestBuildTimeline(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	now := time.Now()
	completed := now.Add(-45 * time.Minute)
	acked := now.Add(-30 * time.Minute)
	resolved := now.Add(-5 * time.Minute)

	report := &Report{
		ID:              1,
		AlertName:       "OOMKilled",
		State:           "resolved",
		CreatedAt:       now.Add(-1 * time.Hour),
		CompletedAt:     &completed,
		AssignedTo:      "alice@example.com",
		AcknowledgedAt:  &acked,
		EscalationLevel: "L2",
		ResolvedAt:      &resolved,
	}

	notes := []Note{
		{ID: 1, Author: "alice@example.com", Body: "Scaled replicas to 5", CreatedAt: now.Add(-20 * time.Minute)},
		{ID: 2, Author: "bob@example.com", Body: "Confirmed fix", CreatedAt: now.Add(-10 * time.Minute)},
	}

	timeline := h.buildTimeline(report, notes)

	// Should have: created, completed, acknowledged, escalated, 2 notes, resolved = 7 entries
	if len(timeline) != 7 {
		t.Fatalf("buildTimeline() returned %d entries, want 7", len(timeline))
	}

	// First entry should be most recent (resolved)
	if timeline[0].Type != "resolved" {
		t.Errorf("first entry type = %q, want 'resolved'", timeline[0].Type)
	}

	// Last entry should be oldest (created)
	if timeline[len(timeline)-1].Type != "created" {
		t.Errorf("last entry type = %q, want 'created'", timeline[len(timeline)-1].Type)
	}

	// Check notes are included
	noteCount := 0
	for _, e := range timeline {
		if e.Type == "note" {
			noteCount++
		}
	}
	if noteCount != 2 {
		t.Errorf("timeline has %d notes, want 2", noteCount)
	}
}

func TestBuildTimeline_MinimalReport(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	report := &Report{
		ID:        1,
		AlertName: "CrashLoopBackOff",
		State:     "processing",
		CreatedAt: time.Now().Add(-5 * time.Minute),
	}

	timeline := h.buildTimeline(report, nil)

	// Should have just: created = 1 entry
	if len(timeline) != 1 {
		t.Fatalf("buildTimeline() returned %d entries, want 1", len(timeline))
	}
	if timeline[0].Type != "created" {
		t.Errorf("entry type = %q, want 'created'", timeline[0].Type)
	}
}
