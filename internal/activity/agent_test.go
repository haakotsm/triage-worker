package activity

import (
	"io"
	"strings"
	"testing"

	"github.com/haakotsm/triage-worker/internal/types"
)

func TestReadJSONRPCResponse_Artifacts(t *testing.T) {
	// kagent returns result.artifacts[].parts[] with "kind" field
	body := `{
		"jsonrpc": "2.0",
		"id": "triage-1",
		"result": {
			"status": {"state":"completed"},
			"artifacts": [{
				"artifactId": "a1",
				"parts": [{"kind": "text", "text": "{\"classification\":\"CrashLoop\"}"}]
			}]
		}
	}`

	text, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readJSONRPCResponse() error = %v", err)
	}
	if text != `{"classification":"CrashLoop"}` {
		t.Errorf("text = %q, want JSON with CrashLoop", text)
	}
}

func TestReadJSONRPCResponse_MessageFallback(t *testing.T) {
	// Standard A2A fallback: result.message.parts[]
	body := `{
		"jsonrpc": "2.0",
		"id": "triage-1",
		"result": {
			"status": {"state":"completed"},
			"message": {
				"role": "agent",
				"parts": [{"kind": "text", "text": "{\"classification\":\"CrashLoop\"}"}]
			}
		}
	}`

	text, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readJSONRPCResponse() error = %v", err)
	}
	if text != `{"classification":"CrashLoop"}` {
		t.Errorf("text = %q, want JSON with CrashLoop", text)
	}
}

func TestReadJSONRPCResponse_Error(t *testing.T) {
	body := `{
		"jsonrpc": "2.0",
		"id": "triage-1",
		"error": {"code": -32600, "message": "invalid request"}
	}`

	_, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "A2A error") {
		t.Errorf("error = %v, want A2A error", err)
	}
}

func TestReadJSONRPCResponse_EmptyResult(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":"1","result":{}}`

	_, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for empty result")
	}
}

func TestReadJSONRPCResponse_NullMessage(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":"1","result":{"status":{"state":"completed"}}}`

	_, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for null message")
	}
}

func TestReadJSONRPCResponse_EmptyParts(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":"1","result":{"status":{"state":"completed"},"message":{"role":"agent","parts":[]}}}`

	_, _, err := readJSONRPCResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for empty parts")
	}
}

func TestReadSSEResponse_Success(t *testing.T) {
	body := `data: {"jsonrpc":"2.0","id":"1","result":{"status":{"state":"working"}}}

data: {"jsonrpc":"2.0","id":"1","result":{"status":{"state":"completed"},"artifacts":[{"artifactId":"a1","parts":[{"kind":"text","text":"final result"}]}]}}

`

	text, _, err := readSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readSSEResponse() error = %v", err)
	}
	if text != "final result" {
		t.Errorf("text = %q, want %q", text, "final result")
	}
}

func TestReadSSEResponse_PlainText(t *testing.T) {
	body := "data: plain text response\n\n"

	text, _, err := readSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readSSEResponse() error = %v", err)
	}
	if text != "plain text response" {
		t.Errorf("text = %q, want %q", text, "plain text response")
	}
}

func TestReadSSEResponse_Empty(t *testing.T) {
	body := ""

	_, _, err := readSSEResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for empty SSE stream")
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"normal text", "hello world", "hello world"},
		{"with newline", "line1\nline2", "line1\nline2"},
		{"with tab", "col1\tcol2", "col1\tcol2"},
		{"control chars", "bad\x00\x01\x02text", "badtext"},
		{"null byte", "hello\x00world", "helloworld"},
		{"bell char", "alert\x07here", "alerthere"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.input)
			if got != tt.want {
				t.Errorf("sanitize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildAgentPrompt(t *testing.T) {
	alerts := []types.Alert{
		{
			Labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"severity":  "critical",
				"namespace": "default",
				"pod":       "my-app-abc123",
			},
			Annotations: map[string]string{
				"description": "Pod has restarted 5 times",
			},
		},
	}

	enrichment := types.EnrichmentResult{
		Prometheus: types.PrometheusResult{
			Available:   true,
			RestartRate: 5.0,
			MemoryPct:   85.3,
			CPUUsage:    0.45,
		},
		Kubernetes: types.KubernetesResult{
			Available: true,
			PodPhase:  "Running",
			ExitCodes: []int32{137},
			RecentEvents: []string{
				"BackOff restarting failed container",
			},
		},
		Loki: types.LokiResult{
			Available:  true,
			ErrorLines: []string{"Error: out of memory", "Fatal: cannot allocate"},
		},
	}

	prompt := buildAgentPrompt(alerts, enrichment)

	// Verify key sections are present
	checks := []string{
		"## Correlated Alerts",
		"KubePodCrashLooping",
		"critical",
		"my-app-abc123",
		"Pod has restarted 5 times",
		"## Enrichment Context",
		"Restart rate (5m): 5.0",
		"Memory usage: 85.3%",
		"CPU usage: 0.450 cores",
		"Running",
		"137",
		"BackOff restarting failed container",
		"Error: out of memory",
		"Fatal: cannot allocate",
		"## Task",
	}

	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Errorf("prompt missing %q", check)
		}
	}
}

func TestBuildAgentPrompt_UnavailableSources(t *testing.T) {
	alerts := []types.Alert{{
		Labels: map[string]string{"alertname": "TestAlert"},
	}}

	enrichment := types.EnrichmentResult{
		Prometheus: types.PrometheusResult{Available: false},
		Kubernetes: types.KubernetesResult{Available: false},
		Loki:       types.LokiResult{Available: false},
	}

	prompt := buildAgentPrompt(alerts, enrichment)

	if !strings.Contains(prompt, "Metrics: UNAVAILABLE") {
		t.Error("prompt should indicate metrics unavailable")
	}
	if !strings.Contains(prompt, "Kubernetes State: UNAVAILABLE") {
		t.Error("prompt should indicate k8s unavailable")
	}
	if !strings.Contains(prompt, "Logs: UNAVAILABLE") {
		t.Error("prompt should indicate logs unavailable")
	}
}

func TestBuildAgentPrompt_LongEventTruncation(t *testing.T) {
	alerts := []types.Alert{{
		Labels: map[string]string{"alertname": "TestAlert"},
	}}

	longEvent := strings.Repeat("x", 300)
	enrichment := types.EnrichmentResult{
		Kubernetes: types.KubernetesResult{
			Available:    true,
			RecentEvents: []string{longEvent},
		},
	}

	prompt := buildAgentPrompt(alerts, enrichment)

	// Event should be truncated to 200 chars + "..."
	if strings.Contains(prompt, longEvent) {
		t.Error("long event should be truncated")
	}
	if !strings.Contains(prompt, "...") {
		t.Error("truncated event should end with ...")
	}
}

func TestBuildAgentPrompt_LogLineTruncation(t *testing.T) {
	alerts := []types.Alert{{
		Labels: map[string]string{"alertname": "TestAlert"},
	}}

	// 25 log lines — should be capped at 20
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = "error line"
	}

	enrichment := types.EnrichmentResult{
		Loki: types.LokiResult{
			Available:  true,
			ErrorLines: lines,
		},
	}

	prompt := buildAgentPrompt(alerts, enrichment)

	if !strings.Contains(prompt, "5 more lines truncated") {
		t.Error("should indicate truncated log lines")
	}
}

// Ensure readJSONRPCResponse handles invalid JSON
func TestReadJSONRPCResponse_InvalidJSON(t *testing.T) {
	_, _, err := readJSONRPCResponse(strings.NewReader("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// Verify SSE with multiple data lines picks the last one
func TestReadSSEResponse_MultipleDataLines(t *testing.T) {
	body := "data: first\ndata: second\ndata: third\n\n"

	text, _, err := readSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readSSEResponse() error = %v", err)
	}
	if text != "third" {
		t.Errorf("text = %q, want %q", text, "third")
	}
}

// Verify readSSEResponse ignores non-data lines
func TestReadSSEResponse_NonDataLines(t *testing.T) {
	body := ": comment\nevent: update\ndata: the-data\n\n"

	text, _, err := readSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readSSEResponse() error = %v", err)
	}
	if text != "the-data" {
		t.Errorf("text = %q, want %q", text, "the-data")
	}
}

// Interface compliance — ensure readJSONRPCResponse accepts io.Reader
func TestReadJSONRPCResponse_ReaderInterface(t *testing.T) {
	var r io.Reader = strings.NewReader(`{"jsonrpc":"2.0","id":"1","result":{"status":{"state":"completed"},"artifacts":[{"artifactId":"x","parts":[{"kind":"text","text":"ok"}]}]}}`)
	text, _, err := readJSONRPCResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" {
		t.Errorf("text = %q, want %q", text, "ok")
	}
}

func TestStripMarkdownJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain JSON", `{"classification":"Known"}`, `{"classification":"Known"}`},
		{"json code fence", "```json\n{\"classification\":\"Known\"}\n```", `{"classification":"Known"}`},
		{"bare code fence", "```\n{\"classification\":\"Known\"}\n```", `{"classification":"Known"}`},
		{"prefix text", "Here is the analysis:\n```json\n{\"classification\":\"Known\"}\n```", `{"classification":"Known"}`},
		{"prose with embedded JSON", "The analysis is: {\"classification\":\"Known\"} that's it.", `{"classification":"Known"}`},
		{"no JSON", "Just some text", "Just some text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripMarkdownJSON(tt.input)
			if got != tt.want {
				t.Errorf("stripMarkdownJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTriageReport_FlexibleCausalChain(t *testing.T) {
	// LLM returns causal_chain as array of objects instead of strings
	input := `{
"classification": "Config",
"severity": "warning",
"root_cause": "missing config",
"causal_chain": [{"step": "pod started"}, {"step": "config not found"}],
"evidence": [],
"impact": {"affected_services": [], "blast_radius": "pod"},
"recommendations": [],
"confidence": 0.7,
"escalation_needed": false
}`
	report, err := parseTriageReport(input)
	if err != nil {
		t.Fatalf("parseTriageReport failed: %v", err)
	}
	if report.Classification != "Config" {
		t.Errorf("classification = %q, want Config", report.Classification)
	}
	if len(report.CausalChain) != 2 {
		t.Errorf("causal_chain length = %d, want 2", len(report.CausalChain))
	}
}

func TestParseTriageReport_StrictWorks(t *testing.T) {
	input := `{
"classification": "OOM",
"severity": "critical",
"root_cause": "memory leak",
"causal_chain": ["pod started", "memory grew", "OOMKilled"],
"evidence": [{"observation": "OOM event", "source": "events", "strength": "strong"}],
"impact": {"affected_services": ["svc-a"], "blast_radius": "deployment"},
"recommendations": [{"action": "increase limits", "risk": "low"}],
"confidence": 0.9,
"escalation_needed": true
}`
	report, err := parseTriageReport(input)
	if err != nil {
		t.Fatalf("parseTriageReport failed: %v", err)
	}
	if report.Classification != "OOM" {
		t.Errorf("classification = %q, want OOM", report.Classification)
	}
	if len(report.CausalChain) != 3 {
		t.Errorf("causal_chain length = %d, want 3", len(report.CausalChain))
	}
}
