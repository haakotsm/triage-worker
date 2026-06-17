package webhook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/haakotsm/triage-worker/internal/types"
)

const (
	advisoryLockSQL = `SELECT pg_advisory_xact_lock($1, hashtext($2))`

	openIncidentSQL = `SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state != 'resolved'
		   AND (state != 'processing' OR created_at > NOW() - make_interval(secs => $5))
		 ORDER BY created_at DESC
		 LIMIT 1`

	flapGuardSQL = `SELECT EXISTS (
		   SELECT 1 FROM triage.reports
		   WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		     AND state = 'resolved' AND resolved_at > NOW() - make_interval(secs => $5)
		 )`

	attemptHistorySQL = `SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		 ORDER BY created_at DESC
		 LIMIT 1`

	preliminaryInsertSQL = `INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
		 VALUES ($1, $2, $3, $4, $5, 'processing')
		 ON CONFLICT (workflow_id) DO NOTHING`

	resolvedUpdateSQL = `UPDATE triage.reports SET resolved_at = NOW(), state = 'resolved'
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state != 'resolved'`
)

// expectStemLock sets up the Begin + advisory-lock expectation pair that
// opens every transactional code path in handler.go.
func expectStemLock(mock sqlmock.Sqlmock, stem string) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(advisoryLockSQL)).
		WithArgs(advisoryLockKey1, stem).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

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

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
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
	identity := types.DeriveIdentity(group.Alerts[0].Labels)

	expectStemLock(mock, identity.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 1 || errored != 0 {
		t.Errorf("handleResolvedAlerts() = (%d, %d), want (1, 0)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_NoDBSkips(t *testing.T) {
	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: nil}
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Labels: map[string]string{"alertname": "Test", "namespace": "ns"}},
		},
	}

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 || errored != 0 {
		t.Errorf("handleResolvedAlerts() without DB = (%d, %d), want (0, 0)", updated, errored)
	}
}

func TestHandleResolvedAlerts_SkipsFiringAlerts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}

	// Mixed group: only resolved alerts should trigger any SQL
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

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 || errored != 0 {
		t.Errorf("handleResolvedAlerts() with only firing = (%d, %d), want (0, 0)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleResolvedAlerts_DeduplicatesStems(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}

	// Two resolved alerts that derive to the same identity stem must produce
	// exactly one transaction — not two.
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
	identity := types.DeriveIdentity(group.Alerts[0].Labels)

	expectStemLock(mock, identity.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 1 || errored != 0 {
		t.Errorf("handleResolvedAlerts() = (%d, %d), want (1, 0) (deduplicated)", updated, errored)
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

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
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

	expectStemLock(mock, identity.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 || errored != 0 {
		t.Errorf("handleResolvedAlerts() = (%d, %d), want (0, 0) (no matching row)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// Two resolved alerts with different alertnames produce two distinct stems
// → two independent locked transactions.
func TestHandleResolvedAlerts_MultipleDistinctWorkflows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
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

	// Map iteration is non-deterministic — match in any order.
	mock.MatchExpectationsInOrder(false)
	for _, id := range []types.IncidentIdentity{id0, id1} {
		expectStemLock(mock, id.WorkflowID())
		mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
			WithArgs(id.Namespace, id.Kind, id.Name, id.AlertName).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 2 || errored != 0 {
		t.Errorf("handleResolvedAlerts() = (%d, %d), want (2, 0)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// A DB error during one stem's UPDATE must surface via errored>0 so the
// HTTP handler returns 500. Sibling stems still get processed.
func TestHandleResolvedAlerts_DBErrorIsReported(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Labels: map[string]string{"alertname": "Err", "namespace": "ns", "deployment": "app"}},
			{Status: "resolved", Labels: map[string]string{"alertname": "OK", "namespace": "ns", "deployment": "app"}},
		},
	}
	id0 := types.DeriveIdentity(group.Alerts[0].Labels)
	id1 := types.DeriveIdentity(group.Alerts[1].Labels)

	mock.MatchExpectationsInOrder(false)
	expectStemLock(mock, id0.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(id0.Namespace, id0.Kind, id0.Name, id0.AlertName).
		WillReturnError(fmt.Errorf("connection refused"))
	mock.ExpectRollback()

	expectStemLock(mock, id1.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(id1.Namespace, id1.Kind, id1.Name, id1.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 1 || errored != 1 {
		t.Errorf("handleResolvedAlerts() = (%d, %d), want (1, 1)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// --- Tests for resolveWorkflowID: open dedup, flap guard, attempt mint ---

func testIdentity() types.IncidentIdentity {
	return types.DeriveIdentity(map[string]string{
		"alertname":  "KubePodCrashLooping",
		"namespace":  "production",
		"deployment": "api-server",
	})
}

// expectOpenLookup queues the open-incident SELECT expectation. The TTL
// argument is passed to Postgres make_interval(secs => $5) as a float
// seconds value — kept in lockstep with the production code via
// stuckProcessingTTL.Seconds().
func expectOpenLookup(mock sqlmock.Sqlmock, id types.IncidentIdentity) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(regexp.QuoteMeta(openIncidentSQL)).
		WithArgs(id.Namespace, id.Kind, id.Name, id.AlertName, stuckProcessingTTL.Seconds())
}

func expectFlapGuard(mock sqlmock.Sqlmock, id types.IncidentIdentity) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(regexp.QuoteMeta(flapGuardSQL)).
		WithArgs(id.Namespace, id.Kind, id.Name, id.AlertName, flapWindow.Seconds())
}

func expectAttemptHistory(mock sqlmock.Sqlmock, id types.IncidentIdentity) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(regexp.QuoteMeta(attemptHistorySQL)).
		WithArgs(id.Namespace, id.Kind, id.Name, id.AlertName)
}

func expectPreliminaryInsert(mock sqlmock.Sqlmock, wfID string, id types.IncidentIdentity) *sqlmock.ExpectedExec {
	return mock.ExpectExec(regexp.QuoteMeta(preliminaryInsertSQL)).
		WithArgs(wfID, id.Namespace, id.Name, id.Kind, id.AlertName)
}

// Re-fire while the previous attempt is still open: the resolver finds the
// open row and returns its workflow_id without consulting the flap guard
// or minting a new attempt.
func TestResolveWorkflowID_ReusesOpenIncident(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()
	openWfID := identity.WorkflowID()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id"}).AddRow(openWfID))
	mock.ExpectCommit()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if got != openWfID {
		t.Errorf("workflow_id = %q, want %q (reuse open)", got, openWfID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Re-fire after the previous attempt is resolved but inside the flap window:
// the resolver returns ErrSkipFlap so the caller drops the alert silently.
func TestResolveWorkflowID_FlapGuardSuppresses(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectCommit()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if !stderrors.Is(err, ErrSkipFlap) {
		t.Fatalf("err = %v, want ErrSkipFlap", err)
	}
	if got != "" {
		t.Errorf("workflow_id = %q, want empty (skip)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Re-fire after the previous attempt is resolved and outside the flap
// window: the resolver scans history for the latest attempt and mints
// stem#N+1 with a preliminary INSERT.
func TestResolveWorkflowID_MintsNextAttemptAfterResolution(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()
	stem := identity.WorkflowID()
	wantWfID := stem + "#2"

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id"}).AddRow(stem))
	expectPreliminaryInsert(mock, wantWfID, identity).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if got != wantWfID {
		t.Errorf("workflow_id = %q, want %q (next attempt)", got, wantWfID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// First-ever fire for an identity: history scan returns ErrNoRows, mint the
// unsuffixed stem so legacy rows round-trip as attempt 1.
func TestResolveWorkflowID_FirstAttemptUsesUnsuffixedStem(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()
	stem := identity.WorkflowID()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).WillReturnError(sql.ErrNoRows)
	expectPreliminaryInsert(mock, stem, identity).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if got != stem {
		t.Errorf("workflow_id = %q, want %q (unsuffixed stem)", got, stem)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// History contains a row at attempt 2 (stem#2). The latest-row scan returns
// stem#2; the resolver must mint stem#3, not blindly retry #2 (which would
// silently no-op via ON CONFLICT and lose the alert).
func TestResolveWorkflowID_PicksMaxAttemptPlusOne(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()
	stem := identity.WorkflowID()
	wantWfID := stem + "#3"

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id"}).AddRow(stem + "#2"))
	expectPreliminaryInsert(mock, wantWfID, identity).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if got != wantWfID {
		t.Errorf("workflow_id = %q, want %q (next attempt)", got, wantWfID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The preliminary INSERT silently no-op'd via ON CONFLICT — surface as an
// error rather than returning a wfID that may attach to a closed workflow.
// This is the load-bearing silent-failure prevention check.
func TestResolveWorkflowID_PreliminaryInsertConflictIsError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()
	stem := identity.WorkflowID()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).WillReturnError(sql.ErrNoRows)
	// Conflict: row already exists for stem (e.g. stale row outside lock path).
	expectPreliminaryInsert(mock, stem, identity).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil {
		t.Fatalf("resolveWorkflowID: got nil err, want conflict error")
	}
	if !strings.Contains(err.Error(), "preliminary insert conflict") {
		t.Errorf("err = %v, want preliminary insert conflict error", err)
	}
	if got != "" {
		t.Errorf("workflow_id = %q, want empty on error", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The stuckProcessingTTL value must be passed to make_interval(secs => $5)
// as the duration's Seconds() — not the bare time.Duration. expectOpenLookup
// pins this via WithArgs, so a regression that switches back to
// duration.String() (which would silently mis-parse on Postgres) trips this
// test. The TTL predicate's semantics themselves require a real Postgres to
// verify — see the testcontainers follow-up.
func TestResolveWorkflowID_PassesTTLArgAsSeconds(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	// Pin: the open-incident SELECT receives stuckProcessingTTL.Seconds()
	// as the 5th arg (the make_interval(secs => $5) parameter).
	mock.ExpectQuery(regexp.QuoteMeta(openIncidentSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName, stuckProcessingTTL.Seconds()).
		WillReturnError(sql.ErrNoRows)
	// And the flap-guard receives flapWindow.Seconds().
	mock.ExpectQuery(regexp.QuoteMeta(flapGuardSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName, flapWindow.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).WillReturnError(sql.ErrNoRows)
	expectPreliminaryInsert(mock, identity.WorkflowID(), identity).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if _, err := h.resolveWorkflowID(t.Context(), identity); err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// BeginTx failure must propagate so the caller returns 500.
func TestResolveWorkflowID_BeginTxError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	mock.ExpectBegin().WillReturnError(fmt.Errorf("conn refused"))

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil {
		t.Fatal("resolveWorkflowID: got nil err, want begin tx error")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %v, want begin-tx error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Commit failure on the open-row reuse path is a real error.
func TestResolveWorkflowID_CommitError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id"}).AddRow(identity.WorkflowID()))
	mock.ExpectCommit().WillReturnError(fmt.Errorf("commit failed"))

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil {
		t.Fatal("resolveWorkflowID: got nil err, want commit error")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v, want commit error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Advisory-lock Exec failure must propagate; nothing else should run.
func TestResolveWorkflowID_AdvisoryLockError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(advisoryLockSQL)).
		WithArgs(advisoryLockKey1, identity.WorkflowID()).
		WillReturnError(fmt.Errorf("lock acquisition failed"))
	mock.ExpectRollback()

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil || !strings.Contains(err.Error(), "advisory lock") {
		t.Errorf("err = %v, want advisory-lock error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A non-ErrNoRows DB error on the open-incident lookup must NOT be treated
// as "no row found" — that would let the resolver fall through to the mint
// path and double-create attempts during a transient DB blip.
func TestResolveWorkflowID_OpenLookupNonErrNoRowsError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(fmt.Errorf("connection reset"))
	mock.ExpectRollback()

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil || !strings.Contains(err.Error(), "open-incident lookup") {
		t.Errorf("err = %v, want open-incident-lookup error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Flap-guard query failure propagates as an error rather than silently
// allowing the alert through (the previous fail-open behaviour was removed
// in the rework).
func TestResolveWorkflowID_FlapGuardError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).WillReturnError(fmt.Errorf("query timeout"))
	mock.ExpectRollback()

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil || !strings.Contains(err.Error(), "flap-guard") {
		t.Errorf("err = %v, want flap-guard error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A non-ErrNoRows error on the attempt-history scan must surface so we
// don't mint attempt 1 on top of an existing (but unreadable) row.
func TestResolveWorkflowID_AttemptHistoryNonErrNoRowsError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	identity := testIdentity()

	expectStemLock(mock, identity.WorkflowID())
	expectOpenLookup(mock, identity).WillReturnError(sql.ErrNoRows)
	expectFlapGuard(mock, identity).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectAttemptHistory(mock, identity).WillReturnError(fmt.Errorf("disk read error"))
	mock.ExpectRollback()

	_, err := h.resolveWorkflowID(t.Context(), identity)
	if err == nil || !strings.Contains(err.Error(), "attempt-history lookup") {
		t.Errorf("err = %v, want attempt-history-lookup error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// resolveByStem's BeginTx error must surface as errored>0 so the HTTP
// handler returns 500. Sibling stems still run.
func TestHandleResolvedAlerts_ResolveByStem_BeginTxError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Labels: map[string]string{"alertname": "A", "namespace": "ns", "deployment": "app"}},
			{Status: "resolved", Labels: map[string]string{"alertname": "B", "namespace": "ns", "deployment": "app"}},
		},
	}
	idOK := types.DeriveIdentity(group.Alerts[1].Labels)

	// Map iteration is non-deterministic — one stem's Begin fails, the
	// other completes normally.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectBegin().WillReturnError(fmt.Errorf("conn pool exhausted"))

	expectStemLock(mock, idOK.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(idOK.Namespace, idOK.Kind, idOK.Name, idOK.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if errored != 1 {
		t.Errorf("errored = %d, want 1", errored)
	}
	// updated could be 0 or 1 depending on which stem hit which expectation,
	// but the failure tally must reach 1.
	if updated > 1 {
		t.Errorf("updated = %d, want <= 1", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// resolveByStem's Commit failure must propagate.
func TestHandleResolvedAlerts_ResolveByStem_CommitError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: db}
	group := types.AlertGroup{
		Status: "resolved",
		Alerts: []types.Alert{
			{Status: "resolved", Labels: map[string]string{"alertname": "X", "namespace": "ns", "deployment": "app"}},
		},
	}
	identity := types.DeriveIdentity(group.Alerts[0].Labels)

	expectStemLock(mock, identity.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(identity.Namespace, identity.Kind, identity.Name, identity.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(fmt.Errorf("commit lost"))

	updated, errored := h.handleResolvedAlerts(t.Context(), group)
	if updated != 0 || errored != 1 {
		t.Errorf("got (%d, %d), want (0, 1)", updated, errored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Without a DB the resolver returns the unsuffixed stem so the webhook still
// works as a thin Temporal trigger in in-memory deployments.
func TestResolveWorkflowID_NoDBFallback(t *testing.T) {
	h := &Handler{logger: newTestLogger(), healthy: &atomic.Bool{}, db: nil}
	identity := testIdentity()
	got, err := h.resolveWorkflowID(t.Context(), identity)
	if err != nil {
		t.Fatalf("resolveWorkflowID: %v", err)
	}
	if got != identity.WorkflowID() {
		t.Errorf("workflow_id = %q, want %q (db-less fallback)", got, identity.WorkflowID())
	}
}
