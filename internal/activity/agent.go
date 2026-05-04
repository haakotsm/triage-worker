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

	"go.temporal.io/sdk/temporal"

	"github.com/haakotsm/triage-worker/internal/auth"
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
	Role  string   `json:"role"`
	Parts []a2aPart `json:"parts"`
}

type a2aPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// a2aResponse is the JSON-RPC response (simplified for non-streaming).
type a2aResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      string       `json:"id"`
	Result  *a2aResult   `json:"result,omitempty"`
	Error   *a2aError    `json:"error,omitempty"`
}

type a2aResult struct {
	Status  string     `json:"status"`
	Message *a2aMessage `json:"message,omitempty"`
}

type a2aError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// InvokeTriageAgent calls the error-triage-agent with correlated alerts and enrichment context.
func (a *AgentActivity) InvokeTriageAgent(ctx context.Context, alerts []types.Alert, enrichment types.EnrichmentResult) (types.TriageReport, error) {
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
					{Type: "text", Text: prompt},
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

	if strings.Contains(contentType, "text/event-stream") {
		agentText, err = readSSEResponse(resp.Body)
	} else {
		agentText, err = readJSONRPCResponse(resp.Body)
	}
	if err != nil {
		return types.TriageReport{}, err
	}

	// Parse agent response into TriageReport (retryable — LLM output is non-deterministic)
	var report types.TriageReport
	if err := json.Unmarshal([]byte(agentText), &report); err != nil {
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

// readJSONRPCResponse reads a standard JSON-RPC response.
func readJSONRPCResponse(body io.Reader) (string, error) {
	var a2aResp a2aResponse
	if err := json.NewDecoder(body).Decode(&a2aResp); err != nil {
		return "", temporal.NewApplicationError(
			fmt.Sprintf("decode A2A response: %v", err), "ParseError", err)
	}
	if a2aResp.Error != nil {
		return "", fmt.Errorf("A2A error %d: %s", a2aResp.Error.Code, a2aResp.Error.Message)
	}
	if a2aResp.Result == nil || a2aResp.Result.Message == nil || len(a2aResp.Result.Message.Parts) == 0 {
		return "", temporal.NewNonRetryableApplicationError(
			"empty agent response", "ParseError", nil)
	}
	return a2aResp.Result.Message.Parts[0].Text, nil
}

// readSSEResponse consumes an SSE stream and returns the final text content.
func readSSEResponse(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	var lastData string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			lastData = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SSE stream: %w", err)
	}

	if lastData == "" {
		return "", temporal.NewNonRetryableApplicationError(
			"empty SSE stream", "ParseError", nil)
	}

	// The last SSE data event should be a JSON-RPC response (retryable parse)
	var a2aResp a2aResponse
	if err := json.Unmarshal([]byte(lastData), &a2aResp); err != nil {
		// Maybe it's just the text directly
		return lastData, nil
	}
	if a2aResp.Result != nil && a2aResp.Result.Message != nil && len(a2aResp.Result.Message.Parts) > 0 {
		return a2aResp.Result.Message.Parts[0].Text, nil
	}
	return lastData, nil
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
