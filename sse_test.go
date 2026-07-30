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

func TestNormalizeResponseAfterSkipsTextDeltaWithoutOriginalRequest(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// Body only; no OriginalRequest field. Old path fully unmarshaled and still
	// worked (empty names → no rewrite). New path must return empty body quickly
	// and must not error when OriginalRequest is absent.
	raw, err := json.Marshal(map[string]any{
		"FromFormat": "claude",
		"ToFormat":   "openai-response",
		"Model":      "claude-opus-5",
		"Stream":     true,
		"Body":       []byte(`data: {"type":"response.output_text.delta","delta":"字"}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := normalizeResponseAfter(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := payloadBodyFromEnvelope(t, out)
	if len(body) != 0 {
		t.Fatalf("text delta must not rewrite body, got %q", body)
	}
}

func TestNormalizeResponseAfterStillRewritesFunctionCall(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	tools := []any{
		map[string]any{
			"type": "custom",
			"name": "apply_patch",
		},
	}
	orig, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "tools": tools})
	chunk := []byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_hot","name":"apply_patch"}}`)
	raw, err := json.Marshal(map[string]any{
		"FromFormat":      "claude",
		"ToFormat":        "openai-response",
		"Model":           "claude-opus-5",
		"Stream":          true,
		"OriginalRequest": orig,
		"Body":            chunk,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := normalizeResponseAfter(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := payloadBodyFromEnvelope(t, out)
	if !strings.Contains(string(body), "custom_tool_call") || !strings.Contains(string(body), "ctc_call_hot") {
		t.Fatalf("expected custom_tool_call rewrite, got %s", body)
	}
}

func payloadBodyFromEnvelope(t *testing.T, out []byte) []byte {
	t.Helper()
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
	if len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, &pr); err != nil {
		t.Fatal(err)
	}
	return pr.Body
}
