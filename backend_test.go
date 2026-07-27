package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
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
	if !cfg.BackendClaude || !cfg.BackendXAI || !cfg.Enabled {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0] != "apply_patch" {
		t.Fatalf("tools=%v", cfg.Tools)
	}
}
