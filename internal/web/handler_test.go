package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
