package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/haakotsm/triage-worker/internal/settings"
)

func newToggleHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := NewHandler(nil, slog.Default())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// GET is rejected (non-htmx → real 405 status).
func TestHandleWorkflowsToggle_MethodNotAllowed(t *testing.T) {
	h := newToggleHandler(t)
	h.SetSettings(settings.New(nil, slog.Default()))

	req := httptest.NewRequest(http.MethodGet, "/api/settings/workflows", nil)
	w := httptest.NewRecorder()
	h.handleWorkflowsToggle(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// With no settings store wired, the toggle reports 503 rather than panicking.
func TestHandleWorkflowsToggle_NilStore(t *testing.T) {
	h := newToggleHandler(t) // settings deliberately left nil

	req := httptest.NewRequest(http.MethodPost, "/api/settings/workflows", strings.NewReader("enabled=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.handleWorkflowsToggle(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// A successful toggle flips the store, renders the dev-controls fragment in its
// new (paused) state, and emits a toast trigger.
func TestHandleWorkflowsToggle_Success(t *testing.T) {
	h := newToggleHandler(t)
	store := settings.New(nil, slog.Default()) // nil db → in-memory only
	h.SetSettings(store)

	req := httptest.NewRequest(http.MethodPost, "/api/settings/workflows", strings.NewReader("enabled=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.handleWorkflowsToggle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if store.WorkflowsEnabled() {
		t.Error("store should be disabled after enabled=false toggle")
	}
	if body := w.Body.String(); !strings.Contains(body, "Resume workflows") {
		t.Errorf("expected paused fragment with a Resume button; got: %s", body)
	}
	if w.Header().Get("HX-Trigger") == "" {
		t.Error("expected HX-Trigger toast header on successful toggle")
	}
}
