package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "triage",
		Subsystem: "web",
		Name:      "http_request_duration_seconds",
		Help:      "Duration of HTTP requests in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "triage",
		Subsystem: "web",
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests.",
	}, []string{"method", "path", "status"})

	sseActiveClients = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "triage",
		Subsystem: "web",
		Name:      "sse_active_clients",
		Help:      "Number of active SSE client connections.",
	})

	reportsByState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "triage",
		Subsystem: "web",
		Name:      "reports_by_state",
		Help:      "Number of reports by state.",
	}, []string{"state"})

	// maskedErrorsTotal counts genuine server errors (5xx-class) that are
	// returned to htmx clients as HTTP 200 so the error fragment/toast can be
	// swapped (htmx discards non-2xx bodies). These do NOT appear in
	// http_requests_total{status=~"5.."}, so alerting must watch this counter
	// to stay aware of server-side failures behind the dashboard.
	maskedErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "triage",
		Subsystem: "web",
		Name:      "masked_errors_total",
		Help:      "Server errors returned as HTTP 200 to htmx clients (not visible in http_requests_total).",
	}, []string{"kind"})
)

// recordMaskedError increments the masked-error counter. kind identifies the
// surface (e.g. "panel" for renderError, "action" for action handlers).
func recordMaskedError(kind string) {
	maskedErrorsTotal.WithLabelValues(kind).Inc()
}

// MetricsHandler returns the Prometheus metrics HTTP handler.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// InstrumentHandler wraps an http.Handler with Prometheus metrics.
func InstrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)

		path := normalizePath(r.URL.Path)
		status := strconv.Itoa(rw.statusCode)
		duration := time.Since(start).Seconds()

		httpRequestDuration.WithLabelValues(r.Method, path, status).Observe(duration)
		httpRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
	})
}

// SetSSEClientCount updates the SSE active clients gauge.
func SetSSEClientCount(count int) {
	sseActiveClients.Set(float64(count))
}

// SetReportStateCount updates the report state gauge.
func SetReportStateCount(state string, count int) {
	reportsByState.WithLabelValues(state).Set(float64(count))
}

// responseWriter captures the status code for metrics.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.statusCode = http.StatusOK
		rw.written = true
	}
	return rw.ResponseWriter.Write(b)
}

// Unwrap supports http.ResponseController in Go 1.20+.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush implements http.Flusher so streaming handlers (the SSE endpoint) keep
// working when wrapped. The SSE handler does a `w.(http.Flusher)` assertion;
// without this method that assertion would fail and disable streaming.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// normalizePath reduces cardinality by collapsing dynamic :id segments to a
// fixed set of route labels. Every live route the web handler serves maps to a
// distinct label so per-endpoint request/latency metrics are usable; anything
// unrecognized falls through to "/other".
func normalizePath(path string) string {
	switch {
	case path == "/" || path == "/dashboard":
		return "/"
	case path == "/events":
		return "/events"
	case path == "/partials/reports":
		return "/partials/reports"
	case path == "/partials/stats":
		return "/partials/stats"
	case path == "/partials/incidents":
		return "/partials/incidents"
	case strings.HasPrefix(path, "/static/"):
		return "/static/*"
	case strings.HasPrefix(path, "/api/incidents/"):
		switch {
		case strings.HasSuffix(path, "/acknowledge"):
			return "/api/incidents/:id/acknowledge"
		case strings.HasSuffix(path, "/escalate"):
			return "/api/incidents/:id/escalate"
		case strings.HasSuffix(path, "/notes"):
			return "/api/incidents/:id/notes"
		case strings.HasSuffix(path, "/retriage"):
			return "/api/incidents/:id/retriage"
		default:
			return "/api/incidents/:id/*"
		}
	case strings.HasPrefix(path, "/incidents/"):
		if strings.HasSuffix(path, "/resolve") {
			return "/incidents/:id/resolve"
		}
		return "/incidents/:id"
	case strings.HasPrefix(path, "/reports/"):
		// Legacy paths: /reports/:id (301 → /incidents/:id) and the still-served
		// /reports/:id/resolve alias.
		if strings.HasSuffix(path, "/resolve") {
			return "/reports/:id/resolve"
		}
		return "/reports/:id"
	default:
		return "/other"
	}
}
