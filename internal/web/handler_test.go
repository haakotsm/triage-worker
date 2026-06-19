package web

import (
	"context"
	"html/template"
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
		{"/static/alpine-components.js"},
		{"/static/theme-init.js"},
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
		wantClass string
		wantLabel string
	}{
		{"processing", "badge-ghost text-base-content/50", "Processing"},
		{"reported", "badge-error animate-pulse motion-reduce:animate-none", "Reported"},
		{"acknowledged", "badge-info", "Acknowledged"},
		{"resolved", "badge-success opacity-70", "Resolved"},
		{"unknown", "badge-ghost", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			// Access template functions indirectly by calling them
			iconFn := templateFuncs()["stateIcon"].(func(string) template.HTML)
			classFn := templateFuncs()["incidentStateClass"].(func(string) string)
			labelFn := templateFuncs()["stateLabel"].(func(string) string)

			// stateIcon now returns an inline SVG instead of an OS emoji.
			if got := string(iconFn(tt.state)); !strings.Contains(got, "<svg") {
				t.Errorf("stateIcon(%q) = %q, want an inline SVG", tt.state, got)
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

func TestDashboardContent_PollingOnlyWhenSSEDisabled(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	render := func(sse bool) string {
		w := httptest.NewRecorder()
		h.render(w, httptest.NewRequest("GET", "/", nil), "dashboard-content", DashboardData{SSEEnabled: sse})
		return w.Body.String()
	}

	// SSE live: refresh via SSE events, no wall-clock polling.
	withSSE := render(true)
	if !strings.Contains(withSSE, "sse:report-update") {
		t.Error("SSE-enabled dashboard should refresh on SSE events")
	}
	if strings.Contains(withSSE, "every 30s") || strings.Contains(withSSE, "every 10s") {
		t.Error("SSE-enabled dashboard must not also poll on a timer")
	}

	// SSE disabled: polling is the fallback.
	noSSE := render(false)
	if !strings.Contains(noSSE, "every 30s") || !strings.Contains(noSSE, "every 10s") {
		t.Error("SSE-disabled dashboard should fall back to timed polling")
	}
	if strings.Contains(noSSE, "sse:") {
		t.Error("SSE-disabled dashboard should not reference sse: triggers")
	}
}

func TestDetailContent_RefreshTriggerFollowsSSE(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// A processing incident with no root cause is "awaiting diagnosis", so the
	// inner body carries a refresh trigger.
	awaiting := Report{ID: 1, State: "processing"}

	render := func(sse bool) string {
		w := httptest.NewRecorder()
		h.render(w, httptest.NewRequest("GET", "/incidents/1", nil), "detail-content",
			DetailData{Report: awaiting, SSEEnabled: sse})
		return w.Body.String()
	}

	withSSE := render(true)
	if !strings.Contains(withSSE, `hx-trigger="sse:incident-update"`) {
		t.Error("SSE-enabled detail should refresh on incident-update, not poll")
	}
	if strings.Contains(withSSE, "every 5s") {
		t.Error("SSE-enabled detail must not self-poll every 5s")
	}
	// The refresh must select AND target the inner body (so the sse-connect
	// wrapper isn't torn down). Targeting #detail-container instead would
	// silently reintroduce the EventSource-teardown bug this fixes.
	if !strings.Contains(withSSE, `hx-select="#detail-body"`) {
		t.Error("detail refresh should hx-select the inner body to preserve the SSE connection")
	}
	if !strings.Contains(withSSE, `hx-target="#detail-body"`) {
		t.Error("detail refresh must target #detail-body, not the sse-connect #detail-container")
	}

	noSSE := render(false)
	if !strings.Contains(noSSE, "every 5s") {
		t.Error("SSE-disabled detail should fall back to a 5s poll")
	}

	// Once a diagnosis lands the incident is no longer awaiting, so the inner
	// body must carry no refresh trigger at all (the other half of the feature).
	w := httptest.NewRecorder()
	done := Report{ID: 1, State: "reported", RootCause: "OOMKilled: memory limit too low"}
	h.render(w, httptest.NewRequest("GET", "/incidents/1", nil), "detail-content",
		DetailData{Report: done, SSEEnabled: true})
	if body := w.Body.String(); strings.Contains(body, "hx-trigger") {
		t.Errorf("a non-awaiting detail must not refresh; got a trigger in: %s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	req := httptest.NewRequest("GET", "/static/output.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestHandleAcknowledge_MalformedBodyHTMX(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// A present-but-malformed JSON body is a client error (fails before any DB
	// access), surfaced as a toast for htmx rather than a confusing
	// "assignee required".
	req := httptest.NewRequest("POST", "/api/incidents/1/acknowledge", strings.NewReader("{not json"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("HX-Trigger"); !strings.Contains(got, "invalid request body") {
		t.Errorf("HX-Trigger = %q, want the invalid-body message", got)
	}
}

func TestActionResponse_EmitsOOBStatus(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	// The action handlers return "action-response": the re-rendered #action-bar
	// PLUS hx-swap-oob copies of the canonical status badge + stepper, so the
	// header stays in sync after an action (fixes the stale-badge bug).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/incidents/1/acknowledge", nil)
	req.Header.Set("HX-Request", "true")
	rpt := &Report{ID: 1, State: "acknowledged", Severity: "warning", AssignedTo: "alice@example.com"}
	h.render(w, req, "action-response", map[string]any{"Report": rpt})

	body := w.Body.String()
	for _, want := range []string{`id="action-bar"`, `id="incident-status"`, `hx-swap-oob="true"`, `id="incident-stepper"`, "Acknowledged"} {
		if !strings.Contains(body, want) {
			t.Errorf("action-response missing %q; got:\n%s", want, body)
		}
	}
}

func TestNotesAndTimelineSeparated(t *testing.T) {
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	// Notes panel: add-note form + the human notes, its own swap target.
	w := httptest.NewRecorder()
	h.render(w, httptest.NewRequest("GET", "/", nil), "incident-notes", map[string]any{
		"Report": &Report{ID: 1, State: "acknowledged"},
		"Notes":  []Note{{Author: "alice@example.com", Body: "rolled back the config", CreatedAt: time.Now()}},
	})
	notes := w.Body.String()
	for _, want := range []string{`id="incident-notes"`, "/api/incidents/1/notes", "Add a note", "rolled back the config"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes panel missing %q", want)
		}
	}

	// Timeline: lifecycle only — no add-note form, no note POST.
	w2 := httptest.NewRecorder()
	h.render(w2, httptest.NewRequest("GET", "/", nil), "timeline", map[string]any{
		"Timeline": []TimelineEntry{{Type: "created", Actor: "system", Message: "Incident created", Icon: "settings", Time: time.Now()}},
	})
	tl := w2.Body.String()
	if !strings.Contains(tl, `id="incident-timeline"`) || !strings.Contains(tl, "Incident created") {
		t.Error("timeline should render lifecycle entries")
	}
	if strings.Contains(tl, "Add a note") || strings.Contains(tl, "/notes") {
		t.Errorf("timeline must no longer contain the add-note form:\n%s", tl)
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

	timeline := h.buildTimeline(report)

	// Lifecycle only: created, completed, acknowledged, escalated, resolved = 5.
	// Notes are NOT merged in (they render in the separate Notes panel).
	if len(timeline) != 5 {
		t.Fatalf("buildTimeline() returned %d entries, want 5", len(timeline))
	}

	// First entry should be most recent (resolved)
	if timeline[0].Type != "resolved" {
		t.Errorf("first entry type = %q, want 'resolved'", timeline[0].Type)
	}

	// Last entry should be oldest (created)
	if timeline[len(timeline)-1].Type != "created" {
		t.Errorf("last entry type = %q, want 'created'", timeline[len(timeline)-1].Type)
	}

	// No note entries should appear in the lifecycle timeline.
	for _, e := range timeline {
		if e.Type == "note" {
			t.Errorf("timeline should not contain note entries; got one: %q", e.Message)
		}
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

	timeline := h.buildTimeline(report)

	// Should have just: created = 1 entry
	if len(timeline) != 1 {
		t.Fatalf("buildTimeline() returned %d entries, want 1", len(timeline))
	}
	if timeline[0].Type != "created" {
		t.Errorf("entry type = %q, want 'created'", timeline[0].Type)
	}
}
