package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Report is the JSON response for a triage report.
type Report struct {
	ID               int64            `json:"id"`
	WorkflowID       string           `json:"workflow_id"`
	Namespace        string           `json:"namespace"`
	Workload         string           `json:"workload"`
	Kind             string           `json:"kind"`
	AlertName        string           `json:"alert_name"`
	Classification   string           `json:"classification"`
	Severity         string           `json:"severity"`
	Summary          string           `json:"summary,omitempty"`
	BlastRadius      string           `json:"blast_radius,omitempty"`
	RootCause        string           `json:"root_cause"`
	CausalChain      []string         `json:"causal_chain"`
	Evidence         []EvidenceItem   `json:"evidence"`
	Recommendations  []Recommendation `json:"recommendations"`
	Confidence       float64          `json:"confidence"`
	EscalationNeeded bool             `json:"escalation_needed"`
	AlertCount       int              `json:"alert_count"`
	StartedAt        time.Time        `json:"started_at"`
	CompletedAt      time.Time        `json:"completed_at"`
	CreatedAt        time.Time        `json:"created_at"`
	ResolvedAt       *time.Time       `json:"resolved_at,omitempty"`
}

// EvidenceItem mirrors types.EvidenceItem for API output.
type EvidenceItem struct {
	Observation string `json:"observation"`
	Source      string `json:"source"`
	Strength    string `json:"strength"`
}

// Recommendation mirrors types.Recommendation for API output.
type Recommendation struct {
	Action   string `json:"action"`
	Command  string `json:"command,omitempty"`
	Risk     string `json:"risk"`
	Source   string `json:"source,omitempty"`
	Expected string `json:"expected,omitempty"`
}

// Handler serves the read-only triage reports API.
type Handler struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewHandler creates an API handler with the given database connection.
func NewHandler(db *sql.DB, logger *slog.Logger) *Handler {
	return &Handler{db: db, logger: logger}
}

// ServeHTTP routes API requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Strip the /api prefix for routing
	path := strings.TrimPrefix(r.URL.Path, "/api")

	switch {
	case path == "/reports" || path == "/reports/":
		h.listReports(w, r)
	case path == "/reports/active":
		h.listActiveReports(w, r)
	case strings.HasPrefix(path, "/reports/"):
		id := strings.TrimPrefix(path, "/reports/")
		h.getReport(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) listReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 20, 1, 100)
	offset := intParam(q.Get("offset"), 0, 0, 10000)

	query := `SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at,
		summary, blast_radius
		FROM triage.reports WHERE 1=1`
	args := []interface{}{}
	argIdx := 1

	if v := q.Get("severity"); v != "" {
		query += " AND severity = $" + strconv.Itoa(argIdx)
		args = append(args, v)
		argIdx++
	}
	if v := q.Get("classification"); v != "" {
		query += " AND classification = $" + strconv.Itoa(argIdx)
		args = append(args, v)
		argIdx++
	}
	if v := q.Get("namespace"); v != "" {
		query += " AND namespace = $" + strconv.Itoa(argIdx)
		args = append(args, v)
		argIdx++
	}

	query += " ORDER BY completed_at DESC LIMIT $" + strconv.Itoa(argIdx) + " OFFSET $" + strconv.Itoa(argIdx+1)
	args = append(args, limit, offset)

	reports, err := h.queryReports(r.Context(), query, args...)
	if err != nil {
		h.logger.Error("query reports failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reports": reports,
		"count":   len(reports),
		"limit":   limit,
		"offset":  offset,
	})
}

func (h *Handler) listActiveReports(w http.ResponseWriter, r *http.Request) {
	query := `SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at,
		summary, blast_radius
		FROM triage.reports
		WHERE resolved_at IS NULL
		ORDER BY
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			CASE blast_radius WHEN 'cluster' THEN 0 WHEN 'namespace' THEN 1 WHEN 'deployment' THEN 2 ELSE 3 END,
			completed_at DESC
		LIMIT 50`

	reports, err := h.queryReports(r.Context(), query)
	if err != nil {
		h.logger.Error("query active reports failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reports": reports,
		"count":   len(reports),
	})
}

func (h *Handler) getReport(w http.ResponseWriter, r *http.Request, idStr string) {
	// Try as numeric ID first, then as workflow_id
	query := `SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at,
		summary, blast_radius
		FROM triage.reports WHERE `

	var args []interface{}
	if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
		query += "id = $1"
		args = []interface{}{id}
	} else {
		// URL-decode slashes: the workflow_id contains slashes
		wfID := strings.ReplaceAll(idStr, "|", "/")
		query += "workflow_id = $1"
		args = []interface{}{wfID}
	}

	reports, err := h.queryReports(r.Context(), query, args...)
	if err != nil {
		h.logger.Error("query report failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if len(reports) == 0 {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, reports[0])
}

func (h *Handler) queryReports(ctx context.Context, query string, args ...interface{}) ([]Report, error) {
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var r Report
		var causalChainJSON, evidenceJSON, recommendationsJSON []byte
		var resolvedAt sql.NullTime

		err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.Namespace, &r.Workload, &r.Kind, &r.AlertName,
			&r.Classification, &r.Severity, &r.RootCause,
			&causalChainJSON, &evidenceJSON, &recommendationsJSON,
			&r.Confidence, &r.EscalationNeeded, &r.AlertCount,
			&r.StartedAt, &r.CompletedAt, &r.CreatedAt, &resolvedAt,
			&r.Summary, &r.BlastRadius,
		)
		if err != nil {
			return nil, err
		}

		if resolvedAt.Valid {
			r.ResolvedAt = &resolvedAt.Time
		}

		// Unmarshal JSONB columns
		if len(causalChainJSON) > 0 {
			_ = json.Unmarshal(causalChainJSON, &r.CausalChain)
		}
		if r.CausalChain == nil {
			r.CausalChain = []string{}
		}

		if len(evidenceJSON) > 0 {
			_ = json.Unmarshal(evidenceJSON, &r.Evidence)
		}
		if r.Evidence == nil {
			r.Evidence = []EvidenceItem{}
		}

		if len(recommendationsJSON) > 0 {
			_ = json.Unmarshal(recommendationsJSON, &r.Recommendations)
		}
		if r.Recommendations == nil {
			r.Recommendations = []Recommendation{}
		}

		reports = append(reports, r)
	}

	if reports == nil {
		reports = []Report{}
	}

	return reports, rows.Err()
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

func intParam(s string, defaultVal, min, max int) int {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < min {
		return defaultVal
	}
	if v > max {
		return max
	}
	return v
}
