# Changelog

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
