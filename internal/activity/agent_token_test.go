package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

// realA2AResponseWithUsage mirrors a live error-triage-agent A2A response
// (captured via direct port-forward). Usage appears at result.metadata in
// Google ADK / Gemini shape under "kagent_usage_metadata".
const realA2AResponseWithUsage = `{
  "id": "probe-2",
  "jsonrpc": "2.0",
  "result": {
    "artifacts": [{"artifactId": "art-1", "parts": [{"kind": "text", "text": "OK"}]}],
    "id": "task-1",
    "kind": "task",
    "metadata": {
      "kagent_author": "error_triage_agent",
      "kagent_usage_metadata": {
        "candidatesTokenCount": 199,
        "promptTokenCount": 946,
        "totalTokenCount": 1145
      }
    },
    "status": {"state": "completed"}
  }
}`

func TestReadJSONRPCResponse_ParsesADKTokenUsage(t *testing.T) {
	text, usage, err := readJSONRPCResponse(strings.NewReader(realA2AResponseWithUsage))
	if err != nil {
		t.Fatalf("readJSONRPCResponse() error = %v", err)
	}
	if text != "OK" {
		t.Errorf("text = %q, want OK", text)
	}
	if usage == nil {
		t.Fatal("expected token usage, got nil")
	}
	if usage.PromptTokens != 946 {
		t.Errorf("PromptTokens = %d, want 946", usage.PromptTokens)
	}
	if usage.CompletionTokens != 199 {
		t.Errorf("CompletionTokens = %d, want 199", usage.CompletionTokens)
	}
	if usage.TotalTokens != 1145 {
		t.Errorf("TotalTokens = %d, want 1145", usage.TotalTokens)
	}
}

func TestReadJSONRPCResponse_NoUsageReturnsNil(t *testing.T) {
	body := `{
		"jsonrpc": "2.0",
		"result": {
			"status": {"state": "completed"},
			"artifacts": [{"artifactId": "a1", "parts": [{"kind": "text", "text": "hello"}]}]
		}
	}`
	text, usage, err := readJSONRPCResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readJSONRPCResponse() error = %v", err)
	}
	if text != "hello" {
		t.Errorf("text = %q, want hello", text)
	}
	if usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
}

func TestUsageFromMetadata_ADKShape(t *testing.T) {
	md := map[string]json.RawMessage{
		"kagent_usage_metadata": json.RawMessage(`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}`),
	}
	u := usageFromMetadata(md)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Errorf("usage = %+v, want {10 5 15}", *u)
	}
}

func TestUsageFromMetadata_OpenAIShape(t *testing.T) {
	md := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`),
	}
	u := usageFromMetadata(md)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 20 || u.TotalTokens != 120 {
		t.Errorf("usage = %+v, want {100 20 120}", *u)
	}
}

func TestUsageFromMetadata_OllamaNativeFallback(t *testing.T) {
	// No OpenAI-style fields; only Ollama-native eval counts present.
	md := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"prompt_eval_count":80,"eval_count":12}`),
	}
	u := usageFromMetadata(md)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.PromptTokens != 80 || u.CompletionTokens != 12 {
		t.Errorf("usage = %+v, want prompt=80 completion=12", *u)
	}
	if u.TotalTokens != 92 {
		t.Errorf("TotalTokens = %d, want 92 (derived)", u.TotalTokens)
	}
}

func TestUsageFromMetadata_EmptyAndAbsent(t *testing.T) {
	if u := usageFromMetadata(nil); u != nil {
		t.Errorf("nil metadata: expected nil, got %+v", u)
	}
	if u := usageFromMetadata(map[string]json.RawMessage{}); u != nil {
		t.Errorf("empty metadata: expected nil, got %+v", u)
	}
	// Present but all-zero usage is treated as absent.
	md := map[string]json.RawMessage{
		"kagent_usage_metadata": json.RawMessage(`{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}`),
	}
	if u := usageFromMetadata(md); u != nil {
		t.Errorf("zero usage: expected nil, got %+v", u)
	}
}

func TestNewTokenUsage_DerivesTotal(t *testing.T) {
	u := newTokenUsage(30, 10, 0)
	if u == nil || u.TotalTokens != 40 {
		t.Fatalf("expected derived total 40, got %+v", u)
	}
	if u := newTokenUsage(0, 0, 0); u != nil {
		t.Errorf("all-zero: expected nil, got %+v", u)
	}
}
