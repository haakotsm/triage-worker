package webhook

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/haakotsm/triage-worker/internal/metrics"
	"github.com/haakotsm/triage-worker/internal/types"
	"github.com/haakotsm/triage-worker/internal/workflow"
)

// ErrSkipFlap is returned by resolveWorkflowID when the prior incident for
// the identity was resolved within the flap window. The caller should drop
// the alert silently — it is not an error condition.
var ErrSkipFlap = stderrors.New("alert skipped: within flap window after recent resolution")

const maxBodySize = 1 << 20 // 1 MB

// WorkflowGate decides whether the webhook may start new triage workflows. A
// nil gate (or one returning true) means workflows run normally; a gate
// returning false pauses new workflow starts — the development-time control for
// kagent/LLM token spend. Resolved alerts are still processed while paused so
// open incidents close cleanly.
type WorkflowGate interface {
	WorkflowsEnabled() bool
}

// Handler handles Alertmanager webhook requests and starts/signals Temporal workflows.
type Handler struct {
	temporalClient client.Client
	taskQueue      string
	logger         *slog.Logger
	healthy        *atomic.Bool
	webhookSecret  string
	apiHandler     http.Handler
	webHandler     http.Handler
	db             *sql.DB
	gate           WorkflowGate
}

// SetWorkflowGate attaches the runtime kill-switch consulted before starting
// workflows. Leaving it unset is treated as always-enabled.
func (h *Handler) SetWorkflowGate(g WorkflowGate) {
	h.gate = g
}

// NewHandler creates a new webhook handler.
// If webhookSecret is non-empty, Bearer token authentication is required on /webhook.
// If apiHandler is non-nil, requests to /api/ are delegated to it.
// If db is non-nil, resolved alerts will update resolved_at on matching reports.
func NewHandler(tc client.Client, taskQueue string, logger *slog.Logger, webhookSecret string, apiHandler http.Handler, webHandler http.Handler, db *sql.DB) *Handler {
	healthy := &atomic.Bool{}
	healthy.Store(true)
	if webhookSecret == "" {
		logger.Warn("WEBHOOK_SECRET not set — webhook endpoint is unauthenticated")
	}
	return &Handler{
		temporalClient: tc,
		taskQueue:      taskQueue,
		logger:         logger,
		healthy:        healthy,
		webhookSecret:  webhookSecret,
		apiHandler:     apiHandler,
		webHandler:     webHandler,
		db:             db,
	}
}

// SetHealthy updates the health status (used for readiness probe).
func (h *Handler) SetHealthy(healthy bool) {
	h.healthy.Store(healthy)
}

// ServeHTTP routes requests to the appropriate handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/webhook":
		h.handleWebhook(w, r)
	case r.URL.Path == "/healthz":
		h.handleHealthz(w, r)
	case r.URL.Path == "/readyz":
		h.handleReadyz(w, r)
	case (strings.HasPrefix(r.URL.Path, "/api/incidents/") || strings.HasPrefix(r.URL.Path, "/api/settings/")) && h.webHandler != nil:
		h.webHandler.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/") && h.apiHandler != nil:
		h.apiHandler.ServeHTTP(w, r)
	default:
		if h.webHandler != nil {
			h.webHandler.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	}
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// result is recorded (with the total handling latency) on every return path
	// via the deferred recorder. It defaults to "error"; each terminal path
	// overwrites it with the outcome it actually produced.
	start := time.Now()
	result := metrics.ResultError
	defer func() { metrics.RecordWebhookRequest(result, time.Since(start)) }()

	if r.Method != http.MethodPost {
		result = metrics.ResultRejected
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate — require Bearer token if secret is configured
	if h.webhookSecret != "" {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if auth == token || subtle.ConstantTimeCompare([]byte(token), []byte(h.webhookSecret)) != 1 {
			result = metrics.ResultRejected
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Check health — reject if Temporal is unreachable
	if !h.healthy.Load() {
		http.Error(w, "service unavailable: Temporal unreachable", http.StatusServiceUnavailable)
		return
	}

	// Validate Content-Type
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		result = metrics.ResultRejected
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Read and validate body
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		result = metrics.ResultRejected
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var alertGroup types.AlertGroup
	if err := json.Unmarshal(body, &alertGroup); err != nil {
		result = metrics.ResultRejected
		h.logger.Error("invalid alertmanager payload", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Handle resolved alerts — mark matching reports as resolved. A DB error
	// surfaces as a 500 so Alertmanager retries; otherwise a resolution lost
	// to a transient DB blip would leave the row stuck in 'processing'
	// forever and block every future re-fire from minting a new attempt.
	firing := alertGroup.FiringAlerts()
	if len(firing) == 0 {
		updated, errored := h.handleResolvedAlerts(ctx, alertGroup)
		if errored > 0 {
			http.Error(w, fmt.Sprintf("resolve failed for %d identities", errored), http.StatusInternalServerError)
			return
		}
		h.logger.Debug("processed resolved-only alert group",
			"group_key", alertGroup.GroupKey,
			"resolved_count", updated,
		)
		result = metrics.ResultResolved
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"resolved","reports_updated":%d}`, updated)
		return
	}

	// Mixed group (firing + resolved): apply resolves first so the open-row
	// lookup below sees the correct lifecycle state.
	var errors []string
	if alertGroup.Status == "firing" {
		updated, errored := h.handleResolvedAlerts(ctx, alertGroup)
		if updated > 0 {
			h.logger.Info("resolved reports in mixed group", "resolved_count", updated)
		}
		if errored > 0 {
			errors = append(errors, fmt.Sprintf("resolve failed for %d identities", errored))
		}
	}

	// Kill-switch: when triage workflows are paused from the dashboard, the
	// resolves above still apply (so open incidents close and the DB stays
	// consistent), but we start no new triage workflows — and crucially do not
	// mint preliminary 'processing' rows via resolveWorkflowID. This is the
	// development-time control for kagent/LLM token spend.
	if h.gate != nil && !h.gate.WorkflowsEnabled() {
		if len(errors) > 0 {
			// A resolve in this mixed group failed — surface it so Alertmanager
			// retries rather than silently dropping the resolution.
			http.Error(w, fmt.Sprintf("partial failure: %v", errors), http.StatusInternalServerError)
			return
		}
		h.logger.Info("triage workflows paused — skipping firing alerts (kill-switch active)",
			"group_key", alertGroup.GroupKey,
			"firing", len(firing),
		)
		result = metrics.ResultPaused
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"status":  "paused",
			"skipped": len(firing),
			"alerts":  len(firing),
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			h.logger.Warn("failed to write response", "error", err)
		}
		return
	}

	// Group firing alerts by identity stem. The stem is shared across all
	// attempts at a given (namespace, kind, workload, alert_name); the
	// concrete workflow_id (which may carry an attempt suffix) is resolved
	// per stem below, after we know whether an open incident already exists.
	alertsByStem := make(map[string][]types.Alert)
	identitiesByStem := make(map[string]types.IncidentIdentity)

	for _, alert := range firing {
		identity := types.DeriveIdentity(alert.Labels)
		stem := identity.WorkflowID()
		alertsByStem[stem] = append(alertsByStem[stem], alert)
		identitiesByStem[stem] = identity
	}

	workflowsSignaled := 0
	flapsSkipped := 0

	for stem, alerts := range alertsByStem {
		identity := identitiesByStem[stem]

		wfID, err := h.resolveWorkflowID(ctx, identity)
		switch {
		case stderrors.Is(err, ErrSkipFlap):
			h.logger.Info("dedup: skipping re-fire within flap window",
				"stem", stem,
				"namespace", identity.Namespace,
				"workload", identity.Name,
			)
			flapsSkipped++
			metrics.RecordWebhookDecision(metrics.DecisionFlapSkipped)
			continue
		case err != nil:
			h.logger.Error("resolve workflow id failed",
				"stem", stem,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("%s: %v", stem, err))
			metrics.RecordWebhookDecision(metrics.DecisionResolveError)
			continue
		}

		if err := h.signalWithStart(ctx, wfID, identity, alerts); err != nil {
			h.logger.Error("signal-with-start failed",
				"workflow_id", wfID,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("%s: %v", wfID, err))
			metrics.RecordWebhookDecision(metrics.DecisionSignalError)
			continue
		}
		workflowsSignaled++
		metrics.RecordWebhookDecision(metrics.DecisionSignaled)
	}

	if len(errors) > 0 {
		// Return 500 so Alertmanager retries
		http.Error(w, fmt.Sprintf("partial failure: %v", errors), http.StatusInternalServerError)
		return
	}

	result = metrics.ResultAccepted
	w.WriteHeader(http.StatusOK)
	resp := map[string]interface{}{
		"status":    "accepted",
		"workflows": workflowsSignaled,
		"skipped":   flapsSkipped,
		"alerts":    len(firing),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("failed to write response", "error", err)
	}
}

// flapWindow is how long after an incident resolves we suppress new firings
// for the same identity as alert noise. Tuned high enough to absorb typical
// Alertmanager retry intervals, low enough that genuine same-day recurrences
// are not lost.
const flapWindow = 5 * time.Minute

// workflowExecutionTimeout caps how long a single triage Temporal workflow
// may run. Used by signalWithStart and — derived from the same constant —
// by stuckProcessingTTL so the two values can't drift independently.
const workflowExecutionTimeout = 15 * time.Minute

// stuckProcessingTTL is how long a row may sit in state='processing' before
// the open-incident lookup treats it as orphaned (signalWithStart failed,
// Alertmanager gave up retrying) and lets a new attempt take over. Bound to
// workflowExecutionTimeout so a Temporal-side timeout and a DB-side bypass
// fire at the same moment.
const stuckProcessingTTL = workflowExecutionTimeout

// resolveWorkflowID picks the workflow_id that a firing alert for this
// identity should signal, and writes the preliminary report row in the same
// transaction so the lifecycle dashboard sees the incident as soon as the
// webhook returns. The whole sequence runs under a per-stem advisory lock so
// concurrent webhook calls can't race on the answer.
//
// Selection rules, applied in order:
//
//  1. If an open (state != 'resolved') row already exists for this identity
//     and is not a stuck-processing orphan, reuse its workflow_id. Re-fires
//     of an in-progress incident dedupe into the same Temporal execution.
//  2. Otherwise, if the most recently-resolved row finished within
//     flapWindow, return ErrSkipFlap. The caller drops the alert.
//  3. Otherwise mint a new workflow_id by appending an incremented attempt
//     counter to the identity stem. Attempt 1 uses the unsuffixed stem so
//     every legacy row continues to round-trip.
//
// The transaction holds the per-stem advisory lock from before the open-row
// lookup all the way through to the INSERT, so the resolve path (which takes
// the same lock in handleResolvedAlerts) cannot interleave between "saw the
// row as open" and "wrote the preliminary INSERT."
//
// When no DB is configured the function falls back to the unsuffixed stem so
// the webhook still works as a thin Temporal trigger in tests and in-memory
// deployments.
func (h *Handler) resolveWorkflowID(ctx context.Context, identity types.IncidentIdentity) (string, error) {
	if h.db == nil {
		return identity.WorkflowID(), nil
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer h.rollbackOnReturn(tx, identity.WorkflowID())

	if err := h.acquireStemLock(ctx, tx, identity); err != nil {
		return "", err
	}

	// Reuse the open incident's workflow_id if one exists. A row stuck in
	// 'processing' past stuckProcessingTTL is treated as orphaned — typically
	// signalWithStart failed and Alertmanager has stopped retrying — so a
	// new attempt is allowed to supersede it rather than jam the identity
	// forever.
	var openWfID string
	err = tx.QueryRowContext(ctx,
		`SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state != 'resolved'
		   AND (state != 'processing' OR created_at > NOW() - make_interval(secs => $5))
		 ORDER BY created_at DESC
		 LIMIT 1`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
		stuckProcessingTTL.Seconds(),
	).Scan(&openWfID)
	switch {
	case err == nil:
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit: %w", err)
		}
		return openWfID, nil
	case stderrors.Is(err, sql.ErrNoRows):
		// Fall through to flap check + mint.
	default:
		return "", fmt.Errorf("open-incident lookup: %w", err)
	}

	// Flap guard: drop the alert if the most recent incident for this
	// identity resolved within the window. Held under the lock so a resolve
	// landing concurrently can't slip past us.
	var lastResolvedRecent bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM triage.reports
		   WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		     AND state = 'resolved' AND resolved_at > NOW() - make_interval(secs => $5)
		 )`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
		flapWindow.Seconds(),
	).Scan(&lastResolvedRecent); err != nil {
		return "", fmt.Errorf("flap-guard lookup: %w", err)
	}
	if lastResolvedRecent {
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit: %w", err)
		}
		return "", ErrSkipFlap
	}

	// Find the highest attempt seen historically and mint the next one.
	// Attempts are monotonic, so the most recent row by created_at is the
	// one to extend. The idx_reports_identity_created composite index backs
	// this lookup.
	var latestWfID string
	err = tx.QueryRowContext(ctx,
		`SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		 ORDER BY created_at DESC
		 LIMIT 1`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
	).Scan(&latestWfID)
	maxAttempt := 0
	switch {
	case err == nil:
		_, maxAttempt = types.ParseAttempt(latestWfID)
	case stderrors.Is(err, sql.ErrNoRows):
		// First-ever fire — maxAttempt stays 0, so we'll mint attempt 1
		// (the unsuffixed stem).
	default:
		return "", fmt.Errorf("attempt-history lookup: %w", err)
	}

	wfID := identity.WorkflowIDForAttempt(maxAttempt + 1)

	// The advisory lock means no other transaction is minting the same
	// suffix concurrently, so an ON CONFLICT here would indicate either a
	// stale row this transaction's lookups did not see (replica lag, manual
	// SQL, a row created outside the lock path) or a programming bug.
	// Surface it as an error rather than silently signaling a workflow_id
	// that may attach to a closed Temporal execution.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
		 VALUES ($1, $2, $3, $4, $5, 'processing')
		 ON CONFLICT (workflow_id) DO NOTHING`,
		wfID, identity.Namespace, identity.Name, identity.Kind, identity.AlertName,
	)
	if err != nil {
		return "", fmt.Errorf("insert preliminary report: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", fmt.Errorf("rows affected: %w", err)
	} else if n == 0 {
		return "", fmt.Errorf("preliminary insert conflict on %q — stale row outside the lock path", wfID)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return wfID, nil
}

// acquireStemLock takes the per-stem advisory lock that serializes resolver
// and resolver-of-resolved paths on the same identity. The lock is held for
// the lifetime of tx — pg_advisory_xact_lock releases automatically on
// commit or rollback.
//
// The lock key is derived from a 32-bit hashtext(stem); collisions are
// possible but harmless — two unrelated identities would briefly serialize
// on the same key, never corrupt each other.
func (h *Handler) acquireStemLock(ctx context.Context, tx *sql.Tx, identity types.IncidentIdentity) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		advisoryLockKey1, identity.WorkflowID(),
	); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	return nil
}

// rollbackOnReturn is the deferred cleanup for every tx in this file. It
// distinguishes the expected "already committed" no-op (sql.ErrTxDone) from
// real rollback failures, which are logged so an abandoned tx holding the
// stem lock is visible in operator logs.
//
// On a non-ErrTxDone error the underlying transaction's state is unknown:
// the connection may have died (the backend will roll back and release the
// xact lock when it reaps the session) or the rollback itself failed on a
// live connection (lock stays held until the connection is recycled).
// Either way the lock will be released eventually, but possibly only after
// the Postgres backend tears down the connection.
func (h *Handler) rollbackOnReturn(tx *sql.Tx, stem string) {
	if err := tx.Rollback(); err != nil && !stderrors.Is(err, sql.ErrTxDone) {
		h.logger.Warn("tx rollback failed; per-stem advisory lock may remain held until the underlying transaction is aborted",
			"stem", stem,
			"error", err,
		)
	}
}

// advisoryLockKey1 is the first argument to the two-arg form of
// pg_advisory_xact_lock(int4, int4). The value is arbitrary — replicas
// just need to agree. The migration code in activity uses the one-arg
// form pg_advisory_lock(bigint), which lives in a separate Postgres lock
// space, so no cross-system collision is possible regardless of this
// value.
//
// The int32 type is self-documenting — it mirrors the Postgres int4
// parameter. database/sql widens it to int64 before reaching the driver
// anyway, so flipping it to int would not change the wire encoding; the
// type is here for readability, not enforcement.
const advisoryLockKey1 int32 = 0x74726961 // ASCII "tria"

func (h *Handler) signalWithStart(ctx context.Context, wfID string, identity types.IncidentIdentity, alerts []types.Alert) error {
	opts := client.StartWorkflowOptions{
		ID:                       wfID,
		TaskQueue:                h.taskQueue,
		WorkflowExecutionTimeout: workflowExecutionTimeout,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}

	// Signal each alert individually to the workflow
	for _, alert := range alerts {
		_, err := h.temporalClient.SignalWithStartWorkflow(
			ctx,
			wfID,
			workflow.AlertSignalName,
			alert,
			opts,
			workflow.TriageWorkflow,
			types.TriageParams{Identity: identity},
		)
		if err != nil {
			return fmt.Errorf("signal alert %s: %w", alert.Fingerprint, err)
		}
	}

	h.logger.Info("workflow signaled",
		"workflow_id", wfID,
		"alert_count", len(alerts),
		"namespace", identity.Namespace,
		"workload", identity.Name,
	)

	return nil
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleResolvedAlerts marks reports as resolved for an alert group. It
// returns (updated, errored): the count of rows transitioned and the count
// of identities whose UPDATE failed. The caller must surface errored > 0 as
// a 500 so Alertmanager retries — otherwise a transient DB blip would
// permanently leave rows in 'processing', which jams the open-incident
// lookup in resolveWorkflowID.
//
// Each per-stem UPDATE runs under the same advisory lock that
// resolveWorkflowID acquires, so a resolve cannot interleave between
// resolveWorkflowID's open-row lookup and its INSERT.
//
// The UPDATE keys on the identity stem rather than a reconstructed
// workflow_id because with the attempt-counter scheme the webhook can't
// know from Alertmanager labels which attempt is currently open. The
// stem-keyed predicate (state != 'resolved') transitions every open row
// for this identity in one shot — normally one row, but a stuck-processing
// orphan from a prior signalWithStart failure can coexist with a freshly
// minted attempt during the stuckProcessingTTL bypass window, and resolving
// both at once is the desired behaviour.
func (h *Handler) handleResolvedAlerts(ctx context.Context, group types.AlertGroup) (updated, errored int) {
	if h.db == nil {
		return 0, 0
	}

	// Dedup by identity stem so two alerts resolving the same incident don't
	// issue two UPDATEs.
	seen := make(map[string]types.IncidentIdentity)
	for _, alert := range group.Alerts {
		if alert.Status != "resolved" {
			continue
		}
		identity := types.DeriveIdentity(alert.Labels)
		seen[identity.WorkflowID()] = identity
	}

	for stem, identity := range seen {
		n, err := h.resolveByStem(ctx, identity)
		if err != nil {
			h.logger.Error("failed to mark report resolved", "stem", stem, "error", err)
			errored++
			continue
		}
		if n > 0 {
			h.logger.Info("report resolved", "stem", stem, "rows", n)
			updated += n
		}
	}
	return updated, errored
}

// resolveByStem transitions whatever row is currently un-resolved for this
// identity to state='resolved', under the same per-stem advisory lock that
// resolveWorkflowID uses. Returns the number of rows affected.
func (h *Handler) resolveByStem(ctx context.Context, identity types.IncidentIdentity) (int, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer h.rollbackOnReturn(tx, identity.WorkflowID())

	if err := h.acquireStemLock(ctx, tx, identity); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE triage.reports SET resolved_at = NOW(), state = 'resolved'
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state != 'resolved'`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
	)
	if err != nil {
		return 0, fmt.Errorf("update report: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int(n), nil
}

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.healthy.Load() {
		http.Error(w, "not ready: Temporal unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
