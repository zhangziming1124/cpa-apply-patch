# Changelog

## v0.6.0 — 2026-11-01

- **客户端 UA 门(可配置)**:新增 `codex_only`(默认开启)。请求方向在协议门之前先看客户端 User-Agent——含 `codex` 标识(`codex_cli_rs` / `Codex Desktop` / `codex_ide_*`,大小写不敏感)才参与桥接,其余第三方 Responses 客户端原样放行(不做指纹扫描、不记 stream state、响应侧保持惰性)。UA 缺失/为空时保守放行:前方有剥头中间层的部署仍可用,协议门/语义门(type=custom 匹配 + schema 指纹)继续兜底。设 `codex_only: false` 可退回纯协议分流。响应方向不需要 UA 门:桥接决策已在请求方向按 RequestID 记入 streamStates,非 Codex 请求无记录即天然透传。宿主头链路已核验:gin 原始请求头经 `opts.Headers` → conductor `cloneRequestHeaders` 原样抵达 after-auth 拦截器。

## v0.5.0 — 2026-11-01

- **流式热路径重构:告别每帧全量 OriginalRequest 序列化**。此前注册的 `response_after_translator` 让宿主(CPA 主程序)对**每个 SSE 流式帧**都执行 `json.Marshal` 全量信封——包含 1MB+ 的 `OriginalRequest` 完整对话上下文——再跨界传给插件。大上下文 Codex 会话(300k token ≈ 1MB/帧 × 每流几百帧)下,生产实测 22 分钟累计分配 **107GB**,容器多次 OOM(768MB 限额,66 分钟内被杀 16 次)。v0.5.0 改用拦截器三件套:
  - 流式帧 → `response.intercept_stream_chunk`(schema v3+ 宿主在 payload 帧上**省略** OriginalRequest/RequestBody,仅传 Body+小字段);
  - 非流式 → `response.intercept_after`(每请求一次,保留 OriginalRequest 回退路径);
  - 桥接决策(上游格式、bridged 工具名)在**请求方向**一次性算好,按 RequestID 存入 `streamStates` 会话表(15min TTL、256 上限),响应方向每帧 O(1) 查表,不再按帧解码 OriginalRequest。
  - A/B 实测(生产流量):注册面切换后 `json.Marshal` 分配速率从 **4.9GB/min 降至 1.2MB/s(-98.6%)**,同窗口流式请求正常。
  - 依赖 CLIProxyAPI v7.2.101 → v8.0.46(RequestID / stream-chunk v3 省略需要)。v7 宿主上 `response_stream_interceptor` 声明被忽略、行为退化为不桥接响应(请求方向改写仍在);建议 v8 宿主使用。
  - 移除 `normalizeResponseAfter` 与 peek/extract 字节扫描函数族(其存在意义就是绕开宿主每帧携带的大字段)。
  - `debug_stream_timing` 语义不变,统计对象改为 stream-chunk 拦截耗时。

## v0.4.6 — 2026-08-21

- **Gemini / Antigravity 原生 apply_patch 桥接**：新增 `backend_gemini` 勾选（默认开）。CPA 的 OpenAI Responses → Gemini/Antigravity 翻译器只把 `type=function` 转成 `functionDeclarations`，`type=custom` 的 apply_patch 会被静默丢掉。请求侧在 `ToFormat=antigravity|gemini` 时先改成 `{input:string}` function；响应侧把 `FromFormat=antigravity|gemini` 的 `function_call` 改回 `custom_tool_call`。覆盖 Google 账号 OAuth（Antigravity）和 Gemini CLI 两条上游。GPT 原生路径仍不碰。

## v0.4.5 — 2026-08-04

- **Grok + Codex 子 agent 422 修复**：请求侧把 `input[]` 中的 `agent_message` 摊成普通 `message`（`role=user`），并把 content 里的 `input_text` / `encrypted_content` 拼成可读正文。xAI Responses 的 `ModelInput` 不认识 `agent_message`，会直接 HTTP 422（`data did not match any variant of untagged enum ModelInput`）；仅在 `backend_xai` + grok* 路径生效，GPT 原生路径不动。
- 与既有 apply_patch 历史桥接可同请求共存；单独只有 `agent_message` 时也会标记 body changed。

## v0.4.4 — 2026-07-31

- **流式热路径再抠**：文本/推理 SSE 帧不再对整个 normalize 信封做 json.Unmarshal。字节扫描读取头字段，并从尾部定位 Body（宿主字段顺序下 Body 在最后），避免扫过大段 OriginalRequest base64。
- 新增配置 debug_stream_timing：开启后把 normalize 耗时 avg/max 打到 CPA stderr，便于对照 per-chunk 开销。
- OriginalRequest 仅在真正需要改写 apply_patch 时按字段名抽取解码。

## v0.4.3 — 2026-07-30

- **流式出字卡顿修复**：`response.normalize_after` 热路径不再在每个 SSE 文本/推理 delta 上 base64 反序列化整份 `OriginalRequest`（常 1MB+）。先解析小字段 + `Body`，非 `function_call*` 相关事件立即透传；仅在可能改写 apply_patch 的帧上才加载 tools 名单。
- `function_call_arguments.delta/done` 只依赖进程内 tracker，不再扫 tools。
- 宿主侧仍会每片携带 OriginalRequest（CPA 限制），但插件侧避免了大字段解码，Desktop 一字一字出的观感应明显改善。

## v0.4.2 — 2026-07-27

- **Grok 原生 apply_patch 信封归一化**：模型常把首行写成 `*** Begin Patch ***`（尾部多 ` ***`），Codex 客户端校验直接失败并退回 shell 写盘。响应侧 `unwrapCustomToolInput` / 流式 delta 与请求历史改写统一归一为 `*** Begin Patch` / `*** End Patch`，继续走 `custom_tool_call` 原生路径与桌面审阅卡。
- 工具描述写明「首行/末行不得带尾部星号」，降低模型再生成错误信封的概率。

## v0.4.1

- xAI SSE：支持 standalone `data:` 行改写，Grok 响应侧恢复 `custom_tool_call`（见 v0.4.1 修复说明）。

## v0.4.0

- Config panel: add backend service toggles `backend_claude` / `backend_xai` (checkbox-style booleans) instead of hard-coding Claude only.
- Bridge Grok/xAI: when `backend_xai` is on, rewrite freeform apply_patch for grok* models on ToFormat=codex before CPA's xAI executor deletes custom apply_patch.
- Keep GPT/OpenAI codex native paths untouched (ToFormat=codex + non-grok models are not bridged).
- Absent boolean keys keep defaults (true/true) via overlay unmarshalling.

## v0.3.0 — 2026-07-26

26 个评审 agent 的对抗性终审后修复(3 高危 + 多项中低危):

- **[高] GPT 原生路径误伤**:请求改写从 BeforeAuth 移到 AfterAuth,并按上游格式门控(仅 `ToFormat=claude` 改写)——codex/xai 等原生支持 custom 工具的上游在任何配置下都不再被触碰。
- **[高] 通配符配置下响应侧静默断裂**:CPA 会用拦截器输出覆盖 `OriginalRequest`,导致响应侧扫不到 `type=custom` 声明。改为 `{"input": string}` schema 指纹识别桥接工具,`tools: ["*"]` 等通配配置恢复双向工作;同名但 schema 不同的客户端自有 function 工具被正确排除(修复误改写)。
- **[高] O(n²) 流式解码**:重写为真正的增量解码器(每分片 O(fragment),状态跨分片保持,8MiB 安全上限),全局锁改为 per-entry 锁,数百 KB 大补丁不再卡顿、并发流互不阻塞;内存占用有硬上界(不再累积原始 arguments)。
- 历史项改写改为两遍扫描,`custom_tool_call_output` 先于其 call 出现时也能正确配对。
- 工具名匹配统一大小写不敏感;`models: []` 显式空列表改为匹配空集(fail-closed);响应侧严格限定 claude→openai-response 翻译对;坏 YAML 的 reconfigure 保留旧配置而不是让宿主剔除插件。
- 新增单测:增量解码分片重放逐字节一致性、失败静默、指纹判别。

## v0.2.1 — 2026-07-26

- 请求拦截器增加协议门槛:仅处理 `openai-response` 入站协议,claude/gemini/chat-completions 直连客户端即使配置了通配符也不会被误改写。
- 响应改写增加格式对门槛:仅处理翻译对含 `openai-response` 的方向。

## v0.2.0 — 2026-07-26

- **移除借壳模式**:删除 ModelRegistrar / ModelRouter 能力及 slug / display_name / target_provider / target_model / context_length 配置。目录层改由客户端 `model_catalog_json` 本地覆盖解决,模型保持真实身份与真实上下文窗口。
- 配置简化为 `enabled` / `models`(前缀通配)/ `tools`,面板说明中文化。
- 桥接范围从 claude 系扩展为按配置匹配的任意模型;`custom_tool_call_output` 仅在配对的 call 被桥接时改写。

## v0.1.x — 2026-07-26(迭代过程,已被 v0.2 取代)

- v0.1.5 配置面板中文化;v0.1.4 支持禁用借壳注册;v0.1.3 桥接门槛放宽到全部 claude 系模型。
- v0.1.2 apply_patch 工具描述加入补丁格式硬规则(整行逐字节复制、含缩进、上下文锚定),缓解模型半行补丁问题。
- v0.1.1 流式 delta 改为增量 JSON 字符串解码,保证 `custom_tool_call_input.delta` 累加与 `.done` 逐字节一致。
- v0.1.0 首版:借壳 GPT 模板 slug 注册模型 + ModelRouter 路由到 Claude + 请求/响应双向改写,三层链路首次打通。

