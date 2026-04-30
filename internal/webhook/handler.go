package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

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
}

// NewHandler creates a new webhook handler.
func NewHandler(tc client.Client, taskQueue string, logger *slog.Logger) *Handler {
	healthy := &atomic.Bool{}
	healthy.Store(true)
	return &Handler{
		temporalClient: tc,
		taskQueue:      taskQueue,
		logger:         logger,
		healthy:        healthy,
	}
}

// SetHealthy updates the health status (used for readiness probe).
func (h *Handler) SetHealthy(healthy bool) {
	h.healthy.Store(healthy)
}

// ServeHTTP routes requests to the appropriate handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/webhook":
		h.handleWebhook(w, r)
	case "/healthz":
		h.handleHealthz(w, r)
	case "/readyz":
		h.handleReadyz(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check health — reject if Temporal is unreachable
	if !h.healthy.Load() {
		http.Error(w, "service unavailable: Temporal unreachable", http.StatusServiceUnavailable)
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

	// Reject resolved-only groups — don't start new workflows for already-resolved incidents
	firing := alertGroup.FiringAlerts()
	if len(firing) == 0 {
		h.logger.Debug("skipping resolved-only alert group", "group_key", alertGroup.GroupKey)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"skipped","reason":"resolved_only"}`))
		return
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
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) signalWithStart(ctx context.Context, wfID string, identity types.IncidentIdentity, alerts []types.Alert) error {
	opts := client.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: h.taskQueue,
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

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.healthy.Load() {
		http.Error(w, "not ready: Temporal unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
