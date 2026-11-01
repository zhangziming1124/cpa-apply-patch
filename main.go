// cpa-apply-patch bridges the Codex client's freeform apply_patch tool onto
// upstreams behind CLIProxyAPI that would otherwise drop or mishandle it.
//
// Backend toggles (management panel checkboxes):
//   - backend_claude: bridge when ToFormat=claude
//   - backend_xai:    bridge when ToFormat=codex and model is Grok/xAI
//     (xAI executor hard-deletes type=custom name=apply_patch)
//   - backend_gemini: bridge when ToFormat=antigravity (Google OAuth) or gemini
//     (Gemini CLI). CPA only converts type=function tools into
//     Gemini functionDeclarations, so custom apply_patch is dropped.
//
// True OpenAI/codex GPT paths stay untouched.
//
// Request side converts custom tools to {"input": string} function tools before
// the upstream translator/executor runs, and flattens Codex agent_message items
// for Grok (xAI ModelInput rejects them with HTTP 422). Response side restores
// custom_tool_call events. Non-GPT clients still need model_catalog_json freeform
// declarations.
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
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/zhangziming1124/cpa-apply-patch/patchenv"
	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "cpa-apply-patch"
	pluginVersion = "0.6.1"
)

// streamStateTTL bounds how long per-request bridging metadata is retained
// after the last observed activity. Streams finish well within this window;
// the TTL only guards against leaks from abandoned requests.
const streamStateTTL = 15 * time.Minute

// streamState carries request-direction decisions (upstream format, bridged
// tool names) into the response-direction interceptors, so payload chunks
// never need OriginalRequest again. empty names means "not bridged".
type streamState struct {
	fromFormat string
	names      map[string]struct{}
	seen       time.Time
}

type streamStateTable struct {
	mu sync.Mutex
	m  map[string]*streamState
}

var streamStates = &streamStateTable{m: make(map[string]*streamState)}

func (t *streamStateTable) put(requestID, fromFormat string, names map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) >= 256 {
		cutoff := time.Now().Add(-streamStateTTL)
		for k, v := range t.m {
			if v.seen.Before(cutoff) {
				delete(t.m, k)
			}
		}
	}
	t.m[requestID] = &streamState{fromFormat: fromFormat, names: names, seen: time.Now()}
}

func (t *streamStateTable) get(requestID string) (*streamState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[requestID]
	if ok {
		v.seen = time.Now()
	}
	return v, ok
}

func (t *streamStateTable) drop(requestID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, requestID)
}

type pluginConfig struct {
	Enabled       bool     `yaml:"enabled"`
	BackendClaude bool     `yaml:"backend_claude"`
	BackendXAI    bool     `yaml:"backend_xai"`
	BackendGemini bool     `yaml:"backend_gemini"`
	Models        []string `yaml:"models"`
	Tools         []string `yaml:"tools"`
	// CodexOnly restricts bridging to clients whose User-Agent identifies
	// as Codex (codex_cli_rs / Codex Desktop / codex_ide). Empty or missing
	// UA still passes this gate: header-stripping deployments in front of
	// the proxy keep working, and the protocol/semantic gates below still
	// apply. Turn off to also bridge third-party Responses clients that
	// send freeform custom tools.
	CodexOnly bool `yaml:"codex_only"`
	// DebugStreamTiming logs per-chunk normalize latency to stderr when true.
	DebugStreamTiming bool `yaml:"debug_stream_timing"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:           true,
		BackendClaude:     true,
		BackendXAI:        true,
		BackendGemini:     true,
		Models:            []string{"*"},
		Tools:             []string{"apply_patch"},
		CodexOnly:         true,
		DebugStreamTiming: false,
	}
}

// configOverlay lets absent boolean keys keep defaults instead of yaml false.
type configOverlay struct {
	Enabled           *bool    `yaml:"enabled"`
	BackendClaude     *bool    `yaml:"backend_claude"`
	BackendXAI        *bool    `yaml:"backend_xai"`
	BackendGemini     *bool    `yaml:"backend_gemini"`
	Models            []string `yaml:"models"`
	Tools             []string `yaml:"tools"`
	CodexOnly         *bool    `yaml:"codex_only"`
	DebugStreamTiming *bool    `yaml:"debug_stream_timing"`
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
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
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
		// A malformed config must not eject the plugin from the host: keep the
		// previous (or default) config and register successfully anyway.
		_ = configure(request)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		// Before auth the upstream format is unknown; all rewriting happens
		// in the after-auth hook where ToFormat is populated.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequestAfterAuth(request)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponseAfter(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)
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
		var overlay configOverlay
		if errDecode := yaml.Unmarshal(req.ConfigYAML, &overlay); errDecode != nil {
			return errDecode
		}
		if overlay.Enabled != nil {
			cfg.Enabled = *overlay.Enabled
		}
		if overlay.BackendClaude != nil {
			cfg.BackendClaude = *overlay.BackendClaude
		}
		if overlay.BackendXAI != nil {
			cfg.BackendXAI = *overlay.BackendXAI
		}
		if overlay.BackendGemini != nil {
			cfg.BackendGemini = *overlay.BackendGemini
		}
		if overlay.DebugStreamTiming != nil {
			cfg.DebugStreamTiming = *overlay.DebugStreamTiming
		}
		if overlay.CodexOnly != nil {
			cfg.CodexOnly = *overlay.CodexOnly
		}
		// Explicit empty models/tools lists match nothing (fail-closed).
		// Absent keys keep defaults from defaultPluginConfig.
		if overlay.Models != nil {
			cfg.Models = overlay.Models
		}
		if overlay.Tools != nil {
			cfg.Tools = overlay.Tools
		}
	}
	cfg.Models = normalizePatterns(cfg.Models)
	cfg.Tools = normalizePatterns(cfg.Tools)
	currentConfig.Store(cfg)
	return nil
}

func normalizePatterns(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
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
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "总开关。关闭后停止全部改写，客户端将退回命令行改文件（插件仍保持加载）。"},
				{Name: "backend_claude", Type: pluginapi.ConfigFieldTypeBoolean, Description: "后端服务：Claude。开启后，上游格式为 claude 时桥接 apply_patch（CPA 会丢弃 freeform custom 工具）。默认开启。"},
				{Name: "backend_xai", Type: pluginapi.ConfigFieldTypeBoolean, Description: "后端服务：xAI / Grok。开启后，对 grok* 模型桥接 apply_patch，并把 Codex 子 agent 的 agent_message 摊成普通 message（避免 Grok ModelInput 422）。默认开启。不影响 GPT/OpenAI codex 原生路径。"},
				{Name: "backend_gemini", Type: pluginapi.ConfigFieldTypeBoolean, Description: "后端服务：Gemini / Antigravity。开启后，上游格式为 antigravity（Google 账号 OAuth）或 gemini（Gemini CLI）时桥接 apply_patch。CPA 只把 type=function 转成 functionDeclarations，custom apply_patch 会被静默丢掉。默认开启。"},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "模型名过滤（可与后端勾选叠加）。支持前缀通配，如 grok*、claude*；* 表示不额外限制模型名。默认 [\"*\"]。非 GPT 模型还须在客户端 model_catalog_json 补 apply_patch_tool_type: freeform。"},
				{Name: "tools", Type: pluginapi.ConfigFieldTypeArray, Description: "桥接哪些 Codex freeform(custom) 工具。默认 [\"apply_patch\"]；填 * 表示客户端声明的所有 custom 工具都桥接。"},
				{Name: "codex_only", Type: pluginapi.ConfigFieldTypeBoolean, Description: "仅服务 Codex 客户端：User-Agent 含 codex 标识（codex_cli_rs / Codex Desktop / codex_ide 等，大小写不敏感）才参与桥接，第三方 Responses 客户端原样放行。UA 缺失/为空时保守放行（前方有剥头中间层仍可用，后续协议/语义门继续把关）。默认开启；关闭则退回纯协议分流。"},
				{Name: "debug_stream_timing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "调试：把流式每片拦截耗时汇总打到 CPA stderr（avg/max）。默认关闭。"},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

// ---- request direction: custom tool → {"input": string} function ------------

func interceptRequestAfterAuth(raw []byte) ([]byte, error) {
	var req rpcRequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		if req.RequestID != "" {
			streamStates.drop(req.RequestID)
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// Client gate: the bridge only serves Codex clients. The client-sent
	// User-Agent carries an identifying "codex" token (Codex CLI / Codex
	// Desktop). Requests without a recognizable UA pass through untouched so
	// third-party Responses clients are never fingerprint-scanned. Empty or
	// missing UA conservatively proceeds; see codexClientUserAgent.
	if cfg.CodexOnly && !codexClientUserAgent(req.Headers) {
		if req.RequestID != "" {
			streamStates.drop(req.RequestID)
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// Codex freeform tools only arrive on the OpenAI Responses protocol.
	if !strings.EqualFold(strings.TrimSpace(req.SourceFormat), "openai-response") {
		if req.RequestID != "" {
			streamStates.drop(req.RequestID)
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if !requestBackendEnabled(cfg, req.ToFormat, req.RequestedModel, req.Model, req.Body) {
		if req.RequestID != "" {
			streamStates.drop(req.RequestID)
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if !modelEnabled(cfg, req.RequestedModel, req.Model, req.Body) {
		if req.RequestID != "" {
			streamStates.drop(req.RequestID)
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	newBody, changed := rewriteRequestBody(cfg, req.Body)
	// Record request-direction decisions so response-side interceptors never
	// need OriginalRequest again (the stream-chunk hook omits it on schema v3+).
	// Names come from the rewritten body: the bridge leaves a fingerprinted
	// {"input": string} function tool there even when nothing else changed.
	if req.RequestID != "" {
		if changed {
			streamStates.put(req.RequestID, strings.ToLower(strings.TrimSpace(req.ToFormat)), customToolNames(cfg, newBody))
		} else {
			streamStates.put(req.RequestID, strings.ToLower(strings.TrimSpace(req.ToFormat)), customToolNames(cfg, req.Body))
		}
	}
	if !changed {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Body: newBody})
}

// codexClientUserAgent reports whether the client identifies itself as Codex.
// Codex CLI / Codex Desktop send a User-Agent containing the token "codex"
// (case-insensitive), e.g. "codex_cli_rs/0.4x" or
// "Codex Desktop/0.164.0 ...". A missing User-Agent conservatively passes:
// the protocol, backend, and model gates below still apply, and custom tools
// only exist on the Responses protocol anyway. Requests with a UA that does
// not mention codex (third-party Responses clients, OpenRouter frontends)
// pass through untouched — no fingerprint scanning, no bridge, no state.
func codexClientUserAgent(headers http.Header) bool {
	if headers == nil {
		return true
	}
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	if ua == "" {
		return true
	}
	return strings.Contains(strings.ToLower(ua), "codex")
}

// modelEnabled reports whether this request's model is configured for Codex
// native-tool bridging. The client-requested name, the resolved execution
// model, and the body's own model field are all considered, since different
// paths populate different subsets of them.
func modelEnabled(cfg pluginConfig, requestedModel, execModel string, body []byte) bool {
	return matchesAny(cfg.Models, requestedModel) ||
		matchesAny(cfg.Models, execModel) ||
		matchesAny(cfg.Models, gjson.GetBytes(body, "model").String())
}

// requestBackendEnabled reports whether the selected upstream format/backend
// is checked on for this request.
func requestBackendEnabled(cfg pluginConfig, toFormat, requestedModel, execModel string, body []byte) bool {
	toFormat = strings.ToLower(strings.TrimSpace(toFormat))
	switch toFormat {
	case "claude":
		return cfg.BackendClaude
	case "codex":
		// xAI reuses the codex wire format. Only bridge Grok/xAI models so
		// true OpenAI/codex GPT paths keep native freeform apply_patch.
		return cfg.BackendXAI && (isXAIModel(requestedModel) || isXAIModel(execModel) || isXAIModel(gjson.GetBytes(body, "model").String()))
	case "gemini", "antigravity":
		// Gemini CLI uses ToFormat=gemini; Google-account OAuth uses antigravity.
		// Both wrap ConvertOpenAIResponsesRequestToGemini, which only keeps type=function.
		return cfg.BackendGemini
	default:
		return false
	}
}

// responseBackendEnabled reports whether response rewriting should run for the
// translator pair that produced this chunk.
func responseBackendEnabled(cfg pluginConfig, fromFormat, modelA, modelB string) bool {
	fromFormat = strings.ToLower(strings.TrimSpace(fromFormat))
	switch fromFormat {
	case "claude":
		return cfg.BackendClaude
	case "codex", "openai-response":
		return cfg.BackendXAI && (isXAIModel(modelA) || isXAIModel(modelB))
	case "gemini", "antigravity":
		return cfg.BackendGemini
	default:
		return false
	}
}

// isXAIModel identifies Grok/xAI model ids without relying on auth metadata.
func isXAIModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	return strings.HasPrefix(model, "grok") || strings.Contains(model, "grok-")
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
		// Outputs carry no tool name, so collect the bridged call ids first
		// (order-independent) and rewrite only their matching outputs.
		bridged := map[string]struct{}{}
		for _, item := range input.Array() {
			if item.Get("type").String() == "custom_tool_call" && matchesAny(cfg.Tools, item.Get("name").String()) {
				bridged[item.Get("call_id").String()] = struct{}{}
			}
		}
		for i, item := range input.Array() {
			switch item.Get("type").String() {
			case "custom_tool_call":
				if !matchesAny(cfg.Tools, item.Get("name").String()) {
					continue
				}
				callID := item.Get("call_id").String()
				rawInput := item.Get("input").String()
				if strings.EqualFold(item.Get("name").String(), "apply_patch") {
					rawInput = patchenv.Normalize(rawInput)
				}
				wrapped, _ := json.Marshal(map[string]string{"input": rawInput})
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
			case "agent_message":
				// Codex multi-agent history. xAI's Responses ModelInput enum
				// rejects type=agent_message (HTTP 422). Flatten to a normal
				// user message so Grok can still see the task text.
				raw, errMarshal := json.Marshal(agentMessageAsUserMessage(item))
				if errMarshal != nil {
					continue
				}
				if next, errSet := sjson.SetRawBytes(body, fmt.Sprintf("input.%d", i), raw); errSet == nil {
					body = next
					changed = true
				}
			}
		}
	}
	return body, changed
}

// agentMessageAsUserMessage converts a Codex collaboration agent_message item
// into a plain Responses message that xAI can deserialize.
func agentMessageAsUserMessage(item gjson.Result) map[string]any {
	msg := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": flattenAgentMessageText(item),
			},
		},
	}
	if id := strings.TrimSpace(item.Get("id").String()); id != "" {
		msg["id"] = id
	}
	return msg
}

// flattenAgentMessageText joins author/recipient metadata and content parts.
// Desktop currently places the task plaintext in encrypted_content parts.
func flattenAgentMessageText(item gjson.Result) string {
	var b strings.Builder
	author := strings.TrimSpace(item.Get("author").String())
	recipient := strings.TrimSpace(item.Get("recipient").String())
	fmt.Fprintf(&b, "[agent_message author=%s recipient=%s]", author, recipient)
	for _, part := range item.Get("content").Array() {
		var piece string
		switch part.Get("type").String() {
		case "input_text", "output_text", "text":
			piece = part.Get("text").String()
		case "encrypted_content":
			piece = part.Get("encrypted_content").String()
			if piece == "" {
				piece = part.Get("text").String()
			}
		default:
			continue
		}
		piece = strings.TrimRight(piece, "\r\n")
		if piece == "" {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(piece)
	}
	return b.String()
}

func customToolAsFunction(name, description string) map[string]any {
	desc := strings.TrimSpace(description)
	if name == "apply_patch" {
		if desc == "" {
			desc = "Create, update, or delete files."
		}
		desc += "\nProvide the complete patch envelope as the `input` string argument for the freeform apply_patch tool." +
			"\nEnvelope lines must match EXACTLY (no trailing stars, no extra spaces, no quotes):" +
			"\n  First line: *** Begin Patch" +
			"\n  Last line:  *** End Patch" +
			"\nWRONG (client rejects): *** Begin Patch ***   or   *** End Patch ***" +
			"\nRIGHT: *** Begin Patch   and   *** End Patch" +
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
	mu      sync.Mutex // guards the map only; per-entry work uses entry.mu
	entries map[string]*trackEntry
}

// trackEntry incrementally decodes the {"input": "..."} wrapper that the
// interceptor's function schema imposes. Each arguments fragment is consumed
// exactly once (O(fragment) per delta), the undecoded tail carried across
// fragments is at most a few bytes, and total consumption is capped, so both
// CPU and memory are bounded regardless of patch size.
type trackEntry struct {
	mu     sync.Mutex
	callID string
	seen   time.Time

	phase    int    // decodeSearchKey → decodeSeekQuote → decodeInValue → decodeDone/decodeFailed
	keyBuf   []byte // rolling tail while searching for the "input" key
	pend     []byte // undecoded tail of the value (incomplete escape sequence)
	consumed int64  // total fragment bytes fed, for the safety cap
}

const (
	decodeSearchKey = iota
	decodeSeekQuote
	decodeInValue
	decodeDone
	decodeFailed
)

const (
	inputKeyToken   = `"input"`
	decodeByteCap   = 8 << 20 // stop emitting deltas past 8 MiB of arguments
	trackerTTL      = 30 * time.Minute
	trackerGCTarget = 64
)

var tracker = &callTracker{entries: map[string]*trackEntry{}}

func (t *callTracker) put(itemID, callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) >= trackerGCTarget {
		cutoff := time.Now().Add(-trackerTTL)
		for k, v := range t.entries {
			if v.seen.Before(cutoff) {
				delete(t.entries, k)
			}
		}
	}
	t.entries[itemID] = &trackEntry{callID: callID, seen: time.Now()}
}

func (t *callTracker) lookup(itemID string) (*trackEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[itemID]
	return entry, ok
}

func (t *callTracker) get(itemID string) (string, bool) {
	entry, ok := t.lookup(itemID)
	if !ok {
		return "", false
	}
	return entry.callID, true
}

// appendDelta feeds one raw arguments fragment and returns the newly decoded
// portion of the unwrapped input string, so streamed deltas concatenate to
// exactly the value later carried by custom_tool_call_input.done.
func (t *callTracker) appendDelta(itemID, fragment string) (callID, newPart string, ok bool) {
	entry, found := t.lookup(itemID)
	if !found {
		return "", "", false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.seen = time.Now()
	entry.consumed += int64(len(fragment))
	if entry.consumed > decodeByteCap {
		entry.phase = decodeFailed
	}
	if entry.phase == decodeDone || entry.phase == decodeFailed {
		return entry.callID, "", true
	}
	return entry.callID, entry.consume(fragment), true
}

// consume advances the incremental decoder over one fragment and returns the
// decoded value bytes it yields. Malformed content flips the entry to
// decodeFailed: deltas go quiet and the done event stays authoritative.
func (e *trackEntry) consume(fragment string) string {
	var out strings.Builder
	data := fragment

	if e.phase == decodeSearchKey {
		e.keyBuf = append(e.keyBuf, data...)
		idx := strings.Index(string(e.keyBuf), inputKeyToken)
		if idx < 0 {
			if keep := len(inputKeyToken) - 1; len(e.keyBuf) > keep {
				e.keyBuf = e.keyBuf[len(e.keyBuf)-keep:]
			}
			return ""
		}
		data = string(e.keyBuf[idx+len(inputKeyToken):])
		e.keyBuf = nil
		e.phase = decodeSeekQuote
	}

	if e.phase == decodeSeekQuote {
		i := 0
		for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r' || data[i] == ':') {
			i++
		}
		if i >= len(data) {
			return ""
		}
		if data[i] != '"' {
			e.phase = decodeFailed
			return ""
		}
		data = data[i+1:]
		e.phase = decodeInValue
	}

	s := string(e.pend) + data
	e.pend = nil
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '"' {
			e.phase = decodeDone
			break
		}
		if c != '\\' {
			// Copy a run of plain bytes in one go.
			j := i
			for j < len(s) && s[j] != '"' && s[j] != '\\' {
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		if i+1 >= len(s) {
			e.pend = []byte(s[i:])
			break
		}
		switch s[i+1] {
		case '"', '\\', '/':
			out.WriteByte(s[i+1])
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			if i+6 > len(s) {
				e.pend = []byte(s[i:])
				i = len(s)
				break
			}
			code, errParse := strconv.ParseUint(s[i+2:i+6], 16, 32)
			if errParse != nil {
				e.phase = decodeFailed
				return out.String()
			}
			if code >= 0xD800 && code <= 0xDBFF {
				// High surrogate: need the paired \uXXXX low surrogate.
				if i+12 > len(s) {
					e.pend = []byte(s[i:])
					i = len(s)
					break
				}
				if s[i+6] != '\\' || s[i+7] != 'u' {
					e.phase = decodeFailed
					return out.String()
				}
				low, errLow := strconv.ParseUint(s[i+8:i+12], 16, 32)
				if errLow != nil {
					e.phase = decodeFailed
					return out.String()
				}
				out.WriteRune(utf16.DecodeRune(rune(code), rune(low)))
				i += 12
			} else {
				out.WriteRune(rune(code))
				i += 6
			}
		default:
			e.phase = decodeFailed
			return out.String()
		}
	}
	return out.String()
}

func (t *callTracker) drop(itemID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, itemID)
}

// rpcResponseInterceptRequest mirrors the host wire shape for
// response.intercept_after (non-streaming). The embedded struct uses
// []byte-with-base64 JSON semantics via encoding/json.
type rpcResponseInterceptRequest struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// interceptResponseAfter handles the non-streaming response path. The host
// calls it once per completed response with OriginalRequest attached (cheap
// at this frequency), so the classic lookup semantics stay unchanged.
func interceptResponseAfter(raw []byte) ([]byte, error) {
	var req rpcResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if len(req.Body) == 0 {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	// Non-stream responses only need rewriting when they carry a function_call
	// that maps to a bridged tool; everything else passes through untouched.
	if !bytesContainsFunctionCall(req.Body) {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	// An accepted request decision is sufficient to restore its own calls.
	state, haveState := streamStates.get(req.RequestID)
	if haveState && len(state.names) > 0 && responseBackendEnabled(cfg, state.fromFormat, req.Model, req.RequestedModel) {
		newBody, changed := rewriteNonStreamBody(req.Body, state.names)
		if changed {
			return okEnvelope(pluginapi.ResponseInterceptResponse{Body: newBody})
		}
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	// The request direction owns every bridging decision. If this request was
	// not accepted by the client/protocol/backend/model gates, its response
	// must remain untouched: deriving names from OriginalRequest here would
	// bypass the Codex client gate and could fight a host-native bridge.
	// A state entry with no names is also an explicit "not bridged" decision.
	if haveState {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if !strings.EqualFold(strings.TrimSpace(req.SourceFormat), "openai-response") {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if req.Stream {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if cfg.CodexOnly && !codexClientUserAgent(req.RequestHeaders) {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	// Only a request observed before plugin load has no decision entry. This
	// narrow compatibility fallback applies the same client gate as the
	// request interceptor before using OriginalRequest.
	orig := req.OriginalRequest
	if len(orig) == 0 {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if !matchesAny(cfg.Models, req.Model) && !matchesAny(cfg.Models, req.RequestedModel) &&
		!matchesAny(cfg.Models, gjson.GetBytes(orig, "model").String()) {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	names := customToolNames(cfg, orig)
	if len(names) == 0 {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	newBody, changed := rewriteNonStreamBody(req.Body, names)
	if !changed {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	return okEnvelope(pluginapi.ResponseInterceptResponse{Body: newBody})
}

// rpcStreamChunkInterceptRequest mirrors the host wire shape for
// response.intercept_stream_chunk.
type rpcStreamChunkInterceptRequest struct {
	pluginapi.StreamChunkInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// interceptStreamChunk handles the streaming hot path. The host calls it once
// per stream chunk with only the chunk body (schema v3+ omits
// OriginalRequest/RequestBody on payload chunks), so per-chunk cost no longer
// scales with conversation size. Bridging decisions come from the
// request-direction streamState table.
func interceptStreamChunk(raw []byte) ([]byte, error) {
	var req rpcStreamChunkInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	start := time.Now()
	defer func() { noteStreamTiming(cfg, time.Since(start)) }()

	// Header-init call: no payload to rewrite. Payload chunks may reference the
	// request state recorded at interception time, so nothing to cache here.
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	body := req.Body
	if len(body) == 0 {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	if !strings.EqualFold(strings.TrimSpace(req.SourceFormat), "openai-response") {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	if !isSSEChunk(body) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	event := sseEventType(body)
	if !sseEventMaybeApplyPatch(event) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}

	// arguments.delta/done only need the in-memory tracker, not tool names.
	// This is the dominant stream volume after a patch tool call starts.
	if event == "response.function_call_arguments.delta" || event == "response.function_call_arguments.done" {
		newBody, changed := rewriteSSEChunk(body, nil)
		if !changed {
			return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
		}
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{Body: newBody})
	}

	// output_item.* / completed: consult request-direction state; only proceed
	// when a bridgeable function_call is actually in the chunk.
	if event != "response.completed" && !sseChunkHasFunctionCall(body) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	if event == "response.completed" && !bytesContainsFunctionCall(body) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	state, ok := streamStates.get(req.RequestID)
	if !ok || len(state.names) == 0 {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	if !responseBackendEnabled(cfg, state.fromFormat, req.Model, req.RequestedModel) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	newBody, changed := rewriteSSEChunk(body, state.names)
	if !changed {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{Body: newBody})
}

// streamTimingAgg is updated only when debug_stream_timing is enabled.
var streamTimingAgg struct {
	mu      sync.Mutex
	count   int64
	sumNs   int64
	maxNs   int64
	lastLog time.Time
}

func noteStreamTiming(cfg pluginConfig, d time.Duration) {
	if !cfg.DebugStreamTiming {
		return
	}
	ns := d.Nanoseconds()
	streamTimingAgg.mu.Lock()
	defer streamTimingAgg.mu.Unlock()
	streamTimingAgg.count++
	streamTimingAgg.sumNs += ns
	if ns > streamTimingAgg.maxNs {
		streamTimingAgg.maxNs = ns
	}
	now := time.Now()
	if streamTimingAgg.count < 200 && now.Sub(streamTimingAgg.lastLog) < 2*time.Second {
		return
	}
	avg := streamTimingAgg.sumNs / streamTimingAgg.count
	fmt.Fprintf(os.Stderr, "[cpa-apply-patch] stream_timing n=%d avg_us=%d max_us=%d\n",
		streamTimingAgg.count, avg/1000, streamTimingAgg.maxNs/1000)
	streamTimingAgg.count = 0
	streamTimingAgg.sumNs = 0
	streamTimingAgg.maxNs = 0
	streamTimingAgg.lastLog = now
}

// sseEventType returns the Responses API event name for an SSE chunk, or "".
func sseEventType(chunk []byte) string {
	s := string(chunk)
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if strings.HasPrefix(trimmed, "event:") {
		if dataIdx := strings.Index(s, "\ndata: "); dataIdx >= 0 {
			return strings.TrimSpace(s[len("event: "):dataIdx])
		}
		// bare event line — no payload this frame
		line := trimmed
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	}
	if strings.HasPrefix(trimmed, "data:") {
		data := strings.TrimSpace(trimmed[len("data:"):])
		return gjson.Get(data, "type").String()
	}
	return ""
}

func sseEventMaybeApplyPatch(event string) bool {
	switch event {
	case "response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed":
		return true
	default:
		return false
	}
}

func sseChunkHasFunctionCall(chunk []byte) bool {
	// item.type is present on output_item.added/done frames we care about.
	return strings.Contains(string(chunk), `"type":"function_call"`) ||
		strings.Contains(string(chunk), `"type": "function_call"`)
}

func bytesContainsFunctionCall(body []byte) bool {
	return strings.Contains(string(body), `"type":"function_call"`) ||
		strings.Contains(string(body), `"type": "function_call"`)
}

// customToolNames collects (lowercased) the tool names that were bridged for
// this request. CPA overwrites OriginalRequest with the interceptor's output,
// so a bridged tool appears either as its original type=custom declaration
// (paths without interception) or as the interceptor's function form, which is
// recognized by its exact {"input": string} schema fingerprint. A client's own
// same-name function tool with a different schema matches neither and its
// responses are left alone.
func customToolNames(cfg pluginConfig, originalRequest []byte) map[string]struct{} {
	names := map[string]struct{}{}
	if tools := gjson.GetBytes(originalRequest, "tools"); tools.Exists() && tools.IsArray() {
		for _, tool := range tools.Array() {
			name := strings.TrimSpace(tool.Get("name").String())
			if name == "" || !matchesAny(cfg.Tools, name) {
				continue
			}
			switch tool.Get("type").String() {
			case "custom":
				names[strings.ToLower(name)] = struct{}{}
			case "function":
				if isBridgedFunctionSchema(tool) {
					names[strings.ToLower(name)] = struct{}{}
				}
			}
		}
	}
	return names
}

// isBridgedFunctionSchema reports whether a function tool declaration carries
// the exact parameter schema produced by customToolAsFunction.
func isBridgedFunctionSchema(tool gjson.Result) bool {
	params := tool.Get("parameters")
	if params.Get("type").String() != "object" {
		return false
	}
	if params.Get("additionalProperties").Exists() && params.Get("additionalProperties").Bool() {
		return false
	}
	req := params.Get("required")
	if !req.IsArray() || len(req.Array()) != 1 || req.Array()[0].String() != "input" {
		return false
	}
	props := params.Get("properties")
	if !props.IsObject() {
		return false
	}
	count := 0
	ok := true
	props.ForEach(func(key, value gjson.Result) bool {
		count++
		if key.String() != "input" || value.Get("type").String() != "string" {
			ok = false
		}
		return true
	})
	return ok && count == 1
}

func isSSEChunk(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	// xAI (and some other executors) emit "event:" and "data:" as separate
	// stream chunks. Treat both as SSE so the response rewriter can act.
	return strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:")
}

func rewriteSSEChunk(chunk []byte, names map[string]struct{}) ([]byte, bool) {
	s := string(chunk)
	trimmed := strings.TrimLeft(s, " \t\r\n")
	// Bare event lines carry no payload; the following data line has the JSON.
	if strings.HasPrefix(trimmed, "event:") && !strings.Contains(s, "\ndata:") {
		return nil, false
	}

	event := ""
	data := ""
	suffix := ""
	if strings.HasPrefix(trimmed, "event:") {
		dataIdx := strings.Index(s, "\ndata: ")
		if dataIdx < 0 {
			return nil, false
		}
		event = strings.TrimSpace(s[len("event: "):dataIdx])
		data = s[dataIdx+len("\ndata: "):]
	} else if strings.HasPrefix(trimmed, "data:") {
		// Standalone data line (xAI stream path).
		rest := strings.TrimSpace(trimmed[len("data:"):])
		data = rest
		event = gjson.Get(data, "type").String()
	} else {
		return nil, false
	}

	for strings.HasSuffix(data, "\n") {
		data = data[:len(data)-1]
		suffix += "\n"
	}
	if event == "" {
		event = gjson.Get(data, "type").String()
	}

	switch event {
	case "response.output_item.added":
		item := gjson.Get(data, "item")
		if item.Get("type").String() != "function_call" {
			return nil, false
		}
		name := item.Get("name").String()
		if _, ok := names[strings.ToLower(name)]; !ok {
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
		return assembleSSEData(event, newData, suffix, chunk), true

	case "response.function_call_arguments.delta":
		itemID := gjson.Get(data, "item_id").String()
		callID, newPart, ok := tracker.appendDelta(itemID, gjson.Get(data, "delta").String())
		if !ok {
			return nil, false
		}
		// Best-effort stream fix for the common Grok mistake so delta concat
		// stays close to the normalized done value. Spans that split mid-marker
		// are still corrected on .done / output_item.done.
		newPart = patchenv.NormalizeFragments(newPart)
		payload := map[string]any{
			"type":            "response.custom_tool_call_input.delta",
			"sequence_number": gjson.Get(data, "sequence_number").Int(),
			"item_id":         "ctc_" + callID,
			"output_index":    gjson.Get(data, "output_index").Int(),
			"delta":           newPart,
		}
		rawPayload, _ := json.Marshal(payload)
		return assembleSSEData("response.custom_tool_call_input.delta", string(rawPayload), suffix, chunk), true

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
		return assembleSSEData("response.custom_tool_call_input.done", string(rawPayload), suffix, chunk), true

	case "response.output_item.done":
		item := gjson.Get(data, "item")
		if item.Get("type").String() != "function_call" {
			return nil, false
		}
		name := item.Get("name").String()
		if _, ok := names[strings.ToLower(name)]; !ok {
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
		return assembleSSEData(event, newData, suffix, chunk), true

	case "response.completed":
		newData, changed := rewriteOutputArray(data, "response.output", names)
		if !changed {
			return nil, false
		}
		return assembleSSEData(event, newData, suffix, chunk), true
	}
	return nil, false
}

// assembleSSEData rebuilds an SSE chunk. Standalone data lines (xAI) stay as
// data-only frames; combined event+data inputs keep both lines.
func assembleSSEData(event, data, suffix string, original []byte) []byte {
	trimmed := strings.TrimLeft(string(original), " \t\r\n")
	if strings.HasPrefix(trimmed, "data:") {
		return []byte("data: " + data + suffix)
	}
	return []byte("event: " + event + "\ndata: " + data + suffix)
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
		if _, ok := names[strings.ToLower(name)]; !ok {
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
// apply_patch envelopes are normalized so Codex client verification accepts
// common model mistakes (trailing "***" on Begin/End lines).
func unwrapCustomToolInput(arguments string) string {
	raw := arguments
	if v := gjson.Get(arguments, "input"); v.Exists() {
		if v.Type == gjson.String {
			raw = v.String()
		} else {
			raw = v.Raw
		}
	}
	return patchenv.Normalize(raw)
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
