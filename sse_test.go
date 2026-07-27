package main

import (
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
