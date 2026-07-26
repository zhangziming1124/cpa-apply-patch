// cpa-apply-patch bridges the Codex client's apply_patch tool onto Claude
// backends behind CLIProxyAPI.
//
// CPA denies apply_patch to non-GPT models at three layers:
//  1. /v1/models?client_version strips apply_patch_tool_type from every model
//     that misses a template slug (internal/client/codex/models/models.go:254).
//  2. The openai-responses→claude request translator drops the freeform
//     apply_patch custom tool outright and silently loses
//     custom_tool_call(_output) history items.
//  3. The claude→openai-responses response translator only ever emits
//     function_call events, never the custom_tool_call events the Codex
//     desktop needs before it renders its patch-review card.
//
// This plugin threads the needle without patching CPA:
//   - ModelRegistrar registers a model whose ID hits an unused catalog
//     template slug (default gpt-5.3-codex-spark), so the catalog branch that
//     KEEPS apply_patch_tool_type=freeform serves it.
//   - ModelRouter routes that slug to the built-in claude provider with the
//     real target model (default claude-opus-5).
//   - RequestInterceptor (before auth, before translation) rewrites the
//     freeform custom tool into a {"input": string} function tool and replays
//     custom_tool_call(_output) history as function_call(_output), which the
//     native translator converts losslessly.
//   - ResponseAfterTranslator rewrites the translated function_call events for
//     those tools back into custom_tool_call events, mirroring the shapes in
//     internal/translator/openai/openai/responses/openai_openai-responses_response.go.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "cpa-apply-patch"
	pluginVersion = "0.2.1"
)

type pluginConfig struct {
	Enabled bool     `yaml:"enabled"`
	Models  []string `yaml:"models"`
	Tools   []string `yaml:"tools"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled: true,
		Models:  []string{"*"},
		Tools:   []string{"apply_patch"},
	}
}

var currentConfig atomic.Value

func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

// ---- envelope / RPC plumbing -------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor      bool `json:"request_interceptor"`
	ResponseAfterTranslator bool `json:"response_after_translator"`
}

type rpcRequestInterceptRequest struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequestBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodResponseNormalizeAfter:
		return normalizeResponseAfter(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
			return errDecode
		}
	}
	cfg.Models = normalizePatterns(cfg.Models, []string{"*"})
	cfg.Tools = normalizePatterns(cfg.Tools, []string{"apply_patch"})
	currentConfig.Store(cfg)
	return nil
}

func normalizePatterns(values, fallback []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// matchesAny reports whether value matches one of the configured patterns.
// A pattern may end with "*" to match by prefix; "*" alone matches everything.
func matchesAny(patterns []string, value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == value {
			return true
		}
	}
	return false
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "zhangziming",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "总开关:关闭后停止全部改写,客户端将退回命令行改文件(插件仍保持加载)。"},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "为哪些模型桥接 Codex 原生工具。支持前缀通配,如 claude*、grok*;单独的 * 表示全部模型。默认 [\"*\"]。非 GPT 模型要真正拿到 apply_patch,还需在客户端 model_catalog_json 里给该模型补 apply_patch_tool_type: freeform。"},
				{Name: "tools", Type: pluginapi.ConfigFieldTypeArray, Description: "桥接哪些 Codex freeform(custom)工具。默认 [\"apply_patch\"];填 * 表示客户端声明的所有 custom 工具都桥接。"},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:      true,
			ResponseAfterTranslator: true,
		},
	}
}

// ---- request direction: custom tool → {"input": string} function ------------

func interceptRequestBefore(raw []byte) ([]byte, error) {
	var req rpcRequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	// Only the OpenAI Responses protocol carries Codex custom tools; leave
	// requests from claude/gemini/chat-completions clients untouched even
	// when models/tools are configured with wildcards.
	if sf := strings.ToLower(strings.TrimSpace(req.SourceFormat)); sf != "" && sf != "openai-response" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if !cfg.Enabled || !modelEnabled(cfg, req.RequestedModel, req.Body) {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	newBody, changed := rewriteRequestBody(cfg, req.Body)
	if !changed {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Body: newBody})
}

// modelEnabled reports whether this request's model is configured for Codex
// native-tool bridging. Both the routing model name and the body's own model
// field are considered, since only one of them is populated on some paths.
func modelEnabled(cfg pluginConfig, requestedModel string, body []byte) bool {
	return matchesAny(cfg.Models, requestedModel) || matchesAny(cfg.Models, gjson.GetBytes(body, "model").String())
}

func rewriteRequestBody(cfg pluginConfig, body []byte) ([]byte, bool) {
	changed := false
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		for i, tool := range tools.Array() {
			if tool.Get("type").String() != "custom" {
				continue
			}
			name := strings.TrimSpace(tool.Get("name").String())
			if name == "" || !matchesAny(cfg.Tools, name) {
				continue
			}
			raw, errMarshal := json.Marshal(customToolAsFunction(name, tool.Get("description").String()))
			if errMarshal != nil {
				continue
			}
			if next, errSet := sjson.SetRawBytes(body, fmt.Sprintf("tools.%d", i), raw); errSet == nil {
				body = next
				changed = true
			}
		}
	}
	if input := gjson.GetBytes(body, "input"); input.Exists() && input.IsArray() {
		// Outputs carry no tool name, so remember which calls were bridged and
		// rewrite only their matching outputs.
		bridged := map[string]struct{}{}
		for i, item := range input.Array() {
			switch item.Get("type").String() {
			case "custom_tool_call":
				if !matchesAny(cfg.Tools, item.Get("name").String()) {
					continue
				}
				callID := item.Get("call_id").String()
				bridged[callID] = struct{}{}
				wrapped, _ := json.Marshal(map[string]string{"input": item.Get("input").String()})
				fc := map[string]any{
					"type":      "function_call",
					"id":        "fc_" + callID,
					"call_id":   callID,
					"name":      item.Get("name").String(),
					"arguments": string(wrapped),
				}
				if st := item.Get("status"); st.Exists() {
					fc["status"] = st.String()
				}
				raw, _ := json.Marshal(fc)
				if next, errSet := sjson.SetRawBytes(body, fmt.Sprintf("input.%d", i), raw); errSet == nil {
					body = next
					changed = true
				}
			case "custom_tool_call_output":
				callID := item.Get("call_id").String()
				if _, ok := bridged[callID]; !ok {
					continue
				}
				fco := []byte(`{"type":"function_call_output","call_id":""}`)
				fco, _ = sjson.SetBytes(fco, "call_id", callID)
				if out := item.Get("output"); out.Exists() {
					fco, _ = sjson.SetRawBytes(fco, "output", []byte(out.Raw))
				} else {
					fco, _ = sjson.SetBytes(fco, "output", "")
				}
				if st := item.Get("status"); st.Exists() {
					fco, _ = sjson.SetBytes(fco, "status", st.String())
				}
				if next, errSet := sjson.SetRawBytes(body, fmt.Sprintf("input.%d", i), fco); errSet == nil {
					body = next
					changed = true
				}
			}
		}
	}
	return body, changed
}

func customToolAsFunction(name, description string) map[string]any {
	desc := strings.TrimSpace(description)
	if name == "apply_patch" {
		if desc == "" {
			desc = "Create, update, or delete files."
		}
		desc += "\nProvide the complete patch envelope (\"*** Begin Patch\" through \"*** End Patch\") as the `input` string argument, exactly as you would emit it for the freeform apply_patch tool." +
			"\nPatch format rules (violations make the patch fail to apply):" +
			"\n- Every context line and every \"-\" removal line MUST reproduce one ENTIRE line of the target file, byte-exact from first character to last, including all leading indentation. Never quote a fragment of a line." +
			"\n- To change part of a line, remove the whole original line with \"-\" and add the whole modified line with \"+\"." +
			"\n- Include up to 3 complete unchanged lines of context around each change so the hunk anchors uniquely."
	} else if desc == "" {
		desc = "Provide the complete raw tool payload as the `input` string argument."
	} else {
		desc += "\nProvide the complete raw tool payload as the `input` string argument."
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": desc,
		"strict":      false,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "The complete raw tool input payload as plain text.",
				},
			},
			"required":             []string{"input"},
			"additionalProperties": false,
		},
	}
}

// ---- response direction: function_call → custom_tool_call --------------------

// callTracker remembers which translated function_call item ids belong to
// custom tools so arguments.delta/done events (which carry no tool name) can be
// rewritten. Claude tool_use ids are globally unique, so a process-wide map is
// safe across concurrent streams.
type callTracker struct {
	mu      sync.Mutex
	entries map[string]*trackEntry
}

type trackEntry struct {
	callID  string
	seen    time.Time
	rawArgs strings.Builder
	decoded string
}

var tracker = &callTracker{entries: map[string]*trackEntry{}}

func (t *callTracker) put(itemID, callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) > 512 {
		cutoff := time.Now().Add(-time.Hour)
		for k, v := range t.entries {
			if v.seen.Before(cutoff) {
				delete(t.entries, k)
			}
		}
	}
	t.entries[itemID] = &trackEntry{callID: callID, seen: time.Now()}
}

func (t *callTracker) get(itemID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[itemID]
	if !ok {
		return "", false
	}
	return entry.callID, true
}

// appendDelta feeds one raw arguments fragment and returns the newly decoded
// portion of the unwrapped input string, so streamed deltas concatenate to
// exactly the value later carried by custom_tool_call_input.done.
func (t *callTracker) appendDelta(itemID, fragment string) (callID, newPart string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, found := t.entries[itemID]
	if !found {
		return "", "", false
	}
	entry.seen = time.Now()
	entry.rawArgs.WriteString(fragment)
	decoded := decodeWrappedInputPrefix(entry.rawArgs.String())
	if len(decoded) > len(entry.decoded) && strings.HasPrefix(decoded, entry.decoded) {
		newPart = decoded[len(entry.decoded):]
		entry.decoded = decoded
	}
	return entry.callID, newPart, true
}

func (t *callTracker) drop(itemID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, itemID)
}

// decodeWrappedInputPrefix incrementally decodes the {"input": "..."} wrapper
// that the interceptor's function schema imposes: given a prefix of the raw
// arguments JSON, it returns the decoded value of the input string so far,
// stopping before any incomplete escape sequence.
func decodeWrappedInputPrefix(raw string) string {
	keyIdx := strings.Index(raw, `"input"`)
	if keyIdx < 0 {
		return ""
	}
	i := keyIdx + len(`"input"`)
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r' || raw[i] == ':') {
		i++
	}
	if i >= len(raw) || raw[i] != '"' {
		return ""
	}
	value := raw[i+1:]
	prefix := longestCompleteJSONStringPrefix(value)
	if prefix == "" {
		return ""
	}
	var out string
	if errUnmarshal := json.Unmarshal([]byte(`"`+prefix+`"`), &out); errUnmarshal != nil {
		return ""
	}
	return out
}

// longestCompleteJSONStringPrefix returns the longest prefix of a JSON string
// body (content between the quotes) that ends on a complete escape sequence
// and stops at the closing quote.
func longestCompleteJSONStringPrefix(s string) string {
	i := 0
	lastComplete := 0
	for i < len(s) {
		c := s[i]
		if c == '"' {
			break
		}
		if c == '\\' {
			if i+1 >= len(s) {
				break
			}
			if s[i+1] == 'u' {
				if i+6 > len(s) {
					break
				}
				code, errParse := strconv.ParseUint(s[i+2:i+6], 16, 32)
				if errParse != nil {
					break
				}
				if code >= 0xD800 && code <= 0xDBFF {
					// High surrogate: require the full low-surrogate escape too.
					if i+12 > len(s) || s[i+6] != '\\' || s[i+7] != 'u' {
						break
					}
					i += 12
				} else {
					i += 6
				}
			} else {
				i += 2
			}
		} else {
			i++
		}
		lastComplete = i
	}
	return s[:lastComplete]
}

func normalizeResponseAfter(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.ResponseTransformRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled || len(req.Body) == 0 {
		return okEnvelope(pluginapi.PayloadResponse{})
	}
	// Only rewrite translations whose client side is the OpenAI Responses
	// protocol; other format pairs (claude→claude passthrough, gemini, chat
	// completions) never carry Codex custom_tool_call events.
	if !strings.EqualFold(req.FromFormat, "openai-response") && !strings.EqualFold(req.ToFormat, "openai-response") {
		return okEnvelope(pluginapi.PayloadResponse{})
	}
	origModel := gjson.GetBytes(req.OriginalRequest, "model").String()
	if !matchesAny(cfg.Models, origModel) && !matchesAny(cfg.Models, req.Model) {
		return okEnvelope(pluginapi.PayloadResponse{})
	}
	names := customToolNames(cfg, req.OriginalRequest)
	var newBody []byte
	var changed bool
	if isSSEChunk(req.Body) {
		newBody, changed = rewriteSSEChunk(req.Body, names)
	} else {
		newBody, changed = rewriteNonStreamBody(req.Body, names)
	}
	if !changed {
		return okEnvelope(pluginapi.PayloadResponse{})
	}
	return okEnvelope(pluginapi.PayloadResponse{Body: newBody})
}

// customToolNames collects the configured tool names the client declared as
// freeform custom tools. Explicitly configured names are always included: on
// some paths OriginalRequest is captured after the request interceptor already
// rewrote the declaration into a function tool, which would otherwise hide it.
func customToolNames(cfg pluginConfig, originalRequest []byte) map[string]struct{} {
	names := map[string]struct{}{}
	for _, t := range cfg.Tools {
		if t != "*" && !strings.HasSuffix(t, "*") {
			names[t] = struct{}{}
		}
	}
	if tools := gjson.GetBytes(originalRequest, "tools"); tools.Exists() && tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "custom" {
				continue
			}
			name := strings.TrimSpace(tool.Get("name").String())
			if name != "" && matchesAny(cfg.Tools, name) {
				names[name] = struct{}{}
			}
		}
	}
	return names
}

func isSSEChunk(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "event:")
}

func rewriteSSEChunk(chunk []byte, names map[string]struct{}) ([]byte, bool) {
	s := string(chunk)
	dataIdx := strings.Index(s, "\ndata: ")
	if dataIdx < 0 || !strings.HasPrefix(s, "event: ") {
		return nil, false
	}
	event := strings.TrimSpace(s[len("event: "):dataIdx])
	data := s[dataIdx+len("\ndata: "):]
	suffix := ""
	for strings.HasSuffix(data, "\n") {
		data = data[:len(data)-1]
		suffix += "\n"
	}

	switch event {
	case "response.output_item.added":
		item := gjson.Get(data, "item")
		if item.Get("type").String() != "function_call" {
			return nil, false
		}
		name := item.Get("name").String()
		if _, ok := names[name]; !ok {
			return nil, false
		}
		callID := item.Get("call_id").String()
		tracker.put(item.Get("id").String(), callID)
		newItem := map[string]any{
			"id":      "ctc_" + callID,
			"type":    "custom_tool_call",
			"status":  "in_progress",
			"input":   "",
			"call_id": callID,
			"name":    name,
		}
		rawItem, _ := json.Marshal(newItem)
		newData, errSet := sjson.SetRaw(data, "item", string(rawItem))
		if errSet != nil {
			return nil, false
		}
		return assembleSSE(event, newData, suffix), true

	case "response.function_call_arguments.delta":
		itemID := gjson.Get(data, "item_id").String()
		callID, newPart, ok := tracker.appendDelta(itemID, gjson.Get(data, "delta").String())
		if !ok {
			return nil, false
		}
		payload := map[string]any{
			"type":            "response.custom_tool_call_input.delta",
			"sequence_number": gjson.Get(data, "sequence_number").Int(),
			"item_id":         "ctc_" + callID,
			"output_index":    gjson.Get(data, "output_index").Int(),
			"delta":           newPart,
		}
		rawPayload, _ := json.Marshal(payload)
		return assembleSSE("response.custom_tool_call_input.delta", string(rawPayload), suffix), true

	case "response.function_call_arguments.done":
		itemID := gjson.Get(data, "item_id").String()
		callID, ok := tracker.get(itemID)
		if !ok {
			return nil, false
		}
		payload := map[string]any{
			"type":            "response.custom_tool_call_input.done",
			"sequence_number": gjson.Get(data, "sequence_number").Int(),
			"item_id":         "ctc_" + callID,
			"output_index":    gjson.Get(data, "output_index").Int(),
			"input":           unwrapCustomToolInput(gjson.Get(data, "arguments").String()),
		}
		rawPayload, _ := json.Marshal(payload)
		return assembleSSE("response.custom_tool_call_input.done", string(rawPayload), suffix), true

	case "response.output_item.done":
		item := gjson.Get(data, "item")
		if item.Get("type").String() != "function_call" {
			return nil, false
		}
		name := item.Get("name").String()
		if _, ok := names[name]; !ok {
			return nil, false
		}
		callID := item.Get("call_id").String()
		tracker.drop(item.Get("id").String())
		newItem := map[string]any{
			"id":      "ctc_" + callID,
			"type":    "custom_tool_call",
			"status":  "completed",
			"input":   unwrapCustomToolInput(item.Get("arguments").String()),
			"call_id": callID,
			"name":    name,
		}
		rawItem, _ := json.Marshal(newItem)
		newData, errSet := sjson.SetRaw(data, "item", string(rawItem))
		if errSet != nil {
			return nil, false
		}
		return assembleSSE(event, newData, suffix), true

	case "response.completed":
		newData, changed := rewriteOutputArray(data, "response.output", names)
		if !changed {
			return nil, false
		}
		return assembleSSE(event, newData, suffix), true
	}
	return nil, false
}

func assembleSSE(event, data, suffix string) []byte {
	return []byte("event: " + event + "\ndata: " + data + suffix)
}

func rewriteNonStreamBody(body []byte, names map[string]struct{}) ([]byte, bool) {
	s := string(body)
	if gjson.Get(s, "output").IsArray() {
		return toBytesChanged(rewriteOutputArray(s, "output", names))
	}
	if gjson.Get(s, "response.output").IsArray() {
		return toBytesChanged(rewriteOutputArray(s, "response.output", names))
	}
	return nil, false
}

func toBytesChanged(s string, changed bool) ([]byte, bool) {
	if !changed {
		return nil, false
	}
	return []byte(s), true
}

func rewriteOutputArray(doc, path string, names map[string]struct{}) (string, bool) {
	output := gjson.Get(doc, path)
	if !output.IsArray() {
		return doc, false
	}
	changed := false
	for i, item := range output.Array() {
		if item.Get("type").String() != "function_call" {
			continue
		}
		name := item.Get("name").String()
		if _, ok := names[name]; !ok {
			continue
		}
		callID := item.Get("call_id").String()
		status := item.Get("status").String()
		if status == "" {
			status = "completed"
		}
		newItem := map[string]any{
			"id":      "ctc_" + callID,
			"type":    "custom_tool_call",
			"status":  status,
			"input":   unwrapCustomToolInput(item.Get("arguments").String()),
			"call_id": callID,
			"name":    name,
		}
		rawItem, _ := json.Marshal(newItem)
		next, errSet := sjson.SetRaw(doc, fmt.Sprintf("%s.%d", path, i), string(rawItem))
		if errSet != nil {
			continue
		}
		doc = next
		changed = true
	}
	return doc, changed
}

// unwrapCustomToolInput mirrors CPA's helper of the same name: the function
// arguments are {"input": "<raw payload>"}; unwrap to the raw string.
func unwrapCustomToolInput(arguments string) string {
	if v := gjson.Get(arguments, "input"); v.Exists() {
		if v.Type == gjson.String {
			return v.String()
		}
		return v.Raw
	}
	return arguments
}

// ---- envelope helpers --------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
