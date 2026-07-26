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
//   - RequestInterceptor (after auth, before executor translation — the only
//     stage where the upstream format is known) rewrites, exclusively for
//     ToFormat=claude, the freeform custom tools into {"input": string}
//     function tools and replays custom_tool_call(_output) history as
//     function_call(_output), which the native translator converts
//     losslessly. codex/xai/openai upstreams support custom tools natively
//     and are never touched.
//   - ResponseAfterTranslator rewrites, exclusively for claude→openai-response
//     translations, the function_call events of bridged tools back into
//     custom_tool_call events, mirroring the shapes in
//     internal/translator/openai/openai/responses/openai_openai-responses_response.go.
//     Bridged tools are recognized by the {"input": string} schema fingerprint
//     because CPA overwrites OriginalRequest with the interceptor's output.
//   - Layer ① (the catalog) has no plugin hook and is solved client-side via
//     model_catalog_json; see scripts/generate-model-catalog.ps1.
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
	"unicode/utf16"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "cpa-apply-patch"
	pluginVersion = "0.3.0"
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
	// An explicitly empty models/tools list matches nothing (fail-closed);
	// defaults apply only when the key is absent, via defaultPluginConfig.
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
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "总开关:关闭后停止全部改写,客户端将退回命令行改文件(插件仍保持加载)。"},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "为哪些模型桥接 Codex 原生工具。支持前缀通配,如 claude*;单独的 * 表示全部模型(安全:仅上游为 claude 的请求会被改写,GPT/grok 等原生路径不受影响)。默认 [\"*\"]。非 GPT 模型要真正拿到 apply_patch,还需在客户端 model_catalog_json 里给该模型补 apply_patch_tool_type: freeform。"},
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

func interceptRequestAfterAuth(raw []byte) ([]byte, error) {
	var req rpcRequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	// Only the OpenAI Responses protocol carries Codex custom tools, and only
	// the claude upstream format lacks native support for them. Every other
	// combination (codex/xai/openai upstreams, claude/gemini inbound clients)
	// is left strictly untouched, wildcards notwithstanding.
	if !strings.EqualFold(strings.TrimSpace(req.SourceFormat), "openai-response") ||
		!strings.EqualFold(strings.TrimSpace(req.ToFormat), "claude") {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if !cfg.Enabled || !modelEnabled(cfg, req.RequestedModel, req.Model, req.Body) {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	newBody, changed := rewriteRequestBody(cfg, req.Body)
	if !changed {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Body: newBody})
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
	// Strictly the claude→openai-response translation pair: the only path the
	// request side bridges. codex/xai native paths emit custom_tool_call
	// themselves and must not be touched twice.
	if !strings.EqualFold(req.FromFormat, "claude") || !strings.EqualFold(req.ToFormat, "openai-response") {
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
