package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestIsXAIModel(t *testing.T) {
	yes := []string{"grok-4.5", "Grok-4.3", "grok-composer-2.5-fast"}
	no := []string{"", "claude-opus-5", "gpt-5.6-sol", "codex-auto-review"}
	for _, m := range yes {
		if !isXAIModel(m) {
			t.Fatalf("expected xai model: %s", m)
		}
	}
	for _, m := range no {
		if isXAIModel(m) {
			t.Fatalf("did not expect xai model: %s", m)
		}
	}
}

func TestRequestBackendEnabled(t *testing.T) {
	cfg := defaultPluginConfig()
	if !requestBackendEnabled(cfg, "claude", "claude-opus-5", "claude-opus-5", []byte(`{"model":"claude-opus-5"}`)) {
		t.Fatal("claude backend should enable claude ToFormat")
	}
	if !requestBackendEnabled(cfg, "codex", "grok-4.5", "grok-4.5", []byte(`{"model":"grok-4.5"}`)) {
		t.Fatal("xai backend should enable grok on codex ToFormat")
	}
	if requestBackendEnabled(cfg, "codex", "gpt-5.6-sol", "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`)) {
		t.Fatal("gpt on codex ToFormat must not be bridged")
	}
	cfg.BackendXAI = false
	if requestBackendEnabled(cfg, "codex", "grok-4.5", "grok-4.5", []byte(`{"model":"grok-4.5"}`)) {
		t.Fatal("disabled xai backend must not bridge")
	}
	cfg = defaultPluginConfig()
	if !requestBackendEnabled(cfg, "antigravity", "gemini-3.7-flash-high", "gemini-3.7-flash-high", []byte(`{"model":"gemini-3.7-flash-high"}`)) {
		t.Fatal("gemini backend should enable antigravity ToFormat")
	}
	if !requestBackendEnabled(cfg, "gemini", "gemini-3.7-flash-high", "gemini-3.7-flash-high", []byte(`{"model":"gemini-3.7-flash-high"}`)) {
		t.Fatal("gemini backend should enable gemini ToFormat")
	}
	cfg.BackendGemini = false
	if requestBackendEnabled(cfg, "antigravity", "gemini-3.7-flash-high", "gemini-3.7-flash-high", []byte(`{"model":"gemini-3.7-flash-high"}`)) {
		t.Fatal("disabled gemini backend must not bridge")
	}
}

func TestResponseBackendEnabled(t *testing.T) {
	cfg := defaultPluginConfig()
	if !responseBackendEnabled(cfg, "antigravity", "gemini-3.7-flash-high", "gemini-3.7-flash-high") {
		t.Fatal("gemini backend should rewrite antigravity responses")
	}
	if !responseBackendEnabled(cfg, "gemini", "gemini-3.7-flash-high", "") {
		t.Fatal("gemini backend should rewrite gemini responses")
	}
	if responseBackendEnabled(cfg, "codex", "gpt-5.6-sol", "gpt-5.6-sol") {
		t.Fatal("gpt responses must not be rewritten")
	}
	cfg.BackendGemini = false
	if responseBackendEnabled(cfg, "antigravity", "gemini-3.7-flash-high", "gemini-3.7-flash-high") {
		t.Fatal("disabled gemini backend must not rewrite responses")
	}
}

func TestInterceptRequestAfterAuthGeminiAntigravity(t *testing.T) {
	rawCfg, _ := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{})
	if err := configure(rawCfg); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gemini-3.7-flash-high","tools":[{"type":"custom","name":"apply_patch","description":"edit files"}]}`)
	raw, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai-response",
		ToFormat:       "antigravity",
		Model:          "gemini-3.7-flash-high",
		RequestedModel: "gemini-3.7-flash-high",
		Body:           body,
	})
	out, err := interceptRequestAfterAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("not ok: %s", out)
	}
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(resp.Body, "tools.0.type").String() != "function" {
		t.Fatalf("custom apply_patch not bridged: %s", resp.Body)
	}
	if gjson.GetBytes(resp.Body, "tools.0.name").String() != "apply_patch" {
		t.Fatalf("name=%s", gjson.GetBytes(resp.Body, "tools.0.name").String())
	}
}

func TestConfigureKeepsBackendDefaults(t *testing.T) {
	yamlText := "tools:\n  - apply_patch\n"
	payload, _ := json.Marshal(map[string]string{
		"config_yaml": base64.StdEncoding.EncodeToString([]byte(yamlText)),
	})
	// pluginabi uses []byte which json encodes as base64; construct via struct
	raw, _ := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(yamlText)})
	_ = payload
	if err := configure(raw); err != nil {
		t.Fatal(err)
	}
	cfg := loadedConfig()
	if !cfg.BackendClaude || !cfg.BackendXAI || !cfg.BackendGemini || !cfg.Enabled {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0] != "apply_patch" {
		t.Fatalf("tools=%v", cfg.Tools)
	}
}

func TestRewriteAgentMessageOnly(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{
				"type":"agent_message",
				"id":"amsg_test",
				"author":"/root",
				"recipient":"/root/blind_a_0717",
				"content":[
					{"type":"input_text","text":"Message Type: NEW_TASK\n"},
					{"type":"encrypted_content","encrypted_content":"你是华电 600726 专股 skill 盲测子 agent。"}
				]
			}
		]
	}`)
	cfg := defaultPluginConfig()
	out, changed := rewriteRequestBody(cfg, body)
	if !changed {
		t.Fatal("expected agent_message rewrite to mark changed")
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("input len=%d", len(items))
	}
	if items[0].Get("type").String() != "message" || items[0].Get("content.0.text").String() != "hi" {
		t.Fatalf("first message mutated: %s", items[0].Raw)
	}
	if items[1].Get("type").String() != "message" {
		t.Fatalf("agent_message not flattened: %s", items[1].Raw)
	}
	if items[1].Get("role").String() != "user" {
		t.Fatalf("role=%s", items[1].Get("role").String())
	}
	text := items[1].Get("content.0.text").String()
	for _, want := range []string{
		"[agent_message author=/root recipient=/root/blind_a_0717]",
		"Message Type: NEW_TASK",
		"华电 600726",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("flattened text missing %q:\n%s", want, text)
		}
	}
	if gjson.GetBytes(out, `input.#(type=="agent_message")`).Exists() {
		t.Fatal("agent_message still present after rewrite")
	}
}

func TestRewriteAgentMessageWithApplyPatchHistory(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"edit files","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}
		],
		"input":[
			{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch","status":"completed"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"Success"},
			{
				"type":"agent_message",
				"author":"/root",
				"recipient":"/root/child",
				"content":[{"type":"encrypted_content","encrypted_content":"do the task"}]
			}
		]
	}`)
	cfg := defaultPluginConfig()
	out, changed := rewriteRequestBody(cfg, body)
	if !changed {
		t.Fatal("expected rewrite")
	}
	if gjson.GetBytes(out, "tools.0.type").String() != "function" {
		t.Fatalf("apply_patch tool not bridged: %s", gjson.GetBytes(out, "tools.0").Raw)
	}
	if gjson.GetBytes(out, "input.0.type").String() != "function_call" {
		t.Fatalf("history call not bridged: %s", gjson.GetBytes(out, "input.0").Raw)
	}
	if gjson.GetBytes(out, "input.1.type").String() != "function_call_output" {
		t.Fatalf("history output not bridged: %s", gjson.GetBytes(out, "input.1").Raw)
	}
	if gjson.GetBytes(out, "input.2.type").String() != "message" {
		t.Fatalf("agent_message not flattened: %s", gjson.GetBytes(out, "input.2").Raw)
	}
	text := gjson.GetBytes(out, "input.2.content.0.text").String()
	if !strings.Contains(text, "do the task") {
		t.Fatalf("task text missing: %s", text)
	}
}
