package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/haakotsm/triage-worker/internal/types"
)

type fakeGate struct{ enabled bool }

func (g fakeGate) WorkflowsEnabled() bool { return g.enabled }

// When the kill-switch is engaged, firing alerts are skipped without ever
// reaching resolveWorkflowID/signalWithStart. db and temporalClient are nil
// here, so if the guard failed to short-circuit, signalWithStart would panic on
// the nil client — a clean "paused" 200 proves the choke point holds.
func TestHandler_WebhookPausedSkipsFiring(t *testing.T) {
	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		gate:    fakeGate{enabled: false},
	}
	h.healthy.Store(true)

	group := types.AlertGroup{
		Version:  "4",
		GroupKey: "ns/x",
		Status:   "firing",
		Alerts: []types.Alert{
			{Status: "firing", Fingerprint: "f1", Labels: map[string]string{
				"alertname": "KubePodCrashLooping", "namespace": "ns", "deployment": "app",
			}},
		},
	}
	body, _ := json.Marshal(group)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "paused" {
		t.Errorf("status = %v, want %q", resp["status"], "paused")
	}
	if resp["skipped"] != float64(1) {
		t.Errorf("skipped = %v, want 1", resp["skipped"])
	}
}

// While paused, resolved alerts in a mixed group MUST still be applied so open
// incidents close and the DB stays consistent — only NEW workflow starts are
// suppressed. The sqlmock expects exactly the resolve UPDATE and nothing for
// the firing alert.
func TestHandler_WebhookPausedStillResolves(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
		gate:    fakeGate{enabled: false},
	}
	h.healthy.Store(true)

	group := types.AlertGroup{
		Version:  "4",
		GroupKey: "ns/x",
		Status:   "firing",
		Alerts: []types.Alert{
			{Status: "resolved", Fingerprint: "r1", Labels: map[string]string{
				"alertname": "OldAlert", "namespace": "ns", "deployment": "app",
			}},
			{Status: "firing", Fingerprint: "f1", Labels: map[string]string{
				"alertname": "NewAlert", "namespace": "ns", "deployment": "app",
			}},
		},
	}
	rid := types.DeriveIdentity(group.Alerts[0].Labels)
	expectStemLock(mock, rid.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(rid.Namespace, rid.Kind, rid.Name, rid.AlertName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(group)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "paused" {
		t.Errorf("status = %v, want %q", resp["status"], "paused")
	}
	// The resolve UPDATE must have run (and nothing for the firing alert).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("resolves must still apply while paused: %v", err)
	}
}

// While paused, a FAILED resolve in a mixed group must still surface as a 500
// so Alertmanager retries — a transient DB blip must not silently drop the
// resolution and leave the incident open. This is the alert-loss-prevention
// path the kill-switch must preserve.
func TestHandler_WebhookPausedResolveErrorReturns500(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	h := &Handler{
		logger:  newTestLogger(),
		healthy: &atomic.Bool{},
		db:      db,
		gate:    fakeGate{enabled: false},
	}
	h.healthy.Store(true)

	group := types.AlertGroup{
		Version:  "4",
		GroupKey: "ns/x",
		Status:   "firing",
		Alerts: []types.Alert{
			{Status: "resolved", Fingerprint: "r1", Labels: map[string]string{
				"alertname": "OldAlert", "namespace": "ns", "deployment": "app",
			}},
			{Status: "firing", Fingerprint: "f1", Labels: map[string]string{
				"alertname": "NewAlert", "namespace": "ns", "deployment": "app",
			}},
		},
	}
	rid := types.DeriveIdentity(group.Alerts[0].Labels)
	expectStemLock(mock, rid.WorkflowID())
	mock.ExpectExec(regexp.QuoteMeta(resolvedUpdateSQL)).
		WithArgs(rid.Namespace, rid.Kind, rid.Name, rid.AlertName).
		WillReturnError(fmt.Errorf("connection refused"))
	mock.ExpectRollback()

	body, _ := json.Marshal(group)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (resolve error must surface while paused); body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
