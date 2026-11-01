package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteStandaloneDataLine(t *testing.T) {
	names := map[string]struct{}{"apply_patch": {}}
	in := `data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"apply_patch"}}`
	out, ok := rewriteSSEChunk([]byte(in), names)
	if !ok {
		t.Fatal("expected rewrite on standalone data line")
	}
	s := string(out)
	for _, want := range []string{"data:", "custom_tool_call", "ctc_call_1", "apply_patch"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if strings.HasPrefix(strings.TrimLeft(s, " \t"), "event:") {
		t.Fatalf("standalone data must not invent event line: %s", s)
	}
}

func TestIsSSEChunkDataLine(t *testing.T) {
	if !isSSEChunk([]byte(`data: {"type":"x"}`)) {
		t.Fatal("data line should be SSE")
	}
	if !isSSEChunk([]byte(`event: response.completed`)) {
		t.Fatal("event line should be SSE")
	}
}

func TestSSEEventType(t *testing.T) {
	if got := sseEventType([]byte(`data: {"type":"response.output_text.delta","delta":"a"}`)); got != "response.output_text.delta" {
		t.Fatalf("data type: got %q", got)
	}
	if got := sseEventType([]byte("event: response.function_call_arguments.delta\ndata: {\"delta\":\"x\"}\n")); got != "response.function_call_arguments.delta" {
		t.Fatalf("event line: got %q", got)
	}
}

func streamChunkEnvelope(t *testing.T, requestID, sourceFormat string, chunkIndex int, body []byte, model string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"RequestID":      requestID,
		"SourceFormat":   sourceFormat,
		"Model":          model,
		"RequestedModel": model,
		"ChunkIndex":     chunkIndex,
		"Body":           body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func chunkBodyFromEnvelope(t *testing.T, out []byte) []byte {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	if len(env.Result) == 0 {
		return nil
	}
	var pr struct {
		Body []byte `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &pr); err != nil {
		t.Fatal(err)
	}
	return pr.Body
}

func TestInterceptStreamChunkSkipsTextDelta(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	streamStates.put("req-1", "claude", map[string]struct{}{"apply_patch": {}})
	raw := streamChunkEnvelope(t, "req-1", "openai-response", 3,
		[]byte(`data: {"type":"response.output_text.delta","delta":"字"}`+"\n"), "claude-opus-5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if body := chunkBodyFromEnvelope(t, out); len(body) != 0 {
		t.Fatalf("text delta must pass through with empty rewrite, got %q", body)
	}
}

func TestInterceptStreamChunkSkipsUnknownRequest(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// No streamState recorded for this request: payload chunks must pass
	// through untouched (no OriginalRequest available, none needed).
	chunk := []byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"apply_patch"}}`)
	raw := streamChunkEnvelope(t, "req-unknown", "openai-response", 4, chunk, "grok-4.5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if body := chunkBodyFromEnvelope(t, out); len(body) != 0 {
		t.Fatalf("unknown request must pass through, got %q", body)
	}
}

func TestInterceptStreamChunkRewritesFromRequestState(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	streamStates.put("req-2", "claude", map[string]struct{}{"apply_patch": {}})
	chunk := []byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_hot","name":"apply_patch"}}`)
	raw := streamChunkEnvelope(t, "req-2", "openai-response", 5, chunk, "claude-opus-5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := chunkBodyFromEnvelope(t, out)
	if !strings.Contains(string(body), "custom_tool_call") || !strings.Contains(string(body), "ctc_call_hot") {
		t.Fatalf("expected custom_tool_call rewrite, got %s", body)
	}
}

func TestInterceptStreamChunkHeaderInitNoop(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	raw := streamChunkEnvelope(t, "req-3", "openai-response", -1, nil, "claude-opus-5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if body := chunkBodyFromEnvelope(t, out); len(body) != 0 {
		t.Fatalf("header-init must be a no-op, got %q", body)
	}
}

func TestInterceptStreamChunkTrackerDeltaWithoutState(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// Tracker-based deltas work even without request state (tracker keyed by item id).
	streamStates.put("req-4", "codex", map[string]struct{}{"apply_patch": {}})
	added := []byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_9","type":"function_call","status":"in_progress","arguments":"","call_id":"call_9","name":"apply_patch"}}`)
	if _, err := interceptStreamChunk(streamChunkEnvelope(t, "req-4", "openai-response", 1, added, "grok-4.5")); err != nil {
		t.Fatal(err)
	}
	delta := []byte(`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_9","delta":"*** Begin Patch"}`)
	// Use a fresh request id: deltas only need the tracker, not request state.
	out, err := interceptStreamChunk(streamChunkEnvelope(t, "req-5", "openai-response", 2, delta, "grok-4.5"))
	if err != nil {
		t.Fatal(err)
	}
	body := chunkBodyFromEnvelope(t, out)
	if !strings.Contains(string(body), "response.custom_tool_call_input.delta") {
		t.Fatalf("expected custom delta rewrite, got %s", body)
	}
}

func TestInterceptResponseAfterUsesRequestState(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	streamStates.put("req-ns", "claude", map[string]struct{}{"apply_patch": {}})
	respBody := []byte(`{"output":[{"id":"fc_1","type":"function_call","status":"completed","arguments":"{}","call_id":"call_ns","name":"apply_patch"}]}`)
	raw, err := json.Marshal(map[string]any{
		"RequestID":      "req-ns",
		"SourceFormat":   "openai-response",
		"Model":          "claude-opus-5",
		"RequestedModel": "claude-opus-5",
		"Stream":         false,
		"Body":           respBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := interceptResponseAfter(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var pr struct {
		Body []byte `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &pr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pr.Body), "custom_tool_call") {
		t.Fatalf("expected custom_tool_call rewrite, got %s", pr.Body)
	}
}

func TestInterceptResponseAfterFallsBackToOriginalRequest(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// Only a request observed before plugin load has no decision entry.
	// The compatibility fallback must enforce the same client gate.
	tools := []any{
		map[string]any{
			"type": "custom",
			"name": "apply_patch",
		},
	}
	orig, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "tools": tools})
	respBody := []byte(`{"output":[{"id":"fc_2","type":"function_call","status":"completed","arguments":"{}","call_id":"call_fb","name":"apply_patch"}]}`)
	raw, err := json.Marshal(map[string]any{
		"RequestID":       "req-fb",
		"SourceFormat":    "openai-response",
		"Model":           "claude-opus-5",
		"RequestedModel":  "claude-opus-5",
		"Stream":          false,
		"OriginalRequest": orig,
		"RequestHeaders":  map[string][]string{"User-Agent": {"codex_cli_rs/0.45.0"}},
		"Body":            respBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := interceptResponseAfter(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var pr struct {
		Body []byte `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &pr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pr.Body), "custom_tool_call") {
		t.Fatalf("expected fallback rewrite via OriginalRequest, got %s", pr.Body)
	}
}
