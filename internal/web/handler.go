package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	triageworkflow "github.com/haakotsm/triage-worker/internal/workflow"
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
	ActiveCount    int
	ResolvedCount  int
	EscalatedCount int
	CriticalCount  int
	WarningCount   int
	InfoCount      int
	TotalCount     int

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
	SSEEnabled bool
}

// DetailData is the template data for the report detail page.
type DetailData struct {
	Report     Report
	L1Commands []Recommendation
	AgentRecs  []Recommendation
	Incident   *Incident
	LiveStatus triageworkflow.WorkflowStatus
	SSEEnabled bool
}

// Handler serves the web dashboard and static assets.
type Handler struct {
	pages    map[string]*template.Template // per-page template sets (clone of shared + page-specific)
	partials *template.Template            // shared partials for htmx fragment renders
	static   http.Handler
	db       *sql.DB
	logger   *slog.Logger
	temporal client.Client
	sse      *SSEBroker // optional: nil if SSE not configured
}

// SetSSEBroker attaches an SSE broker for realtime event streaming.
func (h *Handler) SetSSEBroker(b *SSEBroker) {
	h.sse = b
}

// SetTemporalClient attaches a Temporal client for live workflow queries.
func (h *Handler) SetTemporalClient(tc client.Client) {
	h.temporal = tc
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
		static:   http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
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
	case strings.HasPrefix(r.URL.Path, "/incidents/") && strings.HasSuffix(r.URL.Path, "/status"):
		h.handleIncidentStatus(w, r)
	case strings.HasPrefix(r.URL.Path, "/incidents/"):
		h.handleIncident(w, r)
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

func (h *Handler) handleIncident(w http.ResponseWriter, r *http.Request) {
	workflowID := decodeWorkflowPath(strings.TrimPrefix(r.URL.Path, "/incidents/"))
	if workflowID == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	data, redirectURL, found, err := h.loadIncidentLiveData(r.Context(), workflowID)
	if err != nil {
		h.logger.Error("fetch incident", "error", err, "workflow_id", workflowID)
		h.renderError(w, "Failed to load incident")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if redirectURL != "" {
		redirectRequest(w, r, redirectURL)
		return
	}

	if isHTMX(r) {
		h.render(w, "incident-live", data)
	} else {
		h.render(w, "detail", data)
	}
}

func (h *Handler) handleIncidentStatus(w http.ResponseWriter, r *http.Request) {
	rawID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/incidents/"), "/status")
	workflowID := decodeWorkflowPath(rawID)
	if workflowID == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/incidents/"+encodeWorkflowPath(workflowID), http.StatusFound)
		return
	}

	data, redirectURL, found, err := h.loadIncidentLiveData(r.Context(), workflowID)
	if err != nil {
		h.logger.Error("fetch incident status", "error", err, "workflow_id", workflowID)
		h.renderError(w, "Failed to load incident status")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if redirectURL != "" {
		redirectRequest(w, r, redirectURL)
		return
	}

	h.render(w, "incident-live-panel", data)
}

func (h *Handler) loadIncidentLiveData(ctx context.Context, workflowID string) (DetailData, string, bool, error) {
	incident, err := h.fetchIncident(ctx, workflowID)
	if err != nil {
		return DetailData{}, "", false, err
	}
	if incident == nil {
		return DetailData{}, "", false, nil
	}
	if isTerminalIncidentState(incident.State) {
		return DetailData{}, reportPath(incident.ID), true, nil
	}

	status := h.queryWorkflowStatus(ctx, *incident)
	if status.Step == "complete" {
		return DetailData{}, reportPath(incident.ID), true, nil
	}

	return DetailData{
		Incident:   incident,
		LiveStatus: status,
		SSEEnabled: h.sse != nil,
	}, "", true, nil
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

// fetchDashboardData queries the data needed for the dashboard page.
func (h *Handler) fetchDashboardData(r *http.Request) (DashboardData, error) {
	data, err := h.fetchIncidentTableData(r)
	if err != nil {
		return DashboardData{}, err
	}

	stats, err := h.fetchStats(r.Context())
	if err != nil {
		h.logger.Warn("fetch stats for dashboard", "error", err)
	}

	data.Stats = stats
	data.SSEEnabled = h.sse != nil
	return data, nil
}

func (h *Handler) fetchIncidentTableData(r *http.Request) (DashboardData, error) {
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
	switch status {
	case "active":
		where += " AND resolved_at IS NULL"
	case "resolved":
		where += " AND resolved_at IS NOT NULL"
	}

	var totalCount int
	countQuery := "SELECT COUNT(*) FROM triage.reports " + where + " AND state IN ('reported', 'resolved')"
	if err := h.db.QueryRowContext(r.Context(), countQuery, args...).Scan(&totalCount); err != nil {
		return DashboardData{}, fmt.Errorf("count: %w", err)
	}

	dataQuery := fmt.Sprintf(`SELECT id, workflow_id, namespace, workload, kind, alert_name,
		classification, severity, root_cause, causal_chain, evidence,
		recommendations, confidence, escalation_needed, alert_count,
		started_at, completed_at, created_at, resolved_at, summary, blast_radius, state
		FROM triage.reports %s AND state IN ('reported', 'resolved')
		ORDER BY CASE WHEN resolved_at IS NULL THEN 0 ELSE 1 END,
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			created_at DESC
		LIMIT $%d OFFSET $%d`, where, argIdx, argIdx+1)
	dataArgs := make([]interface{}, len(args)+2)
	copy(dataArgs, args)
	dataArgs[len(args)] = limit
	dataArgs[len(args)+1] = offset

	reports, err := h.queryReports(r.Context(), dataQuery, dataArgs...)
	if err != nil {
		return DashboardData{}, fmt.Errorf("query reports: %w", err)
	}

	incidents := []Incident{}
	if status != "resolved" {
		incidents, err = h.queryActiveIncidents(r.Context(), severity, search)
		if err != nil {
			return DashboardData{}, fmt.Errorf("query active incidents: %w", err)
		}
	}

	return DashboardData{
		Reports:    reports,
		Incidents:  incidents,
		TotalCount: totalCount,
		Limit:      limit,
		Offset:     offset,
		Query:      search,
		Severity:   severity,
		Status:     status,
		SSEEnabled: h.sse != nil,
	}, nil
}

// fetchStats runs aggregate queries for KPI cards and charts.
func (h *Handler) fetchStats(ctx context.Context) (StatsData, error) {
	var s StatsData

	// Single query for all counts using PostgreSQL FILTER.
	// Only count completed reports (state IN reported, resolved) — not in-flight.
	err := h.db.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE state = 'reported'),
		COUNT(*) FILTER (WHERE state = 'resolved'),
		COUNT(*) FILTER (WHERE escalation_needed = true AND state = 'reported'),
		COUNT(*) FILTER (WHERE severity = 'critical' AND state = 'reported'),
		COUNT(*) FILTER (WHERE severity = 'warning' AND state = 'reported'),
		COUNT(*) FILTER (WHERE severity = 'info' AND state = 'reported'),
		COUNT(*) FILTER (WHERE state IN ('reported', 'resolved')),
		COUNT(*) FILTER (WHERE blast_radius = 'cluster' AND state = 'reported'),
		COUNT(*) FILTER (WHERE blast_radius = 'namespace' AND state = 'reported'),
		COUNT(*) FILTER (WHERE blast_radius = 'deployment' AND state = 'reported'),
		COUNT(*) FILTER (WHERE blast_radius = 'pod' AND state = 'reported'),
		COALESCE(AVG(EXTRACT(EPOCH FROM (resolved_at - started_at))) FILTER (WHERE state = 'resolved'), 0)
		FROM triage.reports`).Scan(
		&s.ActiveCount, &s.ResolvedCount, &s.EscalatedCount,
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
		started_at, completed_at, created_at, resolved_at, summary, blast_radius, state
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
		var resolvedAt, completedAt sql.NullTime

		err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.Namespace, &r.Workload, &r.Kind, &r.AlertName,
			&r.Classification, &r.Severity, &r.RootCause,
			&causalChainJSON, &evidenceJSON, &recommendationsJSON,
			&r.Confidence, &r.EscalationNeeded, &r.AlertCount,
			&r.StartedAt, &completedAt, &r.CreatedAt, &resolvedAt,
			&r.Summary, &r.BlastRadius, &r.State,
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

func (h *Handler) queryActiveIncidents(ctx context.Context, severity, search string) ([]Incident, error) {
	where := "WHERE state IN ('correlating', 'enriching', 'triaging')"
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
	}

	rows, err := h.db.QueryContext(ctx, `SELECT id, workflow_id, namespace, workload, kind, alert_name, state, severity, created_at,
		COALESCE(completed_at, created_at) AS updated_at
		FROM triage.reports `+where+`
		ORDER BY created_at DESC
		LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var incidents []Incident
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.WorkflowID, &inc.Namespace, &inc.Workload,
			&inc.Kind, &inc.AlertName, &inc.State, &inc.Severity,
			&inc.CreatedAt, &inc.UpdatedAt); err != nil {
			return nil, err
		}
		incidents = append(incidents, inc)
	}
	if incidents == nil {
		incidents = []Incident{}
	}
	return incidents, rows.Err()
}

func (h *Handler) fetchIncident(ctx context.Context, workflowID string) (*Incident, error) {
	row := h.db.QueryRowContext(ctx, `SELECT id, workflow_id, namespace, workload, kind, alert_name, state, severity, created_at,
		COALESCE(completed_at, created_at) AS updated_at
		FROM triage.reports
		WHERE workflow_id = $1`, workflowID)

	var incident Incident
	if err := row.Scan(
		&incident.ID, &incident.WorkflowID, &incident.Namespace, &incident.Workload,
		&incident.Kind, &incident.AlertName, &incident.State, &incident.Severity,
		&incident.CreatedAt, &incident.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &incident, nil
}

func (h *Handler) queryWorkflowStatus(ctx context.Context, incident Incident) triageworkflow.WorkflowStatus {
	status := triageworkflow.WorkflowStatus{
		Step:      incident.State,
		ElapsedMs: time.Since(incident.CreatedAt).Milliseconds(),
		Resolved:  incident.State == "resolved",
	}
	if status.Step == "" {
		status.Step = "correlating"
	}
	if h.temporal == nil {
		return status
	}

	encoded, err := h.temporal.QueryWorkflow(ctx, incident.WorkflowID, "", triageworkflow.StatusQueryName)
	if err != nil {
		h.logger.Warn("query workflow status", "workflow_id", incident.WorkflowID, "error", err)
		return status
	}

	var live triageworkflow.WorkflowStatus
	if err := encoded.Get(&live); err != nil {
		h.logger.Warn("decode workflow status", "workflow_id", incident.WorkflowID, "error", err)
		return status
	}
	if live.Step == "" {
		live.Step = status.Step
	}
	if live.ElapsedMs <= 0 {
		live.ElapsedMs = status.ElapsedMs
	}
	if incident.State == "resolved" {
		live.Resolved = true
	}
	return live
}

func encodeWorkflowPath(workflowID string) string {
	return strings.ReplaceAll(workflowID, "/", "|")
}

func decodeWorkflowPath(workflowID string) string {
	return strings.ReplaceAll(strings.Trim(workflowID, "/"), "|", "/")
}

func reportPath(id int64) string {
	return "/reports/" + strconv.FormatInt(id, 10)
}

func redirectRequest(w http.ResponseWriter, r *http.Request, location string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

func isTerminalIncidentState(state string) bool {
	switch state {
	case "reported", "resolved", "failed":
		return true
	default:
		return false
	}
}

func incidentStepRank(step string) int {
	switch step {
	case "correlating":
		return 1
	case "enriching":
		return 2
	case "triaging":
		return 3
	case "reporting":
		return 4
	case "complete", "reported", "resolved", "failed":
		return 5
	default:
		return 0
	}
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
		"mul": func(a, b int) int { return a * b },
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
		"workflowPath": encodeWorkflowPath,
		"stepReached": func(current, target string) bool {
			return incidentStepRank(current) >= incidentStepRank(target)
		},
		"incidentStateClass": func(state string) string {
			switch state {
			case "correlating":
				return "badge-warning"
			case "enriching":
				return "badge-info"
			case "triaging":
				return "badge-primary"
			case "reporting":
				return "badge-secondary"
			case "reported", "resolved", "complete":
				return "badge-success"
			case "failed":
				return "badge-error"
			default:
				return "badge-ghost"
			}
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

// handlePartialIncidents renders the main incidents table fragment for htmx.
func (h *Handler) handlePartialIncidents(w http.ResponseWriter, r *http.Request) {
	data, err := h.fetchIncidentTableData(r)
	if err != nil {
		h.logger.Error("fetch incidents table", "error", err)
		h.renderError(w, "Failed to load incidents")
		return
	}
	h.render(w, "incidents-table", data)
}
