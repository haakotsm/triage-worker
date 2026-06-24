package activity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/haakotsm/triage-worker/internal/auth"
	"github.com/haakotsm/triage-worker/internal/metrics"
	"github.com/haakotsm/triage-worker/internal/types"
)

// AgentActivity handles invocation of the kagent error-triage-agent via A2A.
type AgentActivity struct {
	AgentURL      string // http://agentgateway.agentgateway.svc:3001/api/a2a/kagent/error-triage-agent
	TokenProvider *auth.TokenProvider
	HTTPClient    *http.Client
}

// a2aRequest is the JSON-RPC request for A2A message/send.
type a2aRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  a2aMessageSend `json:"params"`
}

type a2aMessageSend struct {
	Message a2aMessage `json:"message"`
}

type a2aMessage struct {
	Role     string                     `json:"role"`
	Parts    []a2aPart                  `json:"parts"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
}

type a2aPart struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// a2aResponse is the JSON-RPC response (simplified for non-streaming).
type a2aResponse struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      string     `json:"id"`
	Result  *a2aResult `json:"result,omitempty"`
	Error   *a2aError  `json:"error,omitempty"`
}

type a2aTaskStatus struct {
	State   string      `json:"state"`
	Message *a2aMessage `json:"message,omitempty"`
}

type a2aArtifact struct {
	ArtifactID string                     `json:"artifactId"`
	Parts      []a2aPart                  `json:"parts"`
	Metadata   map[string]json.RawMessage `json:"metadata,omitempty"`
}

type a2aResult struct {
	Status    a2aTaskStatus              `json:"status"`
	Artifacts []a2aArtifact              `json:"artifacts,omitempty"`
	Message   *a2aMessage                `json:"message,omitempty"`
	Metadata  map[string]json.RawMessage `json:"metadata,omitempty"`
}

type a2aError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// tokenUsage is the normalized per-request LLM token accounting extracted from
// an A2A response, regardless of the upstream framework's field naming.
type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// extractTokenUsage probes the candidate metadata locations of an A2A task
// result for an LLM usage block and returns the first one found (nil if none).
//
// kagent runs on Google ADK, which surfaces usage at result.metadata
// (key "kagent_usage_metadata") in Gemini shape — confirmed live against the
// error-triage-agent. We also probe artifact/message metadata and accept
// OpenAI- and Ollama-shaped usage as defensive fallbacks in case the framework
// or model backend changes.
func extractTokenUsage(result *a2aResult) *tokenUsage {
	if result == nil {
		return nil
	}

	candidates := []map[string]json.RawMessage{result.Metadata}
	for i := range result.Artifacts {
		candidates = append(candidates, result.Artifacts[i].Metadata)
	}
	if result.Status.Message != nil {
		candidates = append(candidates, result.Status.Message.Metadata)
	}
	if result.Message != nil {
		candidates = append(candidates, result.Message.Metadata)
	}

	for _, md := range candidates {
		if u := usageFromMetadata(md); u != nil {
			return u
		}
	}
	return nil
}

// usageFromMetadata pulls a normalized tokenUsage out of a single A2A metadata
// map, trying the known usage shapes in priority order. Returns nil if the map
// holds no recognizable, non-empty usage block.
func usageFromMetadata(md map[string]json.RawMessage) *tokenUsage {
	if md == nil {
		return nil
	}

	// Google ADK / Gemini shape (what kagent actually emits today).
	if raw, ok := md["kagent_usage_metadata"]; ok {
		var adk struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		}
		if err := json.Unmarshal(raw, &adk); err == nil {
			if u := newTokenUsage(adk.PromptTokenCount, adk.CandidatesTokenCount, adk.TotalTokenCount); u != nil {
				return u
			}
		}
	}

	// Generic "usage" block: OpenAI shape, with Ollama-native fallback.
	if raw, ok := md["usage"]; ok {
		var u struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptEvalCount  int `json:"prompt_eval_count"`
			EvalCount        int `json:"eval_count"`
		}
		if err := json.Unmarshal(raw, &u); err == nil {
			if tu := newTokenUsage(u.PromptTokens, u.CompletionTokens, u.TotalTokens); tu != nil {
				return tu
			}
			if tu := newTokenUsage(u.PromptEvalCount, u.EvalCount, 0); tu != nil {
				return tu
			}
		}
	}

	return nil
}

// newTokenUsage builds a tokenUsage, deriving total from prompt+completion when
// the upstream block omits it. Returns nil if all counts are non-positive so an
// empty/zero usage block is treated as "absent".
func newTokenUsage(prompt, completion, total int) *tokenUsage {
	if prompt <= 0 && completion <= 0 && total <= 0 {
		return nil
	}
	if total <= 0 {
		total = prompt + completion
	}
	return &tokenUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
}

func (a *AgentActivity) InvokeTriageAgent(ctx context.Context, alerts []types.Alert, enrichment types.EnrichmentResult) (report types.TriageReport, err error) {
	// Record invocation outcome + latency on every return path. The fallback
	// path (unstructured LLM output) returns a nil error and so counts as a
	// success — the agent did respond, just imperfectly.
	start := time.Now()
	defer func() {
		result := metrics.OutcomeSuccess
		if err != nil {
			result = metrics.OutcomeError
		}
		metrics.RecordAgentInvocation(result, time.Since(start))
	}()

	// Build prompt from alerts + enrichment
	prompt := buildAgentPrompt(alerts, enrichment)

	// Get JWT token
	token, err := a.TokenProvider.Token(ctx)
	if err != nil {
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("auth failed: %v", err), "AuthError", err)
	}

	// Build A2A JSON-RPC request
	a2aReq := a2aRequest{
		JSONRPC: "2.0",
		ID:      "triage-1",
		Method:  "message/send",
		Params: a2aMessageSend{
			Message: a2aMessage{
				Role: "user",
				Parts: []a2aPart{
					{Kind: "text", Text: prompt},
				},
			},
		},
	}

	body, err := json.Marshal(a2aReq)
	if err != nil {
		return types.TriageReport{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.AgentURL, bytes.NewReader(body))
	if err != nil {
		return types.TriageReport{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return types.TriageReport{}, fmt.Errorf("agent request: %w", err)
	}
	defer resp.Body.Close()

	// Handle HTTP errors with appropriate retry classification
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("auth rejected: %d", resp.StatusCode), "AuthError", nil)
	case resp.StatusCode == http.StatusBadRequest:
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("client error: %d", resp.StatusCode), "ClientError", nil)
	case resp.StatusCode == http.StatusTooManyRequests:
		return types.TriageReport{}, fmt.Errorf("rate limited (429), will retry")
	case resp.StatusCode >= 500:
		return types.TriageReport{}, fmt.Errorf("server error: %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return types.TriageReport{}, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	// Handle response — may be SSE stream or plain JSON
	contentType := resp.Header.Get("Content-Type")
	var agentText string
	var usage *tokenUsage

	if strings.Contains(contentType, "text/event-stream") {
		agentText, usage, err = readSSEResponse(resp.Body)
	} else {
		agentText, usage, err = readJSONRPCResponse(resp.Body)
	}
	if err != nil {
		return types.TriageReport{}, err
	}

	// Record token usage regardless of whether the report parses below — the
	// tokens were spent either way. A missing usage block trips the watchdog
	// counter but never fails the request.
	if usage != nil {
		metrics.RecordAgentTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	} else {
		metrics.RecordAgentTokenUsageMissing()
	}

	// Strip markdown code fences — small LLMs often wrap JSON in ```json ... ```
	agentText = stripMarkdownJSON(agentText)

	// Parse agent response with flexible unmarshaling (LLM output is non-deterministic)
	report, err = parseTriageReport(agentText)
	if err != nil {
		// Fallback: small models sometimes return plain prose instead of JSON.
		// Create a minimal report with the raw text rather than failing entirely.
		if len(agentText) > 20 {
			return types.TriageReport{
				Classification: "Unknown",
				Severity:       "warning",
				RootCause:      truncate(agentText, 500),
				CausalChain:    []string{"LLM returned unstructured response"},
				Confidence:     0.2,
			}, nil
		}
		return types.TriageReport{}, temporal.NewApplicationError(
			fmt.Sprintf("parse agent response: %v", err), "ParseError", err)
	}

	// Validate classification
	if !types.ValidClassifications[report.Classification] {
		report.Classification = "Unknown"
		report.Confidence = 0.3
	}

	return report, nil
}

// parseTriageReport flexibly parses LLM JSON into a TriageReport.
// Small models often return wrong types (objects instead of strings, etc.).
func parseTriageReport(text string) (types.TriageReport, error) {
	// First try strict unmarshal
	var report types.TriageReport
	if err := json.Unmarshal([]byte(text), &report); err == nil {
		return report, nil
	}

	// Flexible parse: unmarshal into a map and coerce types
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return types.TriageReport{}, fmt.Errorf("not valid JSON: %w", err)
	}

	// Re-marshal with causal_chain converted to []string
	if cc, ok := raw["causal_chain"]; ok {
		raw["causal_chain"] = coerceToStringSlice(cc)
	}

	// Rebuild JSON and unmarshal
	fixed, err := json.Marshal(raw)
	if err != nil {
		return types.TriageReport{}, err
	}
	if err := json.Unmarshal(fixed, &report); err != nil {
		return types.TriageReport{}, fmt.Errorf("parse after coercion: %w", err)
	}
	return report, nil
}

// coerceToStringSlice converts a JSON array of mixed types (strings or objects) to a JSON []string.
func coerceToStringSlice(raw json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		var s string
		if json.Unmarshal(item, &s) == nil {
			result = append(result, s)
			continue
		}
		// Object: stringify it
		result = append(result, string(item))
	}
	out, _ := json.Marshal(result)
	return out
}

// readJSONRPCResponse reads a standard JSON-RPC response.
// Supports both artifacts-based (kagent) and message-based (standard A2A) response formats.
// It returns the agent text and any parseable token-usage block (nil if absent).
func readJSONRPCResponse(body io.Reader) (string, *tokenUsage, error) {
	var a2aResp a2aResponse
	if err := json.NewDecoder(body).Decode(&a2aResp); err != nil {
		return "", nil, temporal.NewApplicationError(
			fmt.Sprintf("decode A2A response: %v", err), "ParseError", err)
	}
	if a2aResp.Error != nil {
		// JSON-RPC client errors (-32600 to -32602) are non-retryable
		if a2aResp.Error.Code >= -32602 && a2aResp.Error.Code <= -32600 {
			return "", nil, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("A2A error %d: %s", a2aResp.Error.Code, a2aResp.Error.Message),
				"A2AClientError", nil)
		}
		return "", nil, fmt.Errorf("A2A error %d: %s", a2aResp.Error.Code, a2aResp.Error.Message)
	}
	if a2aResp.Result == nil {
		return "", nil, temporal.NewApplicationError(
			"empty agent response", "ParseError", nil)
	}
	usage := extractTokenUsage(a2aResp.Result)
	// kagent returns result.artifacts[].parts[]
	if len(a2aResp.Result.Artifacts) > 0 && len(a2aResp.Result.Artifacts[0].Parts) > 0 {
		return a2aResp.Result.Artifacts[0].Parts[0].Text, usage, nil
	}
	// Fallback: standard A2A result.message.parts[]
	if a2aResp.Result.Message != nil && len(a2aResp.Result.Message.Parts) > 0 {
		return a2aResp.Result.Message.Parts[0].Text, usage, nil
	}
	// Fallback: status.message may contain agent text (some kagent versions)
	if a2aResp.Result.Status.Message != nil && len(a2aResp.Result.Status.Message.Parts) > 0 {
		return a2aResp.Result.Status.Message.Parts[0].Text, usage, nil
	}
	return "", usage, temporal.NewApplicationError(
		"empty agent response", "ParseError", nil)
}

// readSSEResponse consumes an SSE stream and returns the final text content and
// any parseable token-usage block (nil if absent).
func readSSEResponse(body io.Reader) (string, *tokenUsage, error) {
	scanner := bufio.NewScanner(body)
	// Agent responses with full enrichment context can exceed bufio.Scanner's
	// default 64KB line cap; raise it so a long SSE data event isn't truncated.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastData string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			lastData = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, fmt.Errorf("read SSE stream: %w", err)
	}

	if lastData == "" {
		return "", nil, temporal.NewNonRetryableApplicationError(
			"empty SSE stream", "ParseError", nil)
	}

	// The last SSE data event should be a JSON-RPC response (retryable parse)
	var a2aResp a2aResponse
	if err := json.Unmarshal([]byte(lastData), &a2aResp); err != nil {
		// Maybe it's just the text directly
		return lastData, nil, nil
	}
	if a2aResp.Result != nil {
		usage := extractTokenUsage(a2aResp.Result)
		if len(a2aResp.Result.Artifacts) > 0 && len(a2aResp.Result.Artifacts[0].Parts) > 0 {
			return a2aResp.Result.Artifacts[0].Parts[0].Text, usage, nil
		}
		if a2aResp.Result.Message != nil && len(a2aResp.Result.Message.Parts) > 0 {
			return a2aResp.Result.Message.Parts[0].Text, usage, nil
		}
		return lastData, usage, nil
	}
	return lastData, nil, nil
}

// stripMarkdownJSON extracts JSON from LLM responses that wrap it in markdown
// code fences (```json ... ``` or ``` ... ```). Small models often do this.
// Also handles cases where the LLM prefixes/suffixes JSON with prose text.
func stripMarkdownJSON(s string) string {
	s = strings.TrimSpace(s)
	// Already valid JSON start
	if strings.HasPrefix(s, "{") {
		return s
	}
	// Strip ```json ... ``` or ``` ... ```
	if idx := strings.Index(s, "```"); idx >= 0 {
		start := idx + 3
		// Skip optional language tag (e.g., "json")
		if nl := strings.IndexByte(s[start:], '\n'); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(s[start:], "```"); end >= 0 {
			extracted := strings.TrimSpace(s[start : start+end])
			if strings.HasPrefix(extracted, "{") {
				return extracted
			}
		}
	}
	// Last resort: find the first '{' and last '}' — extract embedded JSON
	if first := strings.IndexByte(s, '{'); first >= 0 {
		if last := strings.LastIndexByte(s, '}'); last > first {
			return s[first : last+1]
		}
	}
	return s
}

// buildAgentPrompt constructs the prompt sent to the triage agent.
// Uses code fences to isolate untrusted telemetry data.
func buildAgentPrompt(alerts []types.Alert, enrichment types.EnrichmentResult) string {
	var b strings.Builder

	b.WriteString("## Correlated Alerts\n\n")
	fmt.Fprintf(&b, "Total: %d firing alerts\n\n", len(alerts))
	for i, alert := range alerts {
		fmt.Fprintf(&b, "### Alert %d: %s\n", i+1, alert.Labels["alertname"])
		fmt.Fprintf(&b, "- Severity: %s\n", alert.Labels["severity"])
		fmt.Fprintf(&b, "- Namespace: %s\n", alert.Labels["namespace"])
		if pod := alert.Labels["pod"]; pod != "" {
			fmt.Fprintf(&b, "- Pod: %s\n", pod)
		}
		if desc := alert.Annotations["description"]; desc != "" {
			fmt.Fprintf(&b, "- Description: %s\n", desc)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Enrichment Context\n\n")

	// Prometheus metrics
	if enrichment.Prometheus.Available {
		b.WriteString("### Metrics (Prometheus)\n```\n")
		fmt.Fprintf(&b, "Restart rate (5m): %.1f\n", enrichment.Prometheus.RestartRate)
		fmt.Fprintf(&b, "Memory usage: %.1f%%\n", enrichment.Prometheus.MemoryPct)
		fmt.Fprintf(&b, "CPU usage: %.3f cores\n", enrichment.Prometheus.CPUUsage)
		b.WriteString("```\n\n")
	} else {
		b.WriteString("### Metrics: UNAVAILABLE\n\n")
	}

	// Kubernetes state
	if enrichment.Kubernetes.Available {
		b.WriteString("### Kubernetes State\n```\n")
		fmt.Fprintf(&b, "Pod phase: %s\n", enrichment.Kubernetes.PodPhase)
		if len(enrichment.Kubernetes.ExitCodes) > 0 {
			fmt.Fprintf(&b, "Exit codes: %v\n", enrichment.Kubernetes.ExitCodes)
		}
		if len(enrichment.Kubernetes.RecentEvents) > 0 {
			b.WriteString("Recent events:\n")
			for _, event := range enrichment.Kubernetes.RecentEvents {
				// Truncate long events
				if len(event) > 200 {
					event = event[:200] + "..."
				}
				fmt.Fprintf(&b, "  - %s\n", sanitize(event))
			}
		}
		b.WriteString("```\n\n")
	} else {
		b.WriteString("### Kubernetes State: UNAVAILABLE\n\n")
	}

	// Loki logs
	if enrichment.Loki.Available && len(enrichment.Loki.ErrorLines) > 0 {
		b.WriteString("### Recent Error Logs (Loki)\n```\n")
		for i, line := range enrichment.Loki.ErrorLines {
			if i >= 20 { // Cap at 20 lines
				fmt.Fprintf(&b, "... (%d more lines truncated)\n", len(enrichment.Loki.ErrorLines)-20)
				break
			}
			// Truncate individual lines and sanitize
			if len(line) > 300 {
				line = line[:300] + "..."
			}
			b.WriteString(sanitize(line) + "\n")
		}
		b.WriteString("```\n\n")
	} else if !enrichment.Loki.Available {
		b.WriteString("### Logs: UNAVAILABLE\n\n")
	}

	b.WriteString("## Task\n\nAnalyze the above alerts and context. Produce your structured JSON diagnosis.\n")

	return b.String()
}

// sanitize strips control characters from untrusted telemetry data.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 || r == '\n' || r == '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
