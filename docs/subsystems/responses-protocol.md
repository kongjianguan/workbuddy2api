# Responses 协议转换

`internal/responses` 在网关内完成 OpenAI Responses API 与 Chat Completions 之间的双向转换，
使 Codex CLI 能直连网关。

## Purpose

Codex CLI 的唯一 wire protocol 是 Responses（其 `WireApi` 枚举移除了 chat 变体），
而上游 CodeBuddy 只接受 Chat Completions。本子系统承担两者之间的桥接。

边界：只做协议转换，不碰账号池、冷却、轮转。转换后的请求交给
[`executeChat`](../architecture.md#两条请求路径与共同的治理层)，账号治理全部复用。

## Ownership and dependencies

- 依赖 `internal/server` 提供的执行链路（通过 `chatExecHooks` 注入终端输出形态）
- 被 `internal/server/responses.go` 调用
- 不依赖 `internal/pool` / `internal/session` / `internal/scheduler`

## Core concepts

### 请求方向：item 序列与 message 序列不是一一对应的

这是转换的核心难点。Codex 实测发出的序列是：

```text
function_call(exec_command)   ← 工具调用
message(output_text)          ← assistant 正文旁白
function_call_output          ← 工具结果
```

逐项直译会产出 `assistant(tool_calls)` → `assistant(content)` → `tool`：两个 assistant 相邻，
且 tool 消息不紧随发起它的 assistant（Chat 协议要求），上游会拒绝。

`buildMessages` 因此用**待定缓冲**：`function_call` 攒入 `pendingToolCalls`，
遇到下一条 assistant 消息时合并（content 与 tool_calls 共存合法），或在 tool 结果前冲刷。

### 工具类型映射

Chat Completions 只有 `function` 一种工具类型，Codex 用四种：

| Codex 类型 | 转换 | 还原 |
|---|---|---|
| `function` | 直通 | 直通 |
| `custom`（`apply_patch`，freeform + lark 文法） | 包装为 `{"content": string}` 的 function，文法塞进 `description` | 取 `arguments.content` 还原为 `custom_tool_call.input` |
| `namespace`（如 `collaboration`） | 扁平化为 `{ns}__{name}`，超 64 字符哈希截断 | 拆回 `name` + `namespace` |
| `tool_search` | 转成同名 function | 还原为 `tool_search_call`，`arguments` 是**对象**且带 `execution` |

`ToolContext` 承载跨阶段状态：请求阶段登记工具身份，响应阶段据此还原。
丢了它回程无法判断该解包哪个工具——`customNames` 与 `chatNameToSpec` 必须贯穿两次转换。

### tool_search 是客户端工具

`execution: "client"` 表示 Codex 自己执行搜索。网关不执行搜索，只做三件事：

1. 把它转成模型可见的 function（原样透传 `type:"tool_search"` 会被上游**静默忽略**）
2. 从 `tool_search_output` 收集新发现的工具加入本轮 `tools`（否则结果白给）
3. 把搜索结果作为 `tool` 消息回喂

## API and extension points

```go
// 请求转换：Responses → Chat
func BuildChatRequest(req *Request) (*ChatRequest, *ToolContext, error)

// 非流式响应转换：Chat → Responses
func BuildResponse(chatResp map[string]any, toolCtx *ToolContext, model string) *Response

// 流式：Chat SSE → Responses SSE
func NewStreamTranslator(w http.ResponseWriter, toolCtx *ToolContext, model string) *StreamTranslator
func (t *StreamTranslator) Run(r io.Reader) error
```

扩展新工具类型时：在 `NewToolContext` 加分支、在 `ToolSpec` 记录还原所需信息、
在 `messageToOutputItems` 与 `StreamTranslator.closeTool` 各加回程分支。

## Failure behavior

| 情形 | 行为 |
|---|---|
| 上游流中断 | 发 `response.failed` 事件 |
| 无法解析的 item | 跳过而非报错（容忍客户端演进） |
| 空流 | 仍发完整的 `created` → `completed` 序列，`id` 必填 |
| `finish_reason=length` | `status` 置 `incomplete` |
| 工具参数非 JSON | 原样透传，让工具自身报格式错误 |

## Verification

```sh
go test ./internal/responses/
```

测试覆盖的关键不变量：

- `output_item.done` 与 `response.completed` 携带**完整文本**（Codex 不累加 delta）
- 工具参数在 `output_item.done` 里完整（Codex 只从这里取）
- `custom` 工具解包、`namespace` 还原、`tool_search` 三形态
- 事件顺序：`created` 最先、`completed` 最后
- `tool_calls` 的两种 Go 类型（`[]any` 与 `[]map[string]any`）都能处理

## 上游契约来源

转换规则不是猜的，出处如下：

| 规则 | 出处 |
|---|---|
| Codex 不累加 delta | `codex-rs/codex-api/src/sse/responses.rs` 的 unhandled 匹配臂 |
| `tool_search_call` 形态 | `codex-rs/protocol/src/models.rs` 的 `ToolSearchCall` 变体 |
| `OutputItem` 的 serde 标签 | 同文件的 `ResponseItem` 枚举（`tag = "type"`, `rename_all = "snake_case"`） |
| `apply_patch` 包装形状 | LiteLLM `responses/litellm_completion_transformation/custom_tools.py` |
| namespace 扁平化 | LiteLLM `transformation.py` 的 `qualify` 计算 |

`~/.codex/models.json` 的 `apply_patch_tool_type` 可设为 `null` 关闭 `custom` 工具路径，
届时 Codex 退回用 `exec_command` 调 `applypatch` 命令行改文件。
