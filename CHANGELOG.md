# Changelog

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
