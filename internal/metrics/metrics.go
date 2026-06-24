// Package metrics defines the triage pipeline's domain ("business") Prometheus
// metrics — webhook ingest decisions, agent invocations, enrichment sub-source
// health, and report classifications. They complement the Temporal SDK metrics
// (workflow/activity timing, see internal/telemetry) and the web/HTTP metrics
// (internal/web). All metrics register on the default Prometheus registry and
// are exposed on the worker's existing /metrics endpoint.
//
// Every label value is normalized to a bounded set (see the normalize helpers)
// so metric cardinality stays fixed regardless of payload content.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/haakotsm/triage-worker/internal/types"
)

const namespace = "triage"

// Webhook ingest decision label values. Bounded set.
const (
	DecisionSignaled     = "signaled"      // a triage workflow was signal-with-started
	DecisionFlapSkipped  = "flap_skipped"  // re-fire suppressed within the flap window
	DecisionResolveError = "resolve_error" // resolving the workflow id failed
	DecisionSignalError  = "signal_error"  // signal-with-start failed
)

// Webhook request result label values. Bounded set.
const (
	ResultAccepted = "accepted" // firing alerts accepted, workflows signaled
	ResultResolved = "resolved" // resolved-only group processed
	ResultPaused   = "paused"   // kill-switch active, firing alerts skipped
	ResultRejected = "rejected" // auth/validation/method rejection (4xx)
	ResultError    = "error"    // server-side failure (5xx)
)

// Enrichment sub-source label values. Bounded set.
const (
	SourcePrometheus = "prometheus"
	SourceLoki       = "loki"
	SourceK8s        = "k8s"
)

// Generic success/error result label values used by activity metrics.
const (
	OutcomeSuccess = "success"
	OutcomeError   = "error"
)

// Token type label values for agent token accounting. Bounded set.
const (
	TokenTypePrompt     = "prompt"
	TokenTypeCompletion = "completion"
)

var (
	webhookRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "requests_total",
		Help:      "Alertmanager webhook requests by terminal result.",
	}, []string{"result"})

	webhookRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "request_duration_seconds",
		Help:      "Alertmanager webhook handling duration in seconds by result.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"result"})

	webhookDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "decisions_total",
		Help:      "Per-incident routing decisions made while handling a webhook.",
	}, []string{"decision"})

	agentInvocationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "invocations_total",
		Help:      "kagent triage-agent invocations by outcome.",
	}, []string{"result"})

	agentInvocationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "invocation_duration_seconds",
		Help:      "kagent triage-agent invocation duration in seconds by outcome.",
		// Agent calls hit an LLM and can take many seconds; extend the default
		// buckets up to the activity-level ceiling.
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300},
	}, []string{"result"})

	agentTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "tokens_total",
		Help:      "LLM tokens consumed by triage-agent calls, by type (prompt/completion).",
	}, []string{"type"})

	agentTokensPerRequest = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "tokens_per_request",
		Help:      "Total LLM tokens (prompt+completion) consumed per triage-agent call.",
		Buckets:   []float64{100, 250, 500, 1000, 2000, 4000, 8000, 16000, 32000, 64000},
	})

	agentTokenUsageMissingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "token_usage_missing_total",
		Help:      "Triage-agent responses where no parseable token-usage block was found.",
	})

	enrichmentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "enrichment",
		Name:      "queries_total",
		Help:      "Enrichment sub-source queries by source and outcome.",
	}, []string{"source", "result"})

	enrichmentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "enrichment",
		Name:      "query_duration_seconds",
		Help:      "Enrichment sub-source query duration in seconds by source.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"source"})

	reportClassificationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "report",
		Name:      "classifications_total",
		Help:      "Triage reports persisted, by severity and classification.",
	}, []string{"severity", "classification"})
)

// RecordWebhookRequest records the terminal result and duration of a webhook
// request. result should be one of the Result* constants; unknown values are
// coerced to "error" to keep cardinality bounded.
func RecordWebhookRequest(result string, d time.Duration) {
	result = normalizeWebhookResult(result)
	webhookRequestsTotal.WithLabelValues(result).Inc()
	webhookRequestDuration.WithLabelValues(result).Observe(d.Seconds())
}

// RecordWebhookDecision increments the per-incident decision counter. decision
// should be one of the Decision* constants; unknown values are coerced to
// "signal_error".
func RecordWebhookDecision(decision string) {
	webhookDecisionsTotal.WithLabelValues(normalizeDecision(decision)).Inc()
}

// RecordAgentInvocation records the outcome and duration of a triage-agent call.
func RecordAgentInvocation(result string, d time.Duration) {
	result = normalizeOutcome(result)
	agentInvocationsTotal.WithLabelValues(result).Inc()
	agentInvocationDuration.WithLabelValues(result).Observe(d.Seconds())
}

// RecordAgentTokens records LLM token usage for a single triage-agent call:
// prompt/completion split into the typed counter and the combined total into
// the per-request histogram. Non-positive counts are ignored so a partially
// populated usage block can't skew the counters with zeros.
func RecordAgentTokens(prompt, completion, total int) {
	if prompt > 0 {
		agentTokensTotal.WithLabelValues(TokenTypePrompt).Add(float64(prompt))
	}
	if completion > 0 {
		agentTokensTotal.WithLabelValues(TokenTypeCompletion).Add(float64(completion))
	}
	if total > 0 {
		agentTokensPerRequest.Observe(float64(total))
	}
}

// RecordAgentTokenUsageMissing increments the watchdog counter for triage-agent
// responses that carried no parseable token-usage block.
func RecordAgentTokenUsageMissing() {
	agentTokenUsageMissingTotal.Inc()
}

// RecordEnrichment records the outcome and duration of an enrichment sub-source
// query. source should be one of the Source* constants.
func RecordEnrichment(source, result string, d time.Duration) {
	source = normalizeSource(source)
	enrichmentTotal.WithLabelValues(source, normalizeOutcome(result)).Inc()
	enrichmentDuration.WithLabelValues(source).Observe(d.Seconds())
}

// RecordReportClassification counts a persisted triage report. severity and
// classification are normalized to the known taxonomies (types.ValidSeverities
// / types.ValidClassifications); anything else collapses to "unknown" so the
// LLM cannot blow up cardinality with novel labels.
func RecordReportClassification(severity, classification string) {
	reportClassificationsTotal.WithLabelValues(
		normalizeSeverity(severity),
		normalizeClassification(classification),
	).Inc()
}

func normalizeWebhookResult(result string) string {
	switch result {
	case ResultAccepted, ResultResolved, ResultPaused, ResultRejected, ResultError:
		return result
	default:
		return ResultError
	}
}

func normalizeDecision(decision string) string {
	switch decision {
	case DecisionSignaled, DecisionFlapSkipped, DecisionResolveError, DecisionSignalError:
		return decision
	default:
		return DecisionSignalError
	}
}

func normalizeOutcome(result string) string {
	if result == OutcomeSuccess {
		return OutcomeSuccess
	}
	return OutcomeError
}

func normalizeSource(source string) string {
	switch source {
	case SourcePrometheus, SourceLoki, SourceK8s:
		return source
	default:
		return "other"
	}
}

func normalizeSeverity(severity string) string {
	if types.ValidSeverities[severity] {
		return severity
	}
	return "unknown"
}

func normalizeClassification(classification string) string {
	if types.ValidClassifications[classification] {
		return classification
	}
	return "Unknown"
}
