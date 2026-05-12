package webhook

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/haakotsm/triage-worker/internal/types"
)

// mockTemporalClient implements the minimal interface needed for webhook tests.
// The real client.Client interface is too large to mock fully, so we test
// the handler's routing, validation, and health logic at the HTTP level.

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestHandler_HealthzAlwaysOK(t *testing.T) {
	// We can't easily create a nil-safe Handler without a real client,
	// so test routing at a higher level using a struct with nil client.
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", w.Body.String(), "ok")
	}
}

func TestHandler_ReadyzHealthy(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandler_ReadyzUnhealthy(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(false)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandler_NotFound(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandler_WebhookMethodNotAllowed(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /webhook status = %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestHandler_WebhookUnhealthy(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(false)

	body := `{"version":"4","groupKey":"test","alerts":[]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandler_WebhookInvalidJSON(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_WebhookResolvedOnly(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	alertGroup := types.AlertGroup{
		Version:  "4",
		GroupKey: "test",
		Status:   "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Fingerprint: "abc", Labels: map[string]string{"alertname": "Test"}},
		},
	}

	body, _ := json.Marshal(alertGroup)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Without a DB, no reports are updated but response should indicate resolution was processed
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "resolved" {
		t.Errorf("response status = %q, want %q", resp["status"], "resolved")
	}
}

func TestHandler_WebhookMissingContentType(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnsupportedMediaType)
	}
}

func TestHandler_WebhookAuthRequired(t *testing.T) {
	h := &Handler{
		logger:        newTestLogger(),
		healthy:       &atomic.Bool{},
		webhookSecret: "test-secret-token",
	}
	h.healthy.Store(true)

	// No auth header — should be rejected
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	// Wrong token
	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	// Correct token — should pass auth (will fail on JSON parse, which is expected)
	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("correct token: status = %d, want %d (bad JSON)", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_WebhookAuthSkippedWhenNoSecret(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		// webhookSecret is empty — auth should be skipped
	}
	h.healthy.Store(true)

	alertGroup := types.AlertGroup{
		Version: "4",
		Status:  "resolved",
		Alerts:  []types.Alert{{Status: "resolved", Fingerprint: "x", Labels: map[string]string{"alertname": "Test"}}},
	}
	body, _ := json.Marshal(alertGroup)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (auth should be skipped)", w.Code, http.StatusOK)
	}
}

func TestHandler_SetHealthy(t *testing.T) {
	healthy := &atomic.Bool{}
	h := &Handler{
		logger:  newTestLogger(),
		healthy: healthy,
	}

	h.SetHealthy(true)
	if !healthy.Load() {
		t.Error("healthy should be true")
	}

	h.SetHealthy(false)
	if healthy.Load() {
		t.Error("healthy should be false")
	}
}

// Test that FiringAlerts filtering works correctly with the webhook
func TestAlertGroupFiltering(t *testing.T) {
	now := time.Now()

	alertGroup := types.AlertGroup{
		Version:  "4",
		GroupKey: "ns/alerts",
		Status:   "firing",
		Alerts: []types.Alert{
			{Status: "firing", Fingerprint: "f1", Labels: map[string]string{
				"alertname": "KubePodCrashLooping", "namespace": "default", "deployment": "app",
			}, StartsAt: now},
			{Status: "resolved", Fingerprint: "f2", Labels: map[string]string{
				"alertname": "KubeOOMKilled", "namespace": "default", "deployment": "app",
			}, StartsAt: now},
			{Status: "firing", Fingerprint: "f3", Labels: map[string]string{
				"alertname": "KubePodNotReady", "namespace": "monitoring", "statefulset": "prometheus",
			}, StartsAt: now},
		},
	}

	firing := alertGroup.FiringAlerts()
	if len(firing) != 2 {
		t.Fatalf("FiringAlerts() = %d, want 2", len(firing))
	}

	// Group by workflow ID
	alertsByWorkflow := make(map[string][]types.Alert)
	for _, alert := range firing {
		id := types.DeriveIdentity(alert.Labels)
		wfID := id.WorkflowID()
		alertsByWorkflow[wfID] = append(alertsByWorkflow[wfID], alert)
	}

	// Should be 2 distinct workflows (different namespace/kind/name)
	if len(alertsByWorkflow) != 2 {
		t.Errorf("distinct workflows = %d, want 2", len(alertsByWorkflow))
	}
}

// --- DB-backed integration tests for handleResolvedAlerts ---

func TestHandleResolvedAlerts_UpdatesMatchingReport(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
	}

	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{
				Status: "resolved",
				Labels: map[string]string{
					"alertname":  "KubePodCrashLooping",
					"namespace":  "production",
					"deployment": "api-server",
				},
			},
		},
	}

	// Derive the expected workflow ID
	identity := types.DeriveIdentity(group.Alerts[0].Labels)
	expectedWfID := identity.WorkflowID()

	// Expect the UPDATE query with the correct workflow_id
	mock.ExpectExec("UPDATE triage.reports SET resolved_at").
		WithArgs(expectedWfID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	updated := h.handleResolvedAlerts(t.Context(), group)

	if updated != 1 {
		t.Errorf("handleResolvedAlerts() = %d, want 1", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_NoDBSkips(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      nil,
	}

	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Labels: map[string]string{"alertname": "Test", "namespace": "ns"}},
		},
	}

	updated := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 {
		t.Errorf("handleResolvedAlerts() without DB = %d, want 0", updated)
	}
}

func TestHandleResolvedAlerts_SkipsFiringAlerts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
	}

	// Mixed group: only resolved alerts should trigger UPDATE
	group := types.AlertGroup{
		Status: "firing",
		Alerts: []types.Alert{
			{
				Status: "firing",
				Labels: map[string]string{"alertname": "A", "namespace": "ns", "deployment": "app"},
			},
			{
				Status: "firing",
				Labels: map[string]string{"alertname": "B", "namespace": "ns", "deployment": "app"},
			},
		},
	}

	// No SQL should be executed — all alerts are firing
	updated := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 {
		t.Errorf("handleResolvedAlerts() with only firing = %d, want 0", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_DeduplicatesWorkflowIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
	}

	// Two resolved alerts with same alertname/namespace/deployment → same workflow ID
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{
				Status:      "resolved",
				Fingerprint: "f1",
				Labels:      map[string]string{"alertname": "KubePodCrashLooping", "namespace": "ns", "deployment": "app"},
			},
			{
				Status:      "resolved",
				Fingerprint: "f2",
				Labels:      map[string]string{"alertname": "KubePodCrashLooping", "namespace": "ns", "deployment": "app"},
			},
		},
	}

	// Should only execute ONE update (deduplicated by workflow ID)
	identity := types.DeriveIdentity(group.Alerts[0].Labels)
	expectedWfID := identity.WorkflowID()

	mock.ExpectExec("UPDATE triage.reports SET resolved_at").
		WithArgs(expectedWfID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	updated := h.handleResolvedAlerts(t.Context(), group)
	if updated != 1 {
		t.Errorf("handleResolvedAlerts() = %d, want 1 (deduplicated)", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_NoRowsAffected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
	}

	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{
				Status: "resolved",
				Labels: map[string]string{"alertname": "X", "namespace": "ns", "deployment": "ghost"},
			},
		},
	}

	identity := types.DeriveIdentity(group.Alerts[0].Labels)
	expectedWfID := identity.WorkflowID()

	// Report doesn't exist or already resolved — 0 rows affected
	mock.ExpectExec("UPDATE triage.reports SET resolved_at").
		WithArgs(expectedWfID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	updated := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 {
		t.Errorf("handleResolvedAlerts() = %d, want 0 (no matching report)", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_MultipleDistinctWorkflows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
	}

	// Two alerts with different alertnames → different workflow IDs → two UPDATEs
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{
				Status: "resolved",
				Labels: map[string]string{"alertname": "CrashLoop", "namespace": "ns", "deployment": "app"},
			},
			{
				Status: "resolved",
				Labels: map[string]string{"alertname": "OOMKilled", "namespace": "ns", "deployment": "app"},
			},
		},
	}

	id0 := types.DeriveIdentity(group.Alerts[0].Labels)
	id1 := types.DeriveIdentity(group.Alerts[1].Labels)

	// Expect two separate UPDATEs (order depends on map iteration — use AnyArg)
	mock.ExpectExec("UPDATE triage.reports SET resolved_at").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE triage.reports SET resolved_at").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	updated := h.handleResolvedAlerts(t.Context(), group)
	if updated != 2 {
		t.Errorf("handleResolvedAlerts() = %d, want 2", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}

	// Verify the two workflow IDs are indeed different
	if id0.WorkflowID() == id1.WorkflowID() {
		t.Errorf("expected different workflow IDs, both = %q", id0.WorkflowID())
	}
}
