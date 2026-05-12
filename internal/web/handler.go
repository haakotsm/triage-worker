package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Report mirrors api.Report for template rendering.
type Report struct {
	ID               int64
	WorkflowID       string
	Namespace        string
	Workload         string
	Kind             string
	AlertName        string
	Classification   string
	Severity         string
	Summary          string
	BlastRadius      string
	RootCause        string
	CausalChain      []string
	Evidence         []Evidence
	Recommendations  []Recommendation
	Confidence       float64
	EscalationNeeded bool
	AlertCount       int
	StartedAt        time.Time
	CompletedAt      time.Time
	CreatedAt        time.Time
	ResolvedAt       *time.Time
}

// Evidence mirrors api.EvidenceItem for template rendering.
type Evidence struct {
	Observation string
	Source      string
	Strength    string
}

// Recommendation mirrors api.Recommendation for template rendering.
type Recommendation struct {
	Action   string
	Command  string
	Risk     string
	Source   string
	Expected string
}

// DashboardData is the template data for the dashboard page.
type DashboardData struct {
	Reports    []Report
	TotalCount int
	Limit      int
	Offset     int
	Query      string
	Severity   string
	Status     string
}

// DetailData is the template data for the report detail page.
type DetailData struct {
	Report          Report
	L1Commands      []Recommendation
	AgentRecs       []Recommendation
}

// Handler serves the web dashboard and static assets.
type Handler struct {
	templates *template.Template
	static    http.Handler
	db        *sql.DB
	logger    *slog.Logger
}

// NewHandler creates a web dashboard handler.
func NewHandler(db *sql.DB, logger *slog.Logger) (*Handler, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(content,
		"templates/*.html",
		"templates/partials/*.html",
		"templates/components/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		return nil, fmt.Errorf("static fs: %w", err)
	}

	return &Handler{
		templates: tmpl,
		static:    http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
		db:        db,
		logger:    logger,
	}, nil
}

// ServeHTTP routes dashboard requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/static/"):
		h.static.ServeHTTP(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/dashboard":
		h.handleDashboard(w, r)
	case r.URL.Path == "/partials/reports":
		h.handlePartialReports(w, r)
	case strings.HasPrefix(r.URL.Path, "/reports/"):
		h.handleDetail(w, r)
	default:
		http.NotFound(w, r)
	}
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.fetchDashboardData(r)
	if err != nil {
		h.logger.Error("fetch dashboard data", "error", err)
		h.renderError(w, "Failed to load reports")
		return
	}

	if isHTMX(r) {
		h.render(w, "report-table", data)
	} else {
		h.render(w, "dashboard", data)
	}
}

func (h *Handler) handlePartialReports(w http.ResponseWriter, r *http.Request) {
	data, err := h.fetchDashboardData(r)
	if err != nil {
		h.logger.Error("fetch partial data", "error", err)
		h.renderError(w, "Failed to load reports")
		return
	}
	h.render(w, "report-table", data)
}

func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/reports/")
	if idStr == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	report, err := h.fetchReport(r.Context(), idStr)
	if err != nil {
		h.logger.Error("fetch report", "error", err, "id", idStr)
		h.renderError(w, "Failed to load report")
		return
	}
	if report == nil {
		http.NotFound(w, r)
		return
	}

	var l1 []Recommendation
	var agent []Recommendation
	for _, rec := range report.Recommendations {
		if rec.Source == "l1" {
			l1 = append(l1, rec)
		} else {
			agent = append(agent, rec)
		}
	}

	data := DetailData{
		Report:     *report,
		L1Commands: l1,
		AgentRecs:  agent,
	}

	if isHTMX(r) {
		h.render(w, "report-detail", data)
	} else {
		h.render(w, "detail", data)
	}
}

func (h *Handler) render(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		h.logger.Error("render template", "error", err, "template", name)
	}
}

func (h *Handler) renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_ = h.templates.ExecuteTemplate(w, "error", map[string]string{"Message": msg})
}

// fetchDashboardData queries reports for the dashboard.
func (h *Handler) fetchDashboardData(r *http.Request) (DashboardData, error) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 20, 1, 100)
	offset := intParam(q.Get("offset"), 0, 0, 10000)
	severity := q.Get("severity")
	search := q.Get("search")
	status := q.Get("status")

	where := "WHERE 1=1"
	args := []interface{}{}
	argIdx := 1

	if severity != "" {
		where += " AND severity = $" + strconv.Itoa(argIdx)
		args = append(args, severity)
		argIdx++
	}
	if search != "" {
		where += fmt.Sprintf(" AND (workload ILIKE $%d OR namespace ILIKE $%d OR alert_name ILIKE $%d)", argIdx, argIdx, argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}
	if status == "active" {
		where += " AND resolved_at IS NULL"
	} else if status == "resolved" {
		where += " AND resolved_at IS NOT NULL"
	}

	// Count query
	var totalCount int
	countQuery := "SELECT COUNT(*) FROM triage.reports " + where
	if err := h.db.QueryRowContext(r.Context(), countQuery, args...).Scan(&totalCount); err != nil {
		return DashboardData{}, fmt.Errorf("count: %w", err)
	}

	// Data query
	dataQuery := fmt.Sprintf(`SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at, summary, blast_radius
		FROM triage.reports %s
		ORDER BY CASE WHEN resolved_at IS NULL THEN 0 ELSE 1 END,
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			completed_at DESC
		LIMIT $%d OFFSET $%d`, where, argIdx, argIdx+1)
	dataArgs := append(args, limit, offset)

	reports, err := h.queryReports(r.Context(), dataQuery, dataArgs...)
	if err != nil {
		return DashboardData{}, fmt.Errorf("query: %w", err)
	}

	return DashboardData{
		Reports:    reports,
		TotalCount: totalCount,
		Limit:      limit,
		Offset:     offset,
		Query:      search,
		Severity:   severity,
		Status:     status,
	}, nil
}

// fetchReport queries a single report by ID or workflow_id.
func (h *Handler) fetchReport(ctx context.Context, idStr string) (*Report, error) {
	query := `SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at, summary, blast_radius
		FROM triage.reports WHERE `

	var args []interface{}
	if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
		query += "id = $1"
		args = []interface{}{id}
	} else {
		// Workflow IDs contain slashes (e.g. "triage/ns/Kind/name/alert")
		// which are encoded as pipes in URL path segments for safe routing.
		wfID := strings.ReplaceAll(idStr, "|", "/")
		query += "workflow_id = $1"
		args = []interface{}{wfID}
	}

	reports, err := h.queryReports(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 {
		return nil, nil
	}
	return &reports[0], nil
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

		if len(causalChainJSON) > 0 {
			if err := json.Unmarshal(causalChainJSON, &r.CausalChain); err != nil {
				h.logger.Warn("unmarshal causal_chain", "id", r.ID, "error", err)
			}
		}
		if r.CausalChain == nil {
			r.CausalChain = []string{}
		}
		if len(evidenceJSON) > 0 {
			if err := json.Unmarshal(evidenceJSON, &r.Evidence); err != nil {
				h.logger.Warn("unmarshal evidence", "id", r.ID, "error", err)
			}
		}
		if r.Evidence == nil {
			r.Evidence = []Evidence{}
		}
		if len(recommendationsJSON) > 0 {
			if err := json.Unmarshal(recommendationsJSON, &r.Recommendations); err != nil {
				h.logger.Warn("unmarshal recommendations", "id", r.ID, "error", err)
			}
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

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"severityClass": func(s string) string {
			switch s {
			case "critical":
				return "badge-error"
			case "warning":
				return "badge-warning"
			default:
				return "badge-info"
			}
		},
		"blastIcon": func(b string) string {
			switch b {
			case "cluster":
				return "🔴"
			case "namespace":
				return "🟠"
			case "deployment":
				return "🟡"
			default:
				return "🟢"
			}
		},
		"riskClass": func(r string) string {
			switch r {
			case "high":
				return "badge-error"
			case "medium":
				return "badge-warning"
			case "low":
				return "badge-info"
			default:
				return "badge-ghost"
			}
		},
		"strengthClass": func(s string) string {
			switch s {
			case "strong":
				return "badge-success"
			case "moderate":
				return "badge-warning"
			default:
				return "badge-ghost"
			}
		},
		"timeAgo": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mul":   func(a, b int) int { return a * b },
		"deref": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		"pct": func(a, b int) string {
			if b == 0 {
				return "0"
			}
			return fmt.Sprintf("%.0f", float64(a)/float64(b)*100)
		},
		"pages": func(total, perPage int) int {
			if perPage == 0 {
				return 0
			}
			return (total + perPage - 1) / perPage
		},
	}
}
