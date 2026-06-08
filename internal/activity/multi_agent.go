package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/haakotsm/triage-worker/internal/auth"
	"github.com/haakotsm/triage-worker/internal/types"
)

// MultiAgentActivity handles invocation of multiple kagent agents via A2A protocol.
// Unlike the original AgentActivity (single agent, single URL), this struct can
// target any agent by name through the agent gateway.
//
// Design decisions:
//   - GatewayBaseURL is the prefix (e.g., "http://agentgateway.agentgateway.svc:3001/api/a2a/kagent")
//     and the agent name is appended per invocation. This avoids creating N activity structs.
//   - HTTPClient has no Client.Timeout — the Temporal activity StartToCloseTimeout governs lifespan.
//     This prevents double-timeout conflicts where both HTTP and Temporal race to kill the call.
//   - TokenProvider is shared across all agent calls within a workflow execution.
type MultiAgentActivity struct {
	GatewayBaseURL string // e.g., "http://agentgateway.agentgateway.svc:3001/api/a2a/kagent"
	TokenProvider  *auth.TokenProvider
	HTTPClient     *http.Client
}

// InvokeInvestigator calls a single investigator agent and returns structured findings.
// This is registered as a Temporal activity — each parallel invocation runs as its own activity execution.
//
// The activity is designed for:
//   - Heartbeating: records progress at each phase (auth, invoke, parse) so Temporal can detect stuck agents.
//   - Graceful degradation: returns a partial InvestigatorOutput with Available=false on failure
//     rather than returning an error, allowing the workflow to proceed with partial results.
//   - Timeout safety: agent tool-calling loops are bounded by StartToCloseTimeout (3-5 min).
//     HeartbeatTimeout (60s) catches agents that hang mid-tool-call.
func (m *MultiAgentActivity) InvokeInvestigator(
	ctx context.Context,
	agentName string,
	alerts []types.Alert,
	identity types.IncidentIdentity,
) (types.InvestigatorOutput, error) {
	startTime := time.Now()
	logger := activity.GetLogger(ctx)

	output := types.InvestigatorOutput{
		AgentName: agentName,
		Available: false,
	}

	// --- Build prompt ---
	prompt := buildInvestigatorPrompt(agentName, alerts, identity)

	activity.RecordHeartbeat(ctx, "authenticating")

	// --- Get token ---
	token, err := m.TokenProvider.Token(ctx)
	if err != nil {
		output.Error = fmt.Sprintf("auth failed: %v", err)
		output.DurationMs = time.Since(startTime).Milliseconds()
		logger.Warn("investigator auth failed", "agent", agentName, "error", err)
		// Auth failure is non-retryable — return the output with error rather than failing the activity.
		// This lets the consolidator know this agent was unavailable.
		return output, nil
	}

	activity.RecordHeartbeat(ctx, "invoking_agent:"+agentName)

	// --- Build A2A request ---
	agentURL := m.GatewayBaseURL + "/" + agentName
	a2aReq := a2aRequest{
		JSONRPC: "2.0",
		ID:      "investigate-" + agentName,
		Method:  "message/send",
		Params: a2aMessageSend{
			Message: a2aMessage{
				Role:  "user",
				Parts: []a2aPart{{Kind: "text", Text: prompt}},
			},
		},
	}

	body, err := json.Marshal(a2aReq)
	if err != nil {
		output.Error = fmt.Sprintf("marshal request: %v", err)
		output.DurationMs = time.Since(startTime).Milliseconds()
		return output, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL, bytes.NewReader(body))
	if err != nil {
		output.Error = fmt.Sprintf("build request: %v", err)
		output.DurationMs = time.Since(startTime).Milliseconds()
		return output, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// --- Execute request with periodic heartbeats ---
	// LLM calls can take 2-5 minutes. Send heartbeats every 30s to keep Temporal alive.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, "waiting_for_agent:"+agentName)
			}
		}
	}()

	resp, err := m.HTTPClient.Do(req)
	hbCancel()
	if err != nil {
		output.Error = fmt.Sprintf("agent request failed: %v", err)
		output.DurationMs = time.Since(startTime).Milliseconds()
		logger.Warn("investigator request failed", "agent", agentName, "error", err)
		return output, nil
	}
	defer resp.Body.Close()

	// --- Handle HTTP errors ---
	// Non-retryable errors (auth, client errors) are reported in output, not as activity failures.
	// Retryable errors (429, 5xx) ARE returned as activity errors to trigger Temporal retry.
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return output, fmt.Errorf("agent %s: rate limited (429)", agentName)
	case resp.StatusCode >= 500:
		return output, fmt.Errorf("agent %s: server error %d", agentName, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		output.Error = fmt.Sprintf("agent returned HTTP %d", resp.StatusCode)
		output.DurationMs = time.Since(startTime).Milliseconds()
		return output, nil
	}

	activity.RecordHeartbeat(ctx, "parsing_response:"+agentName)

	// --- Parse response (reuse existing SSE/JSON-RPC parsing) ---
	contentType := resp.Header.Get("Content-Type")
	var agentText string

	if strings.Contains(contentType, "text/event-stream") {
		agentText, err = readSSEResponse(resp.Body)
	} else {
		agentText, err = readJSONRPCResponse(resp.Body)
	}
	if err != nil {
		output.Error = fmt.Sprintf("parse response: %v", err)
		output.DurationMs = time.Since(startTime).Milliseconds()
		return output, nil
	}

	output.RawResponse = agentText

	// --- Parse structured findings ---
	agentText = stripMarkdownJSON(agentText)
	agentText = repairJSON(agentText)
	findings, parseErr := parseInvestigatorOutput(agentText)
	if parseErr != nil {
		// Fallback: agent returned prose instead of structured JSON.
		// Create a single finding from the raw text so information is not lost.
		logger.Warn("investigator returned unstructured output",
			"agent", agentName,
			"parse_error", parseErr,
			"response_preview", truncate(agentText, 200),
		)
		if len(agentText) > 20 {
			output.Available = true
			output.Findings = []types.Finding{{
				Category:    "error_pattern",
				Description: truncate(agentText, 1000),
				Evidence:    "raw agent response (failed structured parse)",
				Severity:    "info",
				Confidence:  0.3,
			}}
		} else {
			output.Error = fmt.Sprintf("empty or unparseable response: %v", parseErr)
		}
	} else {
		output.Available = true
		output.Findings = findings
	}

	output.DurationMs = time.Since(startTime).Milliseconds()
	logger.Info("investigator completed",
		"agent", agentName,
		"findings", len(output.Findings),
		"duration_ms", output.DurationMs,
	)

	return output, nil
}

// InvokeConsolidator calls the consolidator agent to synthesize findings from all investigators.
// Unlike investigators, the consolidator has NO MCP tools — it only reasons over provided data.
//
// This activity DOES return an error on failure (unlike InvokeInvestigator) because there is
// no fallback: if consolidation fails, the workflow must retry or fail.
func (m *MultiAgentActivity) InvokeConsolidator(
	ctx context.Context,
	alerts []types.Alert,
	investigations []types.InvestigatorOutput,
) (types.TriageReport, error) {
	logger := activity.GetLogger(ctx)

	// --- Build consolidation prompt ---
	prompt := buildConsolidatorPrompt(alerts, investigations)

	activity.RecordHeartbeat(ctx, "authenticating")

	token, err := m.TokenProvider.Token(ctx)
	if err != nil {
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("auth failed: %v", err), "AuthError", err)
	}

	activity.RecordHeartbeat(ctx, "invoking_consolidator")

	// --- Build A2A request ---
	agentURL := m.GatewayBaseURL + "/triage-consolidator"
	a2aReq := a2aRequest{
		JSONRPC: "2.0",
		ID:      "consolidate-1",
		Method:  "message/send",
		Params: a2aMessageSend{
			Message: a2aMessage{
				Role:  "user",
				Parts: []a2aPart{{Kind: "text", Text: prompt}},
			},
		},
	}

	body, err := json.Marshal(a2aReq)
	if err != nil {
		return types.TriageReport{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL, bytes.NewReader(body))
	if err != nil {
		return types.TriageReport{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// Periodic heartbeats during the LLM call (consolidation can take 2-5 minutes)
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, "waiting_for_consolidator")
			}
		}
	}()

	resp, err := m.HTTPClient.Do(req)
	hbCancel()
	if err != nil {
		return types.TriageReport{}, fmt.Errorf("consolidator request: %w", err)
	}
	defer resp.Body.Close()

	// --- HTTP error handling (same pattern as original agent.go) ---
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("auth rejected: %d", resp.StatusCode), "AuthError", nil)
	case resp.StatusCode == http.StatusBadRequest:
		return types.TriageReport{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("client error: %d", resp.StatusCode), "ClientError", nil)
	case resp.StatusCode == http.StatusTooManyRequests:
		return types.TriageReport{}, fmt.Errorf("consolidator rate limited (429)")
	case resp.StatusCode >= 500:
		return types.TriageReport{}, fmt.Errorf("consolidator server error: %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return types.TriageReport{}, fmt.Errorf("consolidator unexpected status: %d", resp.StatusCode)
	}

	activity.RecordHeartbeat(ctx, "parsing_consolidator_response")

	// --- Parse response ---
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

	agentText = stripMarkdownJSON(agentText)
	agentText = repairJSON(agentText)

	// --- Parse TriageReport (reuse existing flexible parser) ---
	report, err := parseTriageReport(agentText)
	if err != nil {
		// Fallback: return minimal report with raw text (same pattern as original)
		if len(agentText) > 20 {
			logger.Warn("consolidator returned unstructured output, creating minimal report")
			return types.TriageReport{
				Classification: "Unknown",
				Severity:       "warning",
				RootCause:      truncate(agentText, 500),
				CausalChain:    []string{"Consolidator returned unstructured response"},
				Confidence:     0.2,
			}, nil
		}
		return types.TriageReport{}, temporal.NewApplicationError(
			fmt.Sprintf("parse consolidator response: %v", err), "ParseError", err)
	}

	// Validate classification (same as original)
	if !types.ValidClassifications[report.Classification] {
		report.Classification = "Unknown"
		report.Confidence = 0.3
	}

	logger.Info("consolidation complete",
		"classification", report.Classification,
		"severity", report.Severity,
		"confidence", report.Confidence,
	)

	return report, nil
}

// --- Prompt Builders ---

// buildInvestigatorPrompt constructs a targeted prompt for a specific investigator agent.
// Each investigator gets:
//   - The alert context (what fired, which namespace/workload)
//   - Tool usage guidance (which MCP tools to prioritize)
//   - Output schema (exact JSON structure expected)
//   - Tool budget (max calls to prevent runaway loops)
func buildInvestigatorPrompt(agentName string, alerts []types.Alert, identity types.IncidentIdentity) string {
	var b strings.Builder

	// --- Context header ---
	b.WriteString("# Investigation Task\n\n")
	fmt.Fprintf(&b, "You are investigating an incident for workload **%s/%s** (kind: %s) in namespace **%s**.\n\n",
		identity.Namespace, identity.Name, identity.Kind, identity.Namespace)

	// --- Alert data ---
	b.WriteString("## Firing Alerts\n\n```alert-data\n")
	for i, alert := range alerts {
		fmt.Fprintf(&b, "Alert %d: %s\n", i+1, sanitize(alert.Labels["alertname"]))
		fmt.Fprintf(&b, "  severity: %s\n", sanitize(alert.Labels["severity"]))
		fmt.Fprintf(&b, "  namespace: %s\n", sanitize(alert.Labels["namespace"]))
		if pod := alert.Labels["pod"]; pod != "" {
			fmt.Fprintf(&b, "  pod: %s\n", sanitize(pod))
		}
		if container := alert.Labels["container"]; container != "" {
			fmt.Fprintf(&b, "  container: %s\n", sanitize(container))
		}
		if desc := alert.Annotations["description"]; desc != "" {
			if len(desc) > 500 {
				desc = desc[:500] + "...(truncated)"
			}
			fmt.Fprintf(&b, "  description: %s\n", sanitize(desc))
		}
		b.WriteString("\n")
	}
	b.WriteString("```\n\n")

	// --- Agent-specific tool guidance ---
	b.WriteString("## Investigation Focus\n\n")
	switch {
	case strings.Contains(agentName, "k8s"):
		b.WriteString(`Focus your investigation on **Kubernetes resource state**:
1. Describe the affected pods — check container statuses, restart counts, exit codes
2. List recent Events for the workload (Warning events are most relevant)
3. Check the pod's resource requests/limits vs node capacity
4. Look at the deployment/statefulset rollout status
5. Check configmaps and secrets referenced by the pod

Use your kubectl/k8s tools. Budget: **max 8 tool calls**.
`)
	case strings.Contains(agentName, "logs"):
		b.WriteString(`Focus your investigation on **application logs**:
1. Get recent logs from the affected pod (use tail_lines=100)
2. Look for stack traces, panic messages, or OOM kill indicators
3. Identify error patterns — are errors clustered or continuous?
4. Check for connection refused / timeout messages to dependencies
5. Look for configuration errors (missing env vars, bad file paths)

Use your k8s pod log tools. Budget: **max 6 tool calls**.
`)
	case strings.Contains(agentName, "metrics"):
		b.WriteString(`Focus your investigation on **metrics and resource usage**:
1. Check container memory usage vs limits (OOM risk)
2. Check CPU usage and throttling
3. Query restart rate over the last 30 minutes
4. Look for request latency or error rate spikes
5. Check if resource usage changed suddenly (indicates regression)

Use your Prometheus query tools. Budget: **max 6 tool calls**.
`)
	case strings.Contains(agentName, "argocd"):
		b.WriteString(`Focus your investigation on **ArgoCD deployment state**:
1. List ArgoCD Application CRDs in the workload's namespace
2. Check sync status and health status of the relevant Application
3. Look at the Helm release values (especially resource limits, image tags)
4. Check recent sync history for failed or degraded syncs
5. Identify if the issue correlates with a recent deployment

Use your k8s and helm tools. Budget: **max 8 tool calls**.
`)
	default:
		b.WriteString("Investigate the incident using your available tools. Budget: **max 5 tool calls**.\n")
	}

	// --- Output schema ---
	b.WriteString("\n## Required Output Format\n\n")
	b.WriteString("**CRITICAL: Respond with ONLY valid JSON. No prose, no explanation, no markdown outside the JSON block.**\n\n")
	b.WriteString("```json\n")
	b.WriteString(`{
  "findings": [
    {
      "category": "resource_state|error_pattern|metric_anomaly|config_drift|network_issue|scheduling|storage|dependency|deployment_state",
      "description": "Clear explanation of what you found",
      "evidence": "Raw data supporting this finding (paste relevant output)",
      "severity": "critical|warning|info",
      "confidence": 0.9
    }
  ]
}`)
	b.WriteString("\n```\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Output ONLY the JSON object above — no text before or after\n")
	b.WriteString("- Use double quotes for all strings (NOT single quotes)\n")
	b.WriteString("- Include 1-5 findings, ordered by severity\n")
	b.WriteString("- Only report findings with actual evidence from tool results\n")
	b.WriteString("- Do NOT speculate without evidence\n")
	b.WriteString("- If tools return no useful data, respond with: {\"findings\": []}\n")
	b.WriteString("- Keep evidence SHORT (max 500 chars) to avoid truncation\n")

	return b.String()
}

// buildConsolidatorPrompt constructs the synthesis prompt for the consolidator agent.
// The consolidator has NO tools — it operates purely on the investigator outputs.
func buildConsolidatorPrompt(alerts []types.Alert, investigations []types.InvestigatorOutput) string {
	var b strings.Builder

	b.WriteString("# Incident Consolidation Task\n\n")
	b.WriteString("You are a senior SRE synthesizing findings from multiple investigation agents. ")
	b.WriteString("Cross-reference the evidence, identify the root cause, and produce a unified diagnosis.\n\n")

	// --- Original alert context ---
	b.WriteString("## Original Alerts\n\n```\n")
	for i, alert := range alerts {
		fmt.Fprintf(&b, "Alert %d: %s (severity: %s, namespace: %s)\n",
			i+1,
			sanitize(alert.Labels["alertname"]),
			sanitize(alert.Labels["severity"]),
			sanitize(alert.Labels["namespace"]),
		)
		if desc := alert.Annotations["description"]; desc != "" {
			if len(desc) > 300 {
				desc = desc[:300] + "..."
			}
			fmt.Fprintf(&b, "  → %s\n", sanitize(desc))
		}
	}
	b.WriteString("```\n\n")

	// --- Investigator results ---
	b.WriteString("## Investigation Results\n\n")

	availableCount := 0
	for _, inv := range investigations {
		fmt.Fprintf(&b, "### %s", inv.AgentName)
		if !inv.Available {
			fmt.Fprintf(&b, " ⚠️ UNAVAILABLE (error: %s)\n\n", inv.Error)
			continue
		}
		availableCount++
		fmt.Fprintf(&b, " (%d findings, %dms)\n\n", len(inv.Findings), inv.DurationMs)

		if len(inv.Findings) == 0 {
			b.WriteString("_No findings reported._\n\n")
			continue
		}

		b.WriteString("```findings\n")
		for i, f := range inv.Findings {
			fmt.Fprintf(&b, "Finding %d:\n", i+1)
			fmt.Fprintf(&b, "  category: %s\n", f.Category)
			fmt.Fprintf(&b, "  severity: %s (confidence: %.1f)\n", f.Severity, f.Confidence)
			fmt.Fprintf(&b, "  description: %s\n", f.Description)
			if f.Evidence != "" {
				evidence := f.Evidence
				if len(evidence) > 1000 {
					evidence = evidence[:1000] + "...(truncated)"
				}
				fmt.Fprintf(&b, "  evidence: %s\n", evidence)
			}
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	// --- Consolidation instructions ---
	b.WriteString("## Consolidation Instructions\n\n")
	if availableCount == 0 {
		b.WriteString("⚠️ ALL investigators failed. Produce a minimal diagnosis based only on alert context.\n")
		b.WriteString("Set confidence to 0.1 and classification to 'Unknown'.\n\n")
	} else {
		b.WriteString("1. **Cross-reference**: Do findings from different agents corroborate each other?\n")
		b.WriteString("2. **Contradictions**: Flag any conflicting evidence between agents\n")
		b.WriteString("3. **Root cause**: Identify the single underlying cause (not symptoms)\n")
		b.WriteString("4. **Causal chain**: Order events from trigger → symptoms → impact\n")
		b.WriteString("5. **Confidence**: Weight by number of corroborating sources and evidence strength\n\n")
	}

	// --- Output schema ---
	b.WriteString("## Required Output Format\n\n")
	b.WriteString("**CRITICAL: Respond with ONLY valid JSON. No prose before or after. Use double quotes only.**\n\n")
	b.WriteString("```json\n")
	b.WriteString(`{
  "classification": "CrashLoop|OOM|Network|ImagePull|ResourceExhaustion|Config|Scheduling|Unknown",
  "severity": "critical|warning|info",
  "root_cause": "Single sentence identifying the root cause",
  "causal_chain": ["Event A triggered...", "Which caused...", "Resulting in..."],
  "evidence": [
    {"observation": "What was observed", "source": "Which agent/tool found it", "strength": "strong|moderate|weak"}
  ],
  "impact": {
    "affected_services": ["service-a", "service-b"],
    "blast_radius": "pod|deployment|namespace|cluster"
  },
  "recommendations": [
    {"action": "What to do", "command": "kubectl command if applicable", "risk": "none|low|medium"}
  ],
  "confidence": 0.85,
  "escalation_needed": true
}`)
	b.WriteString("\n```\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Output ONLY the JSON object — no text before or after\n")
	b.WriteString("- Use double quotes for all strings (NOT single quotes)\n")
	b.WriteString("- Keep evidence observations concise but informative (max 300 chars each)\n")

	return b.String()
}

// --- Parsing helpers ---

// parseInvestigatorOutput parses the JSON output from an investigator agent.
// Handles both the full InvestigatorOutput envelope and bare findings array.
func parseInvestigatorOutput(text string) ([]types.Finding, error) {
	// Try parsing as full output envelope first
	var envelope struct {
		Findings []types.Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err == nil && len(envelope.Findings) > 0 {
		return normalizeFindingSeverities(envelope.Findings), nil
	}

	// Try bare array of findings
	var findings []types.Finding
	if err := json.Unmarshal([]byte(text), &findings); err == nil && len(findings) > 0 {
		return normalizeFindingSeverities(findings), nil
	}

	// Try flexible map parsing (LLM sometimes uses unexpected field names)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}

	// Look for any key that might contain findings array
	knownKeys := []string{"findings", "results", "observations", "issues", "data", "analysis", "items", "metrics", "alerts"}
	for _, key := range knownKeys {
		if data, ok := raw[key]; ok {
			var f []types.Finding
			if json.Unmarshal(data, &f) == nil && len(f) > 0 {
				return normalizeFindingSeverities(f), nil
			}
		}
	}

	// Last resort: try every key in the object for an array of objects that have "description"
	for key, data := range raw {
		_ = key
		var arr []map[string]interface{}
		if json.Unmarshal(data, &arr) == nil && len(arr) > 0 {
			// Check if items look like findings (have description or category)
			if _, hasDesc := arr[0]["description"]; hasDesc {
				var f []types.Finding
				if json.Unmarshal(data, &f) == nil && len(f) > 0 {
					return normalizeFindingSeverities(f), nil
				}
			}
		}
	}

	// If the object itself looks like a single finding, wrap it
	var single types.Finding
	if json.Unmarshal([]byte(text), &single) == nil && single.Description != "" {
		return normalizeFindingSeverities([]types.Finding{single}), nil
	}

	return nil, fmt.Errorf("no findings array found in response")
}

// normalizeFindingSeverities ensures all findings have valid severity values.
func normalizeFindingSeverities(findings []types.Finding) []types.Finding {
	for i := range findings {
		if !types.ValidSeverities[findings[i].Severity] {
			findings[i].Severity = "info"
		}
		if findings[i].Confidence < 0 {
			findings[i].Confidence = 0
		}
		if findings[i].Confidence > 1 {
			findings[i].Confidence = 1
		}
	}
	return findings
}

// readResponseBody is a helper that reads and limits the response body size.
// Prevents unbounded memory allocation from malformed agent responses.
func readResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(body, maxBytes)
	return io.ReadAll(limited)
}

// repairJSON attempts to fix common LLM JSON output issues:
// 1. Single quotes instead of double quotes (Python dict style)
// 2. Truncated JSON (unclosed brackets/braces)
// 3. Trailing commas before closing brackets
// 4. Prose wrapping around JSON content
func repairJSON(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}

	// If it already parses as valid JSON, return as-is
	if json.Valid([]byte(text)) {
		return text
	}

	// Fix 0: Try to extract JSON object/array from prose-wrapped text
	// (model sometimes adds "Here is my analysis:" before the JSON)
	if extracted := extractJSONFromProse(text); extracted != "" && json.Valid([]byte(extracted)) {
		return extracted
	}

	// Fix 1: Replace single quotes with double quotes.
	// Try this whenever the text contains single quotes and isn't valid JSON.
	if strings.Contains(text, "'") {
		fixed := fixSingleQuotes(text)
		if json.Valid([]byte(fixed)) {
			return fixed
		}
		// Try extracting JSON from the single-quote-fixed text (prose + single-quoted JSON)
		if extracted := extractJSONFromProse(fixed); extracted != "" && json.Valid([]byte(extracted)) {
			return extracted
		}
		// Even if not fully valid after fix, use the fixed version for further repairs
		text = fixed
	}

	// Fix 2: Trailing commas (e.g., [{"a": 1},] or {"a": 1,})
	text = strings.ReplaceAll(text, ",]", "]")
	text = strings.ReplaceAll(text, ",}", "}")
	if json.Valid([]byte(text)) {
		return text
	}

	// Fix 3: Truncated JSON — try to close unclosed brackets/braces
	text = repairTruncatedJSON(text)

	return text
}

// fixSingleQuotes converts Python-style single-quoted JSON to proper double-quoted JSON.
// Handles the common patterns: {'key': 'value'} → {"key": "value"}
func fixSingleQuotes(text string) string {
	var result strings.Builder
	result.Grow(len(text))

	inDoubleQuote := false
	inSingleQuote := false

	for i := 0; i < len(text); i++ {
		ch := text[i]

		if ch == '\\' && i+1 < len(text) {
			// Escaped character — pass through
			result.WriteByte(ch)
			i++
			result.WriteByte(text[i])
			continue
		}

		if ch == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			result.WriteByte(ch)
			continue
		}

		if ch == '\'' && !inDoubleQuote {
			// Replace single quote with double quote
			inSingleQuote = !inSingleQuote
			result.WriteByte('"')
			continue
		}

		// Inside a single-quoted string, escape any literal double quotes
		if inSingleQuote && ch == '"' {
			result.WriteString(`\"`)
			continue
		}

		result.WriteByte(ch)
	}

	return result.String()
}

// repairTruncatedJSON attempts to close unclosed brackets/braces in truncated JSON.
func repairTruncatedJSON(text string) string {
	// Count open/close brackets
	var stack []byte
	inString := false

	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\\' && inString && i+1 < len(text) {
			i++ // skip escaped char
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == ch {
				stack = stack[:len(stack)-1]
			}
		}
	}

	if len(stack) == 0 {
		return text
	}

	// If we're in a truncated string, close it first
	if inString {
		text += `"`
	}

	// Remove trailing comma if present
	trimmed := strings.TrimRight(text, " \t\n\r")
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == ',' {
		text = trimmed[:len(trimmed)-1]
	}

	// Close brackets in reverse order
	for i := len(stack) - 1; i >= 0; i-- {
		text += string(stack[i])
	}

	return text
}

// extractJSONFromProse finds the first complete JSON object or array in text
// that may be wrapped with prose (e.g., "Here is my analysis:\n{...}\n").
func extractJSONFromProse(text string) string {
	// Find the first { or [ that could start JSON
	for i, ch := range text {
		if ch == '{' || ch == '[' {
			candidate := text[i:]
			// Try parsing from this position
			if json.Valid([]byte(candidate)) {
				return candidate
			}
			// Try up to the matching closer (find last } or ])
			closer := byte('}')
			if ch == '[' {
				closer = ']'
			}
			lastIdx := strings.LastIndexByte(candidate, closer)
			if lastIdx > 0 {
				sub := candidate[:lastIdx+1]
				if json.Valid([]byte(sub)) {
					return sub
				}
			}
			// Only try the first occurrence
			break
		}
	}
	return ""
}
