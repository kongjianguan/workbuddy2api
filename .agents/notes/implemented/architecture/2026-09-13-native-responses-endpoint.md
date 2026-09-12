# Agent Note: 网关原生实现 /v1/responses，去除 LiteLLM 中间层

Status: implemented

## Problem

Codex CLI 的唯一 wire protocol 是 Responses API：其 `WireApi` 枚举已移除 chat 变体，
配置 `wire_api = "chat"` 会直接报错。而上游 CodeBuddy 只接受 Chat Completions，
直连时 `POST /v1/responses` 返回 404。

此前用外部 LiteLLM 进程做协议桥接（`openai/chat_completions/` 模型前缀触发
`use_chat_completions_api` 路径）。该方案可用但代价明显：多一个 Python 进程（常驻 178 MB）、
多一跳网络、多一个故障点，且依赖外部项目的更新节奏。

## Decision

在网关内实现协议转换，`internal/responses` 承担全部转换逻辑，`internal/server` 只加一行路由。

原 LiteLLM 方案的机制（`_OPENAI_CHAT_COMPLETIONS_RESPONSES_MODEL_PREFIX`）证明了这个转换方向
可行且语义明确，实现时以其为行为参照并做差分测试对齐。

转换后的请求交给既有的 `executeChat`，账号轮转、冷却熔断、会话粘性、提示词改写、
thinking 注入、指纹脱敏全部复用——两个协议入口共用同一套账号治理。

## Alternatives considered

**继续用 LiteLLM。** 最省事，已被验证可用，且它是成熟的 58k★ 项目，兼容性修复无需自己维护。
放弃原因：额外进程与常驻内存、多一跳、多一个故障点；且 LiteLLM 的 6246 行转换层里
绝大部分（100+ provider 目录、成本追踪、session 存储、缓存）对此场景无用。

**只做 `function` 工具，要求用户关掉 `apply_patch`。** 工作量约 900 行（对比完整实现 2600 行），
且 `apply_patch_tool_type` 设为 `null` 后 Codex 会退回用 `exec_command` 调命令行改文件，能力不丢。
放弃原因：把复杂度转嫁给客户端配置，反而更脆；且 `apply_patch` 是 Codex 最核心的编码能力。
同类项目 `Kurok1/openai-responses-adapter`（Go，801 行）正是只做 `function` 的形态，
遇到 `apply_patch` 即失效——这是"能跑"与"可用"的差距。

**用 cc-switch 或 deepseek-recipe。** cc-switch 是 Rust 桌面应用（转换层约 900 KB），
支持 6 客户端 × 多 provider，规模远超所需且不可拆分。deepseek-recipe 做的是反方向
（协议 → prompt → 推理后端），无法接在网关前面。

## Consequences

- 常驻内存、进程数、故障点均减少；协议转换在 Go 侧同进程完成，固定开销可忽略
- 转换逻辑成为自有代码，需自行维护；上游 Codex 协议演进时要跟进
- 差分测试以 LiteLLM 为参照实现，它保留在磁盘上供对照（`~/.local/share/litellm-bridge/`）
- 三个实测踩出的坑已固化为回归测试：`output_item.done` 必须携带完整文本
  （Codex 不累加 delta）、`tool_calls` 的两种 Go 类型都要处理、跨 item 的 tool_calls 合并
- 遗留限制：工具结果里的媒体块（图片）未特殊处理，会序列化成 JSON 文本

## Testing

`go test ./internal/responses/` 覆盖转换不变量。端到端验证用真实 Codex 执行：
纯对话、读文件、`apply_patch` 改代码、多轮工具循环、namespace 工具调用、`tool_search` 闭环。

差分测试对比 LiteLLM 输出的语义等价性，见 `docs/subsystems/responses-protocol.md`。
