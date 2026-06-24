package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNormalizeWebhookResult(t *testing.T) {
	cases := map[string]string{
		ResultAccepted: ResultAccepted,
		ResultResolved: ResultResolved,
		ResultPaused:   ResultPaused,
		ResultRejected: ResultRejected,
		ResultError:    ResultError,
		"":             ResultError,
		"bogus":        ResultError,
	}
	for in, want := range cases {
		if got := normalizeWebhookResult(in); got != want {
			t.Errorf("normalizeWebhookResult(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeDecision(t *testing.T) {
	cases := map[string]string{
		DecisionSignaled:     DecisionSignaled,
		DecisionFlapSkipped:  DecisionFlapSkipped,
		DecisionResolveError: DecisionResolveError,
		DecisionSignalError:  DecisionSignalError,
		"":                   DecisionSignalError,
		"nope":               DecisionSignalError,
	}
	for in, want := range cases {
		if got := normalizeDecision(in); got != want {
			t.Errorf("normalizeDecision(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeOutcome(t *testing.T) {
	if got := normalizeOutcome(OutcomeSuccess); got != OutcomeSuccess {
		t.Errorf("normalizeOutcome(success) = %q, want success", got)
	}
	for _, in := range []string{OutcomeError, "", "weird"} {
		if got := normalizeOutcome(in); got != OutcomeError {
			t.Errorf("normalizeOutcome(%q) = %q, want error", in, got)
		}
	}
}

func TestNormalizeSource(t *testing.T) {
	cases := map[string]string{
		SourcePrometheus: SourcePrometheus,
		SourceLoki:       SourceLoki,
		SourceK8s:        SourceK8s,
		"":               "other",
		"elastic":        "other",
	}
	for in, want := range cases {
		if got := normalizeSource(in); got != want {
			t.Errorf("normalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSeverity(t *testing.T) {
	for _, valid := range []string{"critical", "warning", "info"} {
		if got := normalizeSeverity(valid); got != valid {
			t.Errorf("normalizeSeverity(%q) = %q, want passthrough", valid, got)
		}
	}
	for _, in := range []string{"", "SEV1", "high"} {
		if got := normalizeSeverity(in); got != "unknown" {
			t.Errorf("normalizeSeverity(%q) = %q, want unknown", in, got)
		}
	}
}

func TestNormalizeClassification(t *testing.T) {
	for _, valid := range []string{"CrashLoop", "OOM", "Network", "Config"} {
		if got := normalizeClassification(valid); got != valid {
			t.Errorf("normalizeClassification(%q) = %q, want passthrough", valid, got)
		}
	}
	for _, in := range []string{"", "Gremlins", "oom"} {
		if got := normalizeClassification(in); got != "Unknown" {
			t.Errorf("normalizeClassification(%q) = %q, want Unknown", in, got)
		}
	}
}

func TestRecordWebhookRequest(t *testing.T) {
	before := testutil.ToFloat64(webhookRequestsTotal.WithLabelValues(ResultAccepted))
	RecordWebhookRequest(ResultAccepted, 10*time.Millisecond)
	if got := testutil.ToFloat64(webhookRequestsTotal.WithLabelValues(ResultAccepted)); got != before+1 {
		t.Errorf("accepted counter = %v, want %v", got, before+1)
	}

	// Unknown result coerces to "error".
	before = testutil.ToFloat64(webhookRequestsTotal.WithLabelValues(ResultError))
	RecordWebhookRequest("not-a-real-result", time.Millisecond)
	if got := testutil.ToFloat64(webhookRequestsTotal.WithLabelValues(ResultError)); got != before+1 {
		t.Errorf("error counter = %v, want %v", got, before+1)
	}
}

func TestRecordWebhookDecision(t *testing.T) {
	before := testutil.ToFloat64(webhookDecisionsTotal.WithLabelValues(DecisionSignaled))
	RecordWebhookDecision(DecisionSignaled)
	if got := testutil.ToFloat64(webhookDecisionsTotal.WithLabelValues(DecisionSignaled)); got != before+1 {
		t.Errorf("signaled decision = %v, want %v", got, before+1)
	}
}

func TestRecordAgentInvocation(t *testing.T) {
	before := testutil.ToFloat64(agentInvocationsTotal.WithLabelValues(OutcomeError))
	RecordAgentInvocation("anything-non-success", time.Second)
	if got := testutil.ToFloat64(agentInvocationsTotal.WithLabelValues(OutcomeError)); got != before+1 {
		t.Errorf("agent error counter = %v, want %v", got, before+1)
	}
}

func TestRecordEnrichment(t *testing.T) {
	before := testutil.ToFloat64(enrichmentTotal.WithLabelValues(SourceLoki, OutcomeSuccess))
	RecordEnrichment(SourceLoki, OutcomeSuccess, 5*time.Millisecond)
	if got := testutil.ToFloat64(enrichmentTotal.WithLabelValues(SourceLoki, OutcomeSuccess)); got != before+1 {
		t.Errorf("loki success counter = %v, want %v", got, before+1)
	}

	// Unknown source collapses to "other".
	before = testutil.ToFloat64(enrichmentTotal.WithLabelValues("other", OutcomeError))
	RecordEnrichment("cassandra", OutcomeError, time.Millisecond)
	if got := testutil.ToFloat64(enrichmentTotal.WithLabelValues("other", OutcomeError)); got != before+1 {
		t.Errorf("other/error counter = %v, want %v", got, before+1)
	}
}

func TestRecordAgentTokens(t *testing.T) {
	beforePrompt := testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypePrompt))
	beforeCompletion := testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypeCompletion))

	RecordAgentTokens(946, 199, 1145)

	if got := testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypePrompt)); got != beforePrompt+946 {
		t.Errorf("prompt tokens = %v, want %v", got, beforePrompt+946)
	}
	if got := testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypeCompletion)); got != beforeCompletion+199 {
		t.Errorf("completion tokens = %v, want %v", got, beforeCompletion+199)
	}

	// Non-positive counts are ignored (no panic, no negative/zero additions).
	beforePrompt = testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypePrompt))
	RecordAgentTokens(0, -5, 0)
	if got := testutil.ToFloat64(agentTokensTotal.WithLabelValues(TokenTypePrompt)); got != beforePrompt {
		t.Errorf("prompt tokens changed on zero input: %v != %v", got, beforePrompt)
	}
}

func TestRecordAgentTokenUsageMissing(t *testing.T) {
	before := testutil.ToFloat64(agentTokenUsageMissingTotal)
	RecordAgentTokenUsageMissing()
	if got := testutil.ToFloat64(agentTokenUsageMissingTotal); got != before+1 {
		t.Errorf("missing counter = %v, want %v", got, before+1)
	}
}

func TestRecordReportClassification(t *testing.T) {
	before := testutil.ToFloat64(reportClassificationsTotal.WithLabelValues("critical", "OOM"))
	RecordReportClassification("critical", "OOM")
	if got := testutil.ToFloat64(reportClassificationsTotal.WithLabelValues("critical", "OOM")); got != before+1 {
		t.Errorf("critical/OOM counter = %v, want %v", got, before+1)
	}

	// Out-of-taxonomy labels collapse to unknown/Unknown.
	before = testutil.ToFloat64(reportClassificationsTotal.WithLabelValues("unknown", "Unknown"))
	RecordReportClassification("SEV-9", "Aliens")
	if got := testutil.ToFloat64(reportClassificationsTotal.WithLabelValues("unknown", "Unknown")); got != before+1 {
		t.Errorf("unknown/Unknown counter = %v, want %v", got, before+1)
	}
}
