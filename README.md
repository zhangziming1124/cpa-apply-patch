# cpa-apply-patch

> A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that bridges the Codex client's native `apply_patch` tool to non-GPT backends (Claude, Grok, DeepSeek, Qwen, Kimi, …), restoring in-app diff review cards and one-click undo for every model behind your proxy.

一个 CLIProxyAPI(下称 CPA)动态插件:让 Codex 客户端(CLI / 桌面端)在使用 **非 GPT 模型** 时也能用上原生 `apply_patch` 工具——文件改动走结构化补丁而不是 shell 命令,桌面端恢复「已编辑 N 个文件 / 查看更改 / 撤销 / 审核」差异卡片。

## 解决什么痛点

Codex 客户端通过 CPA 调 Claude 等第三方模型时,**apply_patch 工具会凭空消失**:模型只能用 shell 命令改文件,桌面端没有差异审核卡、没有一键撤销。这不是配置问题,而是 CPA 上游在**三层**刻意拦截(行号基于 v7.2.101,commit `42a00a2a`):

| 层 | 位置 | 行为 |
|---|---|---|
| ① 模型目录 | `internal/client/codex/models/models.go:254` | `/v1/models?client_version` 下发目录时,凡是不在 GPT 模板列表里的模型,一律 `delete(entry, "apply_patch_tool_type")`,客户端因此不启用 apply_patch(有单测把该行为固化为预期) |
| ② 请求翻译 | `internal/translator/claude/openai/responses/claude_openai-responses_request.go:632-648` | OpenAI Responses → Claude 方向,`type=custom, name=apply_patch` 的工具定义被**点名丢弃**;`custom_tool_call(_output)` 历史项无对应分支,静默丢失 |
| ③ 响应翻译 | `internal/translator/claude/openai/responses/claude_openai-responses_response.go` | Claude → OpenAI Responses 方向,所有 `tool_use` 一律译成 `function_call`,永远不产生桌面端渲染差异卡所需的 `custom_tool_call` 事件 |

本插件不改 CPA 源码、不换官方二进制,用 CPA 自带的插件系统(C ABI 动态库,官方发行版原生支持)把三层逐一打通:

- **② 请求方向** — `RequestInterceptor.BeforeAuth` 在翻译前把 freeform custom 工具改写成 `{"input": string}` 标准函数工具,`custom_tool_call(_output)` 历史改写成 `function_call(_output)`。丢弃逻辑只认 custom 类型,函数形态畅通无阻,原生翻译器无损转成 Claude 工具与 tool_use/tool_result。
- **③ 响应方向** — `ResponseAfterTranslator` 把翻译产物里这些工具的 `function_call` 事件族改写回 `custom_tool_call` 事件族(`output_item.added` / `custom_tool_call_input.delta` / `.done` / `output_item.done` / `response.completed`),流式 delta 经增量 JSON 字符串解码,**保证 delta 累加与 done 值逐字节一致**,符合官方 Responses API 语义。
- **① 目录层在客户端解决** — CPA 的目录响应链没有插件挂点,但 Codex 客户端支持 `model_catalog_json` 本地目录覆盖:用 [`scripts/generate-model-catalog.ps1`](scripts/generate-model-catalog.ps1) 从你的 CPA 拉取全量目录、给文本模型补上 `apply_patch_tool_type: "freeform"`,模型保持**真实身份与真实上下文窗口**(不借用 GPT 档案)。

GPT 系模型走 CPA 原生路径,插件默认对它们零干预(OpenAI 兼容上游本身支持 custom 工具往返,只缺目录字段,由客户端目录一并补齐)。

## 安装

### 服务端(CPA ≥ 7.1.3,建议最新)

1. 从 [Releases](../../releases) 下载或按下文自行构建 `cpa-apply-patch-v<版本>.so`,放入:

   ```text
   <CPA 目录>/plugins/linux/amd64/cpa-apply-patch-v0.2.1.so
   ```

2. `config.yaml` 启用插件(支持热加载,通常无需重启):

   ```yaml
   plugins:
     enabled: true
     configs:
       cpa-apply-patch:
         enabled: true
         models: ["*"]            # 为哪些模型桥接;支持前缀通配 claude*、grok*
         tools: ["apply_patch"]   # 桥接哪些 freeform 工具;* = 全部 custom 工具
   ```

   完整示例见 [`configs/cpa-config.example.yaml`](configs/cpa-config.example.yaml)。

### 客户端(Codex CLI / 桌面端)

1. 生成本地模型目录(在装有 Codex 的机器上执行):

   ```powershell
   .\scripts\generate-model-catalog.ps1 -CpaBaseUrl "http://<你的CPA>:8317" -ApiKey "<CPA_API_KEY>"
   ```

2. `~/.codex/config.toml` 增加:

   ```toml
   model_catalog_json = "C:\\Users\\<你>\\.codex\\model_catalog.json"
   ```

3. 重启 Codex,选任意 Claude/Grok 等模型,让它改一个文件——rollout 里应出现 `custom_tool_call name=apply_patch`,桌面端出现差异卡片。

> 差异卡片的「撤销 / 审核」依赖工作区是 **git 仓库**;非 git 目录卡片不渲染,与本插件无关。

## 构建

插件是 cgo 动态库,Windows 上交叉编译到 linux/amd64 需要 [Zig](https://ziglang.org/) 作为 C 交叉编译器:

```powershell
.\scripts\build.ps1 -Version 0.2.1        # 产物在 dist/
```

Linux/macOS 直接 `make build VERSION=0.2.1`。

## 已知限制

- **补丁行级精度依赖模型**:工具描述里内置了格式硬规则(上下文行整行逐字节复制、含缩进),但这属于提示级约束。注意 apply_patch 执行器对缩进不符的补丁会**模糊匹配并按补丁字面缩进静默落盘**(不报错),Python/YAML 等缩进敏感文件需留意。
- **不支持 code mode**:`tool_mode: code_mode_only`(GPT-5.6 系的 JS 工具调用形态)在非 GPT 模型上会被客户端 harness 拒绝,请勿在本地目录里给非 GPT 模型设置该字段。
- CPA 升级若变更插件 ABI(当前 v1),需按新 SDK 重新编译。

## 目录结构

```text
main.go                              插件全部逻辑(单文件,cgo c-shared)
scripts/build.ps1                    Windows 交叉编译脚本(zig)
scripts/generate-model-catalog.ps1   客户端本地目录生成器
configs/cpa-config.example.yaml      服务端配置示例
dist/                                构建产物(不入库)
```

## License

[MIT](LICENSE)
