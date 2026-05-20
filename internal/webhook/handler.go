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

	// Process each firing alert — group by derived workflow ID
	alertsByWorkflow := make(map[string][]types.Alert)
	identitiesByWorkflow := make(map[string]types.IncidentIdentity)

	for _, alert := range firing {
		identity := types.DeriveIdentity(alert.Labels)
		wfID := identity.WorkflowID()
		alertsByWorkflow[wfID] = append(alertsByWorkflow[wfID], alert)
		identitiesByWorkflow[wfID] = identity
	}

	// SignalWithStart for each derived workflow ID
	ctx := r.Context()
	var errors []string

	for wfID, alerts := range alertsByWorkflow {
		identity := identitiesByWorkflow[wfID]

		// Dedup: skip if same workload was resolved within the last 5 minutes.
		if h.db != nil {
			var recentResolve int
			_ = h.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM triage.reports
				 WHERE namespace = $1 AND workload = $2 AND kind = $3 AND alert_name = $4
				   AND state = 'resolved' AND resolved_at > NOW() - INTERVAL '5 minutes'`,
				identity.Namespace, identity.Name, identity.Kind, identity.AlertName,
			).Scan(&recentResolve)
			if recentResolve > 0 {
				h.logger.Info("dedup: skipping re-fire within 5min of resolution",
					"workflow_id", wfID,
					"namespace", identity.Namespace,
					"workload", identity.Name,
				)
				continue
			}
		}

		// Create preliminary report row for realtime lifecycle tracking (best-effort).
		if h.db != nil {
			_, _ = h.db.ExecContext(ctx,
				`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
				 VALUES ($1, $2, $3, $4, $5, 'processing')
				 ON CONFLICT (workflow_id) DO NOTHING`,
				wfID, identity.Namespace, identity.Name, identity.Kind, identity.AlertName,
			)
		}

		if err := h.signalWithStart(ctx, wfID, identity, alerts); err != nil {
			h.logger.Error("signal-with-start failed",
				"workflow_id", wfID,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("%s: %v", wfID, err))
		}
	}

	if len(errors) > 0 {
		// Return 500 so Alertmanager retries
		http.Error(w, fmt.Sprintf("partial failure: %v", errors), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	resp := map[string]interface{}{
		"status":    "accepted",
		"workflows": len(alertsByWorkflow),
		"alerts":    len(firing),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("failed to write response", "error", err)
	}
}

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
func (h *Handler) handleResolvedAlerts(ctx context.Context, group types.AlertGroup) int {
	if h.db == nil {
		return 0
	}

	// Derive unique workflow IDs from resolved alerts only
	seen := make(map[string]bool)
	for _, alert := range group.Alerts {
		if alert.Status != "resolved" {
			continue
		}
		identity := types.DeriveIdentity(alert.Labels)
		wfID := identity.WorkflowID()
		seen[wfID] = true
	}

	updated := 0
	for wfID := range seen {
		result, err := h.db.ExecContext(ctx,
			`UPDATE triage.reports SET resolved_at = NOW(), state = 'resolved'
			 WHERE workflow_id = $1 AND resolved_at IS NULL`,
			wfID,
		)
		if err != nil {
			h.logger.Error("failed to mark report resolved", "workflow_id", wfID, "error", err)
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			h.logger.Info("report resolved", "workflow_id", wfID)
			updated++
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
