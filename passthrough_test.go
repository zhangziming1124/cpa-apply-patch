package main

// Regression: non-Codex clients (Claude Code on /v1/messages, Gemini CLI on
// /v1beta, OpenAI chat-completions tools on /v1/chat/completions) must pass
// through the plugin untouched in both directions.
//
// The discriminator is the protocol pair (SourceFormat/ToFormat), which the
// CPA host derives structurally from the API surface the client called —
// never from spoofable request headers. Freeform type=custom tools only
// exist on the OpenAI Responses protocol, so a non-openai-response
// SourceFormat means there is nothing to bridge by definition.

import (
	"encoding/json"
	"testing"
)

func requestInterceptEnvelope(t *testing.T, requestID, sourceFormat, toFormat, model string, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"RequestID":      requestID,
		"SourceFormat":   sourceFormat,
		"ToFormat":       toFormat,
		"Model":          model,
		"RequestedModel": model,
		"Stream":         true,
		"Body":           body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// passthroughResult asserts the envelope is ok and returns the rewritten
// Body (nil means "no modification requested").
func passthroughResult(t *testing.T, out []byte) []byte {
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

func TestRequestPassesThroughNonResponsesClients(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	cases := []struct {
		name, source, to, model string
		body                    []byte
	}{
		{
			// Claude Code: anthropic in, claude upstream.
			name: "claude-code", source: "anthropic", to: "claude", model: "claude-opus-5",
			body: []byte(`{"model":"claude-opus-5","tools":[{"name":"apply_patch","description":"Edit files"}],"messages":[{"role":"user","content":"hi"}]}`),
		},
		{
			// Any chat-completions client: openai in/out, even with an
			// apply_patch function tool whose schema matches the bridge
			// fingerprint — the protocol gate fires before fingerprinting.
			name: "chat-completions", source: "openai", to: "openai", model: "kimi-k3.1-pro",
			body: []byte(`{"model":"kimi-k3.1-pro","tools":[{"type":"function","function":{"name":"apply_patch","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}}}],"messages":[{"role":"user","content":"hi"}]}`),
		},
		{
			// Gemini CLI: gemini in/out.
			name: "gemini-cli", source: "gemini", to: "gemini", model: "gemini-3.7-flash-high",
			body: []byte(`{"model":"gemini-3.7-flash-high","tools":[{"functionDeclarations":[{"name":"apply_patch"}]}],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestID := "req-" + tc.name
			raw := requestInterceptEnvelope(t, requestID, tc.source, tc.to, tc.model, tc.body)
			out, err := interceptRequestAfterAuth(raw)
			if err != nil {
				t.Fatal(err)
			}
			if body := passthroughResult(t, out); len(body) != 0 {
				t.Fatalf("non-Responses client must not be rewritten, got %s", body)
			}
			// No bridging decision may be recorded: response-side hooks for
			// this request must stay inert too.
			if _, ok := streamStates.get(requestID); ok {
				t.Fatal("non-Responses client must not record stream state")
			}
		})
	}
}

func TestStreamChunkPassesThroughNonResponsesClients(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// Claude-protocol SSE frame. The SourceFormat gate returns before any
	// SSE parsing; even a recorded state from another request cannot make
	// this chunk rewrite.
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"in\"}}\n\n")
	raw := streamChunkEnvelope(t, "req-claude-stream", "anthropic", 7, chunk, "claude-opus-5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if body := chunkBodyFromEnvelope(t, out); len(body) != 0 {
		t.Fatalf("non-Responses stream chunk must pass through, got %s", body)
	}
}

func TestNonStreamResponsePassesThroughNonResponsesClients(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	// Claude-protocol tool_use response. Belt and suspenders: even without
	// the protocol gate, bytesContainsFunctionCall cannot match tool_use.
	respBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"apply_patch","input":{"patch":"x"}}]}`)
	raw, err := json.Marshal(map[string]any{
		"RequestID":      "req-claude-ns",
		"SourceFormat":   "anthropic",
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
	if body := passthroughResult(t, out); len(body) != 0 {
		t.Fatalf("non-Responses response must pass through, got %s", body)
	}
}

func TestStreamChunkHeaderInitStaysNoopForNonResponses(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	raw := streamChunkEnvelope(t, "req-hdr", "anthropic", -1, nil, "claude-opus-5")
	out, err := interceptStreamChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if body := chunkBodyFromEnvelope(t, out); len(body) != 0 {
		t.Fatalf("header-init must be a no-op, got %q", body)
	}
}

// A third-party Responses client (not Codex) uses ordinary JSON function
// tools — no freeform custom tools. It passes the protocol gate, yet the
// semantic gates below it (type=custom match, bridge fingerprint) cannot
// fire: the request rewrite is a no-op, no bridged names are recorded, and
// the response side is inert on empty names. This pins "no problem exists,
// plugin provably no-op" for today's mainstream non-Codex Responses clients.
func TestResponsesClientWithPlainFunctionToolsUntouched(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	requestID := "req-fn-tools"
	body := []byte(`{"model":"claude-opus-5","tools":[{"type":"function","name":"edit_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	raw := requestInterceptEnvelope(t, requestID, "openai-response", "claude", "claude-opus-5", body)
	out, err := interceptRequestAfterAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten := passthroughResult(t, out); len(rewritten) != 0 {
		t.Fatalf("function-tool Responses client must not be rewritten, got %s", rewritten)
	}
	if state, ok := streamStates.get(requestID); ok && len(state.names) != 0 {
		t.Fatalf("no bridged names may be recorded, got %v", state.names)
	}

	// Response side: a function_call for the client's own tool passes through.
	chunk := []byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"edit_file"}}`)
	outChunk, err := interceptStreamChunk(streamChunkEnvelope(t, requestID, "openai-response", 2, chunk, "claude-opus-5"))
	if err != nil {
		t.Fatal(err)
	}
	if b := chunkBodyFromEnvelope(t, outChunk); len(b) != 0 {
		t.Fatalf("foreign function_call must pass through, got %s", b)
	}

	// Its arguments deltas too (the tracker never learned this item id).
	delta := []byte(`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_1","delta":"x"}`)
	outDelta, err := interceptStreamChunk(streamChunkEnvelope(t, requestID, "openai-response", 3, delta, "claude-opus-5"))
	if err != nil {
		t.Fatal(err)
	}
	if b := chunkBodyFromEnvelope(t, outDelta); len(b) != 0 {
		t.Fatalf("foreign function_call delta must pass through, got %s", b)
	}
}

// The client gate: requests whose User-Agent does not identify as Codex pass
// through even on the Responses protocol with a fingerprintable function
// tool. Codex-flavored UAs proceed to the protocol gate. Missing/empty UA
// conservatively proceeds (older gate semantics).
func TestUserAgentGate(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	bridgable := []byte(`{"model":"glm-5.3","tools":[{"type":"custom","name":"apply_patch","description":"x"}],"input":[]}`)

	mk := func(ua string) []byte {
		var hdrs map[string][]string
		if ua != "" {
			hdrs = map[string][]string{"User-Agent": {ua}}
		}
		raw, err := json.Marshal(map[string]any{
			"RequestID":      "req-ua-" + ua,
			"SourceFormat":   "openai-response",
			"ToFormat":       "claude",
			"Model":          "claude-opus-5",
			"RequestedModel": "claude-opus-5",
			"Stream":         true,
			"Headers":        hdrs,
			"Body":           bridgable,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// Non-Codex UA: no rewrite, no state recorded.
	out, err := interceptRequestAfterAuth(mk("OpenRouter-Frontend/1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if body := passthroughResult(t, out); len(body) != 0 {
		t.Fatalf("non-Codex UA must pass through, got %s", body)
	}
	if _, ok := streamStates.get("req-ua-OpenRouter-Frontend/1.0"); ok {
		t.Fatal("non-Codex UA must not record stream state")
	}

	// Codex CLI UA: proceeds (state recorded — the bridge is engaged).
	out, err = interceptRequestAfterAuth(mk("codex_cli_rs/0.45.0 (Ubuntu; x86_64)"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := streamStates.get("req-ua-codex_cli_rs/0.45.0 (Ubuntu; x86_64)"); !ok {
		t.Fatal("Codex CLI UA should engage the bridge (state recorded)")
	}

	// Codex Desktop UA (capital C): case-insensitive match.
	out, err = interceptRequestAfterAuth(mk("Codex Desktop/0.164.0-alpha (Windows)"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := streamStates.get("req-ua-Codex Desktop/0.164.0-alpha (Windows)"); !ok {
		t.Fatal("Codex Desktop UA should engage the bridge")
	}

	// Missing User-Agent: conservative pass-through to protocol gates.
	raw := mk("")
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	delete(env, "Headers")
	raw, _ = json.Marshal(env)
	if _, err := interceptRequestAfterAuth(raw); err != nil {
		t.Fatal(err)
	}
}
