package webhook

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/haakotsm/triage-worker/internal/types"
	"github.com/haakotsm/triage-worker/internal/workflow"
)

const maxBodySize = 1 << 20 // 1 MB

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
	case strings.HasPrefix(r.URL.Path, "/api/incidents/") && h.webHandler != nil:
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate — require Bearer token if secret is configured
	if h.webhookSecret != "" {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if auth == token || subtle.ConstantTimeCompare([]byte(token), []byte(h.webhookSecret)) != 1 {
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
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Read and validate body
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var alertGroup types.AlertGroup
	if err := json.Unmarshal(body, &alertGroup); err != nil {
		h.logger.Error("invalid alertmanager payload", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Handle resolved alerts — mark matching reports as resolved
	firing := alertGroup.FiringAlerts()
	if len(firing) == 0 {
		resolved := h.handleResolvedAlerts(r.Context(), alertGroup)
		h.logger.Debug("processed resolved-only alert group",
			"group_key", alertGroup.GroupKey,
			"resolved_count", resolved,
		)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"resolved","reports_updated":%d}`, resolved)
		return
	}

	// Also process resolved alerts in mixed groups (firing + resolved)
	if alertGroup.Status == "firing" {
		if resolved := h.handleResolvedAlerts(r.Context(), alertGroup); resolved > 0 {
			h.logger.Info("resolved reports in mixed group", "resolved_count", resolved)
		}
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

	ctx := r.Context()
	var errors []string
	workflowsSignaled := 0

	for stem, alerts := range alertsByStem {
		identity := identitiesByStem[stem]

		// 5-minute flap guard: if the prior incident for this identity was
		// just resolved, treat the re-fire as alert noise and drop it.
		if h.recentlyResolved(ctx, identity) {
			h.logger.Info("dedup: skipping re-fire within 5min of resolution",
				"stem", stem,
				"namespace", identity.Namespace,
				"workload", identity.Name,
			)
			continue
		}

		wfID, err := h.resolveWorkflowID(ctx, identity)
		if err != nil {
			h.logger.Error("resolve workflow id failed",
				"stem", stem,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("%s: %v", stem, err))
			continue
		}

		if err := h.signalWithStart(ctx, wfID, identity, alerts); err != nil {
			h.logger.Error("signal-with-start failed",
				"workflow_id", wfID,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("%s: %v", wfID, err))
			continue
		}
		workflowsSignaled++
	}

	if len(errors) > 0 {
		// Return 500 so Alertmanager retries
		http.Error(w, fmt.Sprintf("partial failure: %v", errors), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	resp := map[string]interface{}{
		"status":    "accepted",
		"workflows": workflowsSignaled,
		"alerts":    len(firing),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("failed to write response", "error", err)
	}
}

// recentlyResolved reports whether the incident identity was marked resolved
// within the last 5 minutes — the alert-flap guard that suppresses immediate
// re-fires. Returns false when no DB is configured or on query error (fail-
// open so a transient DB blip doesn't drop genuine alerts).
func (h *Handler) recentlyResolved(ctx context.Context, identity types.IncidentIdentity) bool {
	if h.db == nil {
		return false
	}
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state = 'resolved' AND resolved_at > NOW() - INTERVAL '5 minutes'`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
	).Scan(&n); err != nil {
		h.logger.Warn("flap-guard query failed, allowing alert",
			"error", err,
			"namespace", identity.Namespace,
			"workload", identity.Name,
		)
		return false
	}
	return n > 0
}

// resolveWorkflowID picks the workflow_id that a firing alert for this
// identity should signal. It also writes the preliminary report row in the
// same transaction so the lifecycle dashboard sees the incident as soon as
// the webhook returns.
//
// Selection rules:
//
//  1. If an open (state != 'resolved') row already exists for this identity,
//     reuse its workflow_id. Re-fires of an in-progress incident dedupe
//     into the same Temporal execution exactly as before this change.
//  2. Otherwise mint a new workflow_id by appending an incremented attempt
//     counter to the identity stem. The first attempt for any identity
//     uses the unsuffixed stem so legacy rows continue to round-trip.
//
// A per-stem advisory lock serializes minting so two concurrent re-fires
// after a resolution cannot both pick the same attempt number, which would
// otherwise leave one of them with a silently-rejected INSERT.
//
// When no DB is configured the function falls back to the legacy unsuffixed
// stem so the webhook still works as a dumb Temporal trigger in tests and
// in-memory deployments.
func (h *Handler) resolveWorkflowID(ctx context.Context, identity types.IncidentIdentity) (string, error) {
	if h.db == nil {
		return identity.WorkflowID(), nil
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize per-stem so concurrent webhook calls can't both mint the
	// same attempt suffix. Two-arg pg_advisory_xact_lock keys on a stable
	// table-scoped namespace plus hashtext(stem); the lock releases on
	// commit/rollback automatically.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		advisoryLockNamespace, identity.WorkflowID(),
	); err != nil {
		return "", fmt.Errorf("advisory lock: %w", err)
	}

	// Reuse the open incident's workflow_id if one exists.
	var openWfID string
	err = tx.QueryRowContext(ctx,
		`SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
		   AND state != 'resolved'
		 ORDER BY created_at DESC
		 LIMIT 1`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
	).Scan(&openWfID)
	switch {
	case err == nil:
		// Idempotent re-fire — caller will SignalWithStart the existing
		// workflow ID. No new row needed; the row is already there.
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit: %w", err)
		}
		return openWfID, nil
	case err == sql.ErrNoRows:
		// Fall through to mint a new attempt.
	default:
		return "", fmt.Errorf("open-incident lookup: %w", err)
	}

	// No open incident — find the highest attempt seen historically and
	// mint the next one. Parsing the suffix in Go (rather than SQL) keeps
	// the encoding in one place: types.ParseAttempt.
	rows, err := tx.QueryContext(ctx,
		`SELECT workflow_id FROM triage.reports
		 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4`,
		identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
	)
	if err != nil {
		return "", fmt.Errorf("attempt-history lookup: %w", err)
	}
	maxAttempt := 0
	for rows.Next() {
		var existing string
		if err := rows.Scan(&existing); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan attempt row: %w", err)
		}
		if _, n := types.ParseAttempt(existing); n > maxAttempt {
			maxAttempt = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate attempt rows: %w", err)
	}
	rows.Close()

	wfID := identity.WorkflowIDForAttempt(maxAttempt + 1)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
		 VALUES ($1, $2, $3, $4, $5, 'processing')
		 ON CONFLICT (workflow_id) DO NOTHING`,
		wfID, identity.Namespace, identity.Name, identity.Kind, identity.AlertName,
	); err != nil {
		return "", fmt.Errorf("insert preliminary report: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return wfID, nil
}

// advisoryLockNamespace is the first argument to the two-arg form of
// pg_advisory_xact_lock. Picked to be distinct from the migration lock key
// in activity/report.go (0x7472696167655F31, ASCII "triage_1") so the two
// lock spaces can't collide. Value is arbitrary — replicas just need to
// agree.
const advisoryLockNamespace int32 = 0x74726961 // ASCII "tria"

func (h *Handler) signalWithStart(ctx context.Context, wfID string, identity types.IncidentIdentity, alerts []types.Alert) error {
	opts := client.StartWorkflowOptions{
		ID:                       wfID,
		TaskQueue:                h.taskQueue,
		WorkflowExecutionTimeout: 15 * time.Minute,
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

// handleResolvedAlerts marks reports as resolved for resolved alert groups.
// Returns the number of reports updated.
//
// The update keys on the identity stem rather than a reconstructed
// workflow_id because, with the attempt-counter scheme, the webhook can't
// know from Alertmanager's labels alone which attempt is currently open.
// Resolving "whichever row is currently not resolved for this identity" is
// what the operator means anyway: there is at most one such row at a time.
func (h *Handler) handleResolvedAlerts(ctx context.Context, group types.AlertGroup) int {
	if h.db == nil {
		return 0
	}

	// Dedup by identity stem (the WorkflowID() form) so two alerts
	// resolving the same incident don't issue two UPDATEs.
	seen := make(map[string]types.IncidentIdentity)
	for _, alert := range group.Alerts {
		if alert.Status != "resolved" {
			continue
		}
		identity := types.DeriveIdentity(alert.Labels)
		seen[identity.WorkflowID()] = identity
	}

	updated := 0
	for stem, identity := range seen {
		result, err := h.db.ExecContext(ctx,
			`UPDATE triage.reports SET resolved_at = NOW(), state = 'resolved'
			 WHERE namespace = $1 AND kind = $2 AND workload = $3 AND alert_name = $4
			   AND state != 'resolved'`,
			identity.Namespace, identity.Kind, identity.Name, identity.AlertName,
		)
		if err != nil {
			h.logger.Error("failed to mark report resolved", "stem", stem, "error", err)
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			h.logger.Info("report resolved", "stem", stem, "rows", n)
			updated += int(n)
		}
	}
	return updated
}

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.healthy.Load() {
		http.Error(w, "not ready: Temporal unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
