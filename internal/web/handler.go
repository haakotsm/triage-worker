package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
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
	State            string
	RootCause        string
	CausalChain      []string
	Evidence         []Evidence
	Recommendations  []Recommendation
	Confidence       float64
	EscalationNeeded bool
	AlertCount       int
	StartedAt        time.Time
	CompletedAt      *time.Time
	CreatedAt        time.Time
	ResolvedAt       *time.Time
	AssignedTo       string
	AcknowledgedAt   *time.Time
	EscalationLevel  string
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

// NameCount holds a label and its count for chart data.
type NameCount struct {
	Name  string
	Count int
	Pct   float64
}

// StatsData provides aggregated metrics for dashboard charts.
type StatsData struct {
	ActiveCount       int
	ResolvedCount     int
	EscalatedCount    int
	AcknowledgedCount int
	ReportedCount     int
	ProcessingCount   int
	CriticalCount     int
	WarningCount      int
	InfoCount         int
	TotalCount        int

	BlastCluster    int
	BlastNamespace  int
	BlastDeployment int
	BlastPod        int

	Classifications []NameCount

	MTTRSeconds float64
	MTTRDisplay string

	ResolutionRate float64

	// Sparkline data: daily counts for last 14 days.
	DailyCounts     []int
	SparklinePoints string
}

// DashboardData is the template data for the dashboard page.
type DashboardData struct {
	Reports    []Report
	Stats      StatsData
	Incidents  []Incident
	TotalCount int
	Limit      int
	Offset     int
	Query      string
	Severity   string
	Status     string
	Sort       string
	Dir        string
	SSEEnabled bool
}

// DetailData is the template data for the report detail page.
type DetailData struct {
	Report          Report
	L1Commands      []Recommendation
	AgentRecs       []Recommendation
}

// Handler serves the web dashboard and static assets.
type Handler struct {
	pages    map[string]*template.Template // per-page template sets (clone of shared + page-specific)
	partials *template.Template            // shared partials for htmx fragment renders
	static   http.Handler
	db       *sql.DB
	logger   *slog.Logger
	sse      *SSEBroker // optional: nil if SSE not configured
	retriage RetrieveStarter // optional: starts re-triage workflows
}

// RetrieveStarter starts a new triage workflow for re-analysis.
type RetrieveStarter interface {
	StartRetriage(ctx context.Context, workflowID, namespace, workload, kind, alertName string) (string, error)
}

// SetSSEBroker attaches an SSE broker for realtime event streaming.
func (h *Handler) SetSSEBroker(b *SSEBroker) {
	h.sse = b
}

// SetRetrieveStarter attaches a workflow starter for re-triage functionality.
func (h *Handler) SetRetrieveStarter(rs RetrieveStarter) {
	h.retriage = rs
}

// NewHandler creates a web dashboard handler.
func NewHandler(db *sql.DB, logger *slog.Logger) (*Handler, error) {
	funcs := templateFuncs()

	// Parse shared templates: layout + partials + components
	shared, err := template.New("").Funcs(funcs).ParseFS(content,
		"templates/layout.html",
		"templates/partials/*.html",
		"templates/components/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse shared templates: %w", err)
	}

	// Clone per page so each gets its own "content" definition
	pages := make(map[string]*template.Template)
	for _, page := range []string{"dashboard", "detail"} {
		clone, cloneErr := shared.Clone()
		if cloneErr != nil {
			return nil, fmt.Errorf("clone templates for %s: %w", page, cloneErr)
		}
		pageTmpl, parseErr := clone.ParseFS(content, "templates/"+page+".html")
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, parseErr)
		}
		pages[page] = pageTmpl
	}

	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		return nil, fmt.Errorf("static fs: %w", err)
	}

	return &Handler{
		pages:    pages,
		partials: shared,
		static:   cacheHeaders(http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))),
		db:       db,
		logger:   logger,
	}, nil
}

// ServeHTTP routes dashboard requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security headers on all dashboard responses.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")

	switch {
	case strings.HasPrefix(r.URL.Path, "/static/"):
		h.static.ServeHTTP(w, r)
	case r.URL.Path == "/events":
		h.handleEvents(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/dashboard":
		h.handleDashboard(w, r)
	case r.URL.Path == "/partials/reports":
		h.handlePartialReports(w, r)
	case r.URL.Path == "/partials/stats":
		h.handlePartialStats(w, r)
	case r.URL.Path == "/partials/incidents":
		h.handlePartialIncidents(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/incidents/") && strings.HasSuffix(r.URL.Path, "/acknowledge"):
		h.handleAcknowledge(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/incidents/") && strings.HasSuffix(r.URL.Path, "/escalate"):
		h.handleEscalate(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/incidents/") && strings.HasSuffix(r.URL.Path, "/notes"):
		h.handleNotes(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/incidents/") && strings.HasSuffix(r.URL.Path, "/retriage"):
		h.handleRetriage(w, r)
	case strings.HasPrefix(r.URL.Path, "/reports/") && strings.HasSuffix(r.URL.Path, "/resolve"):
		h.handleResolve(w, r)
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
	data, err := h.fetchReportTableData(r)
	if err != nil {
		h.logger.Error("fetch partial data", "error", err)
		h.renderError(w, "Failed to load reports")
		return
	}
	h.render(w, "report-table", data)
}

func (h *Handler) handlePartialStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.fetchStats(r.Context())
	if err != nil {
		h.logger.Error("fetch stats", "error", err)
		h.renderError(w, "Failed to load stats")
		return
	}
	h.render(w, "stats-panel", stats)
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
	var buf bytes.Buffer
	// Full-page renders use per-page template sets; partials use shared set
	if tmpl, ok := h.pages[name]; ok {
		if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
			h.logger.Error("render page", "error", err, "template", name)
			h.renderError(w, "Internal error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := buf.WriteTo(w); err != nil {
			h.logger.Debug("write response", "error", err, "template", name)
		}
		return
	}
	if err := h.partials.ExecuteTemplate(&buf, name, data); err != nil {
		h.logger.Error("render partial", "error", err, "template", name)
		h.renderError(w, "Internal error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		h.logger.Debug("write response", "error", err, "template", name)
	}
}

func (h *Handler) renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	if err := h.partials.ExecuteTemplate(w, "error", map[string]string{"Message": msg}); err != nil {
		h.logger.Error("render error template", "error", err)
	}
}

// fetchDashboardData queries reports for the dashboard.
func (h *Handler) fetchDashboardData(r *http.Request) (DashboardData, error) {
	data, err := h.fetchReportTableData(r)
	if err != nil {
		return DashboardData{}, err
	}

	// Fetch aggregate stats (non-fatal if it fails).
	stats, err := h.fetchStats(r.Context())
	if err != nil {
		h.logger.Warn("fetch stats for dashboard", "error", err)
	}

	// Fetch active incidents (non-fatal if it fails).
	incidents, err := FetchActiveIncidents(r.Context(), h.db)
	if err != nil {
		h.logger.Warn("fetch incidents for dashboard", "error", err)
	}

	data.Stats = stats
	data.Incidents = incidents
	return data, nil
}

// validSortFields maps user-facing sort keys to SQL expressions (whitelist to prevent injection).
var validSortFields = map[string]string{
	"severity":  "CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END",
	"workload":  "workload",
	"namespace": "namespace",
	"alert":     "alert_name",
	"blast":     "CASE blast_radius WHEN 'cluster' THEN 0 WHEN 'namespace' THEN 1 WHEN 'deployment' THEN 2 ELSE 3 END",
	"age":       "COALESCE(completed_at, created_at)",
	"status":    "CASE WHEN resolved_at IS NULL THEN 0 ELSE 1 END",
}

// fetchReportTableData is a lightweight version of fetchDashboardData that only
// queries the reports table (no stats, no incidents). Used for partial refreshes.
func (h *Handler) fetchReportTableData(r *http.Request) (DashboardData, error) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 20, 1, 100)
	offset := intParam(q.Get("offset"), 0, 0, 10000)
	severity := q.Get("severity")
	search := q.Get("search")
	status := q.Get("status")
	sort := q.Get("sort")
	dir := q.Get("dir")

	// Validate sort field.
	if _, ok := validSortFields[sort]; !ok {
		sort = ""
	}
	if dir != "asc" && dir != "desc" {
		dir = "desc"
	}

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
	switch status {
	case "active":
		where += " AND state IN ('processing', 'reported', 'acknowledged')"
	case "resolved":
		where += " AND state = 'resolved'"
	case "processing":
		where += " AND state = 'processing'"
	case "acknowledged":
		where += " AND state = 'acknowledged'"
	case "reported":
		where += " AND state = 'reported'"
	default:
		// Default: show active + recently resolved (last 24h).
		where += " AND (state != 'resolved' OR resolved_at > NOW() - INTERVAL '24 hours')"
	}

	var totalCount int
	countQuery := "SELECT COUNT(*) FROM triage.reports " + where
	if err := h.db.QueryRowContext(r.Context(), countQuery, args...).Scan(&totalCount); err != nil {
		return DashboardData{}, fmt.Errorf("count: %w", err)
	}

	orderBy := `ORDER BY CASE WHEN resolved_at IS NULL THEN 0 ELSE 1 END,
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			created_at DESC`
	if sort != "" {
		orderBy = fmt.Sprintf("ORDER BY %s %s", validSortFields[sort], dir)
	}

	dataQuery := fmt.Sprintf(`SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at, summary, blast_radius, state,
		assigned_to, acknowledged_at, escalation_level
		FROM triage.reports %s
		%s
		LIMIT $%d OFFSET $%d`, where, orderBy, argIdx, argIdx+1)
	dataArgs := make([]interface{}, len(args)+2)
	copy(dataArgs, args)
	dataArgs[len(args)] = limit
	dataArgs[len(args)+1] = offset

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
		Sort:       sort,
		Dir:        dir,
		SSEEnabled: h.sse != nil,
	}, nil
}

// fetchStats runs aggregate queries for KPI cards and charts.
func (h *Handler) fetchStats(ctx context.Context) (StatsData, error) {
	var s StatsData

	// Single query for all counts using PostgreSQL FILTER.
	err := h.db.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE state IN ('processing', 'reported', 'acknowledged')),
		COUNT(*) FILTER (WHERE state = 'resolved'),
		COUNT(*) FILTER (WHERE escalation_needed = true AND state != 'resolved'),
		COUNT(*) FILTER (WHERE state = 'acknowledged'),
		COUNT(*) FILTER (WHERE state = 'reported'),
		COUNT(*) FILTER (WHERE state = 'processing'),
		COUNT(*) FILTER (WHERE severity = 'critical' AND state != 'resolved'),
		COUNT(*) FILTER (WHERE severity = 'warning' AND state != 'resolved'),
		COUNT(*) FILTER (WHERE severity = 'info' AND state != 'resolved'),
		COUNT(*),
		COUNT(*) FILTER (WHERE blast_radius = 'cluster' AND state != 'resolved'),
		COUNT(*) FILTER (WHERE blast_radius = 'namespace' AND state != 'resolved'),
		COUNT(*) FILTER (WHERE blast_radius = 'deployment' AND state != 'resolved'),
		COUNT(*) FILTER (WHERE blast_radius = 'pod' AND state != 'resolved'),
		COALESCE(AVG(EXTRACT(EPOCH FROM (resolved_at - started_at))) FILTER (WHERE state = 'resolved'), 0)
		FROM triage.reports`).Scan(
		&s.ActiveCount, &s.ResolvedCount, &s.EscalatedCount,
		&s.AcknowledgedCount, &s.ReportedCount, &s.ProcessingCount,
		&s.CriticalCount, &s.WarningCount, &s.InfoCount, &s.TotalCount,
		&s.BlastCluster, &s.BlastNamespace, &s.BlastDeployment, &s.BlastPod,
		&s.MTTRSeconds,
	)
	if err != nil {
		return s, fmt.Errorf("stats counts: %w", err)
	}

	s.MTTRDisplay = formatDuration(s.MTTRSeconds)
	if s.TotalCount > 0 {
		s.ResolutionRate = float64(s.ResolvedCount) / float64(s.TotalCount) * 100
	}

	// Classification breakdown (active/reported only).
	classRows, err := h.db.QueryContext(ctx, `SELECT classification, COUNT(*) as cnt
		FROM triage.reports WHERE state = 'reported'
		GROUP BY classification ORDER BY cnt DESC`)
	if err != nil {
		return s, fmt.Errorf("classification: %w", err)
	}
	defer classRows.Close()
	for classRows.Next() {
		var nc NameCount
		if err := classRows.Scan(&nc.Name, &nc.Count); err != nil {
			return s, fmt.Errorf("scan classification: %w", err)
		}
		if s.ActiveCount > 0 {
			nc.Pct = float64(nc.Count) / float64(s.ActiveCount) * 100
		}
		s.Classifications = append(s.Classifications, nc)
	}
	if err := classRows.Err(); err != nil {
		return s, fmt.Errorf("classification rows: %w", err)
	}

	// Daily counts for sparkline (last 14 days, completed reports only).
	sparkRows, err := h.db.QueryContext(ctx, `SELECT d::date, COUNT(r.id)
		FROM generate_series(CURRENT_DATE - INTERVAL '13 days', CURRENT_DATE, '1 day') AS d
		LEFT JOIN triage.reports r ON r.created_at::date = d::date AND r.state IN ('reported', 'resolved')
		GROUP BY d::date ORDER BY d::date`)
	if err != nil {
		return s, fmt.Errorf("sparkline: %w", err)
	}
	defer sparkRows.Close()
	for sparkRows.Next() {
		var day time.Time
		var cnt int
		if err := sparkRows.Scan(&day, &cnt); err != nil {
			return s, fmt.Errorf("scan sparkline: %w", err)
		}
		s.DailyCounts = append(s.DailyCounts, cnt)
	}
	if err := sparkRows.Err(); err != nil {
		return s, fmt.Errorf("sparkline rows: %w", err)
	}

	s.SparklinePoints = computeSparkline(s.DailyCounts, 200, 50)
	return s, nil
}

func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

func computeSparkline(counts []int, width, height int) string {
	if len(counts) == 0 {
		return fmt.Sprintf("0,%d %d,%d", height, width, height)
	}
	if len(counts) == 1 {
		// Single data point → flat line at mid-height
		y := float64(height) / 2
		return fmt.Sprintf("0,%.0f %d,%.0f", y, width, y)
	}
	maxVal := 1
	for _, c := range counts {
		if c > maxVal {
			maxVal = c
		}
	}
	step := float64(width) / float64(len(counts)-1)
	pts := make([]string, len(counts))
	for i, c := range counts {
		x := float64(i) * step
		y := float64(height) - (float64(c)/float64(maxVal))*float64(height)*0.85
		pts[i] = fmt.Sprintf("%.0f,%.0f", x, math.Max(y, 2))
	}
	return strings.Join(pts, " ")
}

// fetchReport queries a single report by ID or workflow_id.
func (h *Handler) fetchReport(ctx context.Context, idStr string) (*Report, error) {
	query := `SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at, summary, blast_radius, state,
		assigned_to, acknowledged_at, escalation_level
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
		var resolvedAt, completedAt, acknowledgedAt sql.NullTime
		var assignedTo, escalationLevel sql.NullString

		err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.Namespace, &r.Workload, &r.Kind, &r.AlertName,
			&r.Classification, &r.Severity, &r.RootCause,
			&causalChainJSON, &evidenceJSON, &recommendationsJSON,
			&r.Confidence, &r.EscalationNeeded, &r.AlertCount,
			&r.StartedAt, &completedAt, &r.CreatedAt, &resolvedAt,
			&r.Summary, &r.BlastRadius, &r.State,
			&assignedTo, &acknowledgedAt, &escalationLevel,
		)
		if err != nil {
			return nil, err
		}

		if resolvedAt.Valid {
			r.ResolvedAt = &resolvedAt.Time
		}
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if acknowledgedAt.Valid {
			r.AcknowledgedAt = &acknowledgedAt.Time
		}
		if assignedTo.Valid {
			r.AssignedTo = assignedTo.String
		}
		if escalationLevel.Valid {
			r.EscalationLevel = escalationLevel.String
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
		"timeAgo": func(v interface{}) string {
			var t time.Time
			switch val := v.(type) {
			case time.Time:
				t = val
			case *time.Time:
				if val == nil {
					return "—"
				}
				t = *val
			default:
				return "—"
			}
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
		// SAFETY: all return values are hardcoded HTML, no user input.
		"blastDots": func(b string) template.HTML {
			switch b {
			case "cluster":
				return template.HTML(`<span aria-hidden="true" class="text-error">●●●●</span><span class="sr-only">cluster</span>`)
			case "namespace":
				return template.HTML(`<span aria-hidden="true" class="text-warning">●●●</span><span class="sr-only">namespace</span>`)
			case "deployment":
				return template.HTML(`<span aria-hidden="true" class="text-info">●●</span><span class="sr-only">deployment</span>`)
			default:
				return template.HTML(`<span aria-hidden="true" class="text-success">●</span><span class="sr-only">pod</span>`)
			}
		},
		"fmtPct": func(f float64) string {
			return fmt.Sprintf("%.0f", f)
		},
		"incidentStateClass": func(state string) string {
			switch state {
			case "processing":
				return "badge-ghost opacity-60"
			case "reported":
				return "badge-error animate-pulse"
			case "acknowledged":
				return "badge-info"
			case "resolved":
				return "badge-success opacity-70"
			default:
				return "badge-ghost"
			}
		},
		"stateIcon": func(state string) string {
			switch state {
			case "processing":
				return "⏳"
			case "reported":
				return "🔔"
			case "acknowledged":
				return "👤"
			case "resolved":
				return "✓"
			default:
				return "❓"
			}
		},
		"stateLabel": func(state string) string {
			switch state {
			case "processing":
				return "Processing"
			case "reported":
				return "Reported"
			case "acknowledged":
				return "Acknowledged"
			case "resolved":
				return "Resolved"
			default:
				return state
			}
		},
		"sortIndicator": func(col, activeSort, activeDir string) template.HTML {
			if col != activeSort {
				return template.HTML(`<span class="opacity-30">⇅</span>`)
			}
			if activeDir == "asc" {
				return template.HTML(`<span class="text-primary">▲</span>`)
			}
			return template.HTML(`<span class="text-primary">▼</span>`)
		},
		"toggleDir": func(col, activeSort, activeDir string) string {
			if col == activeSort && activeDir == "desc" {
				return "asc"
			}
			return "desc"
		},
		"formatDuration": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
			default:
				return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
			}
		},
	}
}

// handleEvents delegates to SSE broker or returns 503 if unavailable.
func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	if h.sse == nil {
		http.Error(w, "SSE not available", http.StatusServiceUnavailable)
		return
	}
	h.sse.ServeHTTP(w, r)
}

// handlePartialIncidents renders just the incidents table fragment for htmx.
func (h *Handler) handlePartialIncidents(w http.ResponseWriter, r *http.Request) {
	incidents, err := FetchActiveIncidents(r.Context(), h.db)
	if err != nil {
		h.logger.Error("fetch incidents", "error", err)
		h.renderError(w, "Failed to load incidents")
		return
	}
	h.render(w, "incidents-table", map[string]interface{}{
		"Incidents": incidents,
	})
}

// handleResolve processes POST /reports/:id/resolve to mark a report as resolved.
func (h *Handler) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract report ID: /reports/<id>/resolve
	path := strings.TrimPrefix(r.URL.Path, "/reports/")
	idStr := strings.TrimSuffix(path, "/resolve")
	if idStr == "" {
		http.Error(w, "Missing report ID", http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid report ID", http.StatusBadRequest)
		return
	}

	// Parse optional resolution metadata from JSON body.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Note   string `json:"resolution_note"`
		Source string `json:"resolution_source"`
	}
	if r.Body != nil {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType == "application/json" {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
	}
	switch body.Source {
	case "manual", "automated", "escalated", "api":
		// valid
	default:
		body.Source = "manual"
	}

	// Resolve: only transition if not already resolved (race guard).
	result, err := h.db.ExecContext(r.Context(),
		`UPDATE triage.reports
		 SET state = 'resolved', resolved_at = NOW(),
		     resolution_note = COALESCE(NULLIF($2, ''), resolution_note),
		     resolution_source = $3
		 WHERE id = $1 AND state != 'resolved'`, id, body.Note, body.Source)
	if err != nil {
		h.logger.Error("resolve report", "error", err, "id", id)
		http.Error(w, "Failed to resolve", http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Either doesn't exist or already resolved.
		h.logger.Info("resolve: no-op (already resolved or not found)", "id", id)
	} else {
		user := UserFromContext(r.Context())
		email := ""
		if user != nil {
			email = user.Email
		}
		h.logger.Info("report resolved", "id", id, "by", email)

		// Trigger PG NOTIFY so SSE clients refresh.
		_, _ = h.db.ExecContext(r.Context(),
			`SELECT pg_notify('report_changes', json_build_object('id', $1, 'state', 'resolved')::text)`, id)
	}

	// If htmx request, return updated badge + success toast.
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Trigger", "report-resolved")
		fmt.Fprintf(w, `<span class="badge badge-success badge-sm">Resolved</span>`)
		return
	}

	// Non-htmx: redirect back to report detail.
	http.Redirect(w, r, "/reports/"+idStr, http.StatusSeeOther)
}

// handleRetriage triggers a re-triage workflow for an existing incident.
func (h *Handler) handleRetriage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.retriage == nil {
		http.Error(w, `{"error":"re-triage not configured"}`, http.StatusServiceUnavailable)
		return
	}

	id, err := extractIncidentID(r.URL.Path, "/api/incidents/", "/retriage")
	if err != nil {
		http.Error(w, "Invalid incident ID", http.StatusBadRequest)
		return
	}

	// Fetch incident details for re-triage.
	var workflowID, namespace, workload, kind, alertName, state string
	err = h.db.QueryRowContext(r.Context(),
		`SELECT workflow_id, namespace, workload, kind, alert_name, state
		 FROM triage.reports WHERE id = $1`, id).
		Scan(&workflowID, &namespace, &workload, &kind, &alertName, &state)
	if err != nil {
		http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
		return
	}

	// Cannot re-triage a processing incident (already running).
	if state == "processing" {
		http.Error(w, `{"error":"incident is already being processed"}`, http.StatusConflict)
		return
	}

	// Enforce re-triage cap: max 3 versions per workload identity.
	var retriageCount int
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM triage.reports
		 WHERE namespace = $1 AND workload = $2 AND kind = $3 AND alert_name = $4`,
		namespace, workload, kind, alertName).Scan(&retriageCount); err != nil {
		h.logger.Error("retriage cap query failed", "error", err, "incident_id", id)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if retriageCount >= 3 {
		http.Error(w, `{"error":"re-triage limit reached (max 3)"}`, http.StatusTooManyRequests)
		return
	}

	// Start a new triage workflow with a versioned ID.
	newWfID, err := h.retriage.StartRetriage(r.Context(), workflowID, namespace, workload, kind, alertName)
	if err != nil {
		h.logger.Error("start retriage", "error", err, "incident_id", id)
		http.Error(w, `{"error":"failed to start re-triage"}`, http.StatusInternalServerError)
		return
	}

	h.logger.Info("retriage started", "incident_id", id, "new_workflow_id", newWfID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":              id,
		"new_workflow_id": newWfID,
		"status":          "processing",
	})
}

// extractIncidentID parses the incident ID from a path like /api/incidents/:id/action.
func extractIncidentID(path, prefix, suffix string) (int64, error) {
	trimmed := strings.TrimPrefix(path, prefix)
	idStr := strings.TrimSuffix(trimmed, suffix)
	return strconv.ParseInt(idStr, 10, 64)
}

func (h *Handler) handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	id, err := extractIncidentID(r.URL.Path, "/api/incidents/", "/acknowledge")
	if err != nil {
		http.Error(w, "Invalid incident ID", http.StatusBadRequest)
		return
	}

	// Determine assignee: prefer authenticated user, fall back to request body.
	var body struct {
		Assignee string `json:"assignee"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	assignee := body.Assignee
	if user := UserFromContext(r.Context()); user != nil && user.Email != "" {
		assignee = user.Email
	}
	if assignee == "" {
		http.Error(w, `{"error":"assignee required"}`, http.StatusBadRequest)
		return
	}

	// Transition: processing or reported → acknowledged.
	result, err := h.db.ExecContext(r.Context(),
		`UPDATE triage.reports
		 SET state = 'acknowledged', assigned_to = $2, acknowledged_at = NOW()
		 WHERE id = $1 AND state IN ('processing', 'reported')`,
		id, assignee)
	if err != nil {
		h.logger.Error("acknowledge incident", "error", err, "id", id)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, `{"error":"incident not found or already acknowledged/resolved"}`, http.StatusConflict)
		return
	}

	h.logger.Info("incident acknowledged", "id", id, "by", assignee)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":          id,
		"state":       "acknowledged",
		"assigned_to": assignee,
	})
}

func (h *Handler) handleEscalate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	id, err := extractIncidentID(r.URL.Path, "/api/incidents/", "/escalate")
	if err != nil {
		http.Error(w, "Invalid incident ID", http.StatusBadRequest)
		return
	}

	var body struct {
		Level  string `json:"level"`
		Target string `json:"target"`
	}
	if r.Body == nil {
		http.Error(w, `{"error":"request body required"}`, http.StatusBadRequest)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	// Validate escalation level.
	switch body.Level {
	case "L1", "L2", "L3":
	default:
		http.Error(w, `{"error":"level must be L1, L2, or L3"}`, http.StatusBadRequest)
		return
	}
	if body.Target == "" {
		http.Error(w, `{"error":"target required"}`, http.StatusBadRequest)
		return
	}

	// Escalation does NOT change state — only sets attributes.
	result, err := h.db.ExecContext(r.Context(),
		`UPDATE triage.reports
		 SET escalation_level = $2, escalated_to = $3, escalated_at = NOW()
		 WHERE id = $1 AND state IN ('processing', 'reported', 'acknowledged')`,
		id, body.Level, body.Target)
	if err != nil {
		h.logger.Error("escalate incident", "error", err, "id", id)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, `{"error":"incident not found or already resolved"}`, http.StatusConflict)
		return
	}

	h.logger.Info("incident escalated", "id", id, "level", body.Level, "target", body.Target)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":               id,
		"escalation_level": body.Level,
		"escalated_to":     body.Target,
	})
}

func (h *Handler) handleNotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536) // 64KB max for notes

	id, err := extractIncidentID(r.URL.Path, "/api/incidents/", "/notes")
	if err != nil {
		http.Error(w, "Invalid incident ID", http.StatusBadRequest)
		return
	}

	var body struct {
		Body string `json:"body"`
	}
	if r.Body == nil {
		http.Error(w, `{"error":"request body required"}`, http.StatusBadRequest)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if body.Body == "" {
		http.Error(w, `{"error":"body is required"}`, http.StatusBadRequest)
		return
	}

	// Determine author from auth context.
	author := "anonymous"
	if user := UserFromContext(r.Context()); user != nil && user.Email != "" {
		author = user.Email
	}

	var noteID int64
	err = h.db.QueryRowContext(r.Context(),
		`INSERT INTO triage.incident_notes (incident_id, author, body)
		 VALUES ($1, $2, $3) RETURNING id`,
		id, author, body.Body).Scan(&noteID)
	if err != nil {
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
			return
		}
		h.logger.Error("add note", "error", err, "incident_id", id)
		http.Error(w, `{"error":"failed to add note"}`, http.StatusInternalServerError)
		return
	}

	h.logger.Info("note added", "incident_id", id, "note_id", noteID, "author", author)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":          noteID,
		"incident_id": id,
		"author":      author,
	})
}

// cacheHeaders wraps an http.Handler to add immutable cache headers for static assets.
func cacheHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}
