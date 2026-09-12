# 架构

WorkBuddy2API 是一个自托管的 OpenAI 兼容反向代理网关：接收 OpenAI 形态的 HTTP 请求，
经账号池调度后转发到腾讯 CodeBuddy 上游。

## 组成

```text
客户端（Codex / Claude Code / SDK）
   │  POST /v1/chat/completions  或  POST /v1/responses
   ▼
internal/server           HTTP 入口：鉴权、请求体上限、提示词改写、轮转、日志
   ├── internal/responses  /v1/responses 协议转换（Responses ↔ Chat）
   ├── internal/prompt     出站前的系统提示词替换 / 降级
   ├── internal/pool       账号池：选号、冷却、熔断、在途租约
   ├── internal/session    会话粘性路由
   └── internal/upstream   上游 HTTP：请求体改写、SSE 收发、错误分类
   │
   ▼
copilot.tencent.com / www.codebuddy.cn
```

`internal/scheduler` 独立于请求路径运行定时任务（签到、活跃上报、猫猫旅行、token 保活）。

## 两条请求路径与共同的治理层

`/v1/chat/completions` 与 `/v1/responses` 是同一套账号治理的两个协议入口。
`internal/server` 的 `executeChat` 承载共同部分：提示词改写 → 选号 → 在途租约 →
token 刷新 → 上游调用 → 错误分类与账号处置 → 轮转 → 输出。

协议差异由 `chatExecHooks` 隔离：零值即标准 Chat 行为（SSE 透传 / 直接写聚合结果），
`/v1/responses` 注入钩子把上游 Chat 流翻译成 Responses 事件序列。
账号轮转、冷却、熔断、会话粘性、提示词改写、日志对两个入口完全相同。

## 出站改写管线

所有出站请求体在 `internal/upstream/payload.go` 的 `PrepareBodyOptWithEfforts` 中单 pass 改写：

- 强制 `stream:true`（上游拒绝非流式）
- `developer` 角色归一为 `system`（上游 role 白名单校验，否则 400 code=11-128）
- `tool_choice` 归一化（对象形式会 400 code=11101）
- 注入 DeepSeek 思维链开关，按模型支持档位降级 `reasoning_effort`
- 回填 assistant 消息的 `reasoning_content`（多轮一致性）
- 黑名单指纹脱敏（`internal/upstream/sanitize.go`）

该管线用 `map[string]any` 反序列化，**未知字段原样透传**——不做白名单。

## 账号处置状态机

`internal/upstream/Classify` 是错误分类的唯一权威，`internal/server.applyErrorPolicy`
据分类施加处置。分类优先级：余额耗尽 → session 失效 → 限流文案 → 状态码兜底。

两条独立的退避线：熔断器（`fails` 计数器，所有冷却入口与 5xx 共用）与软冷却指数退避
（`soft_streak`，独立计数）。何时清零、何时豁免见
[子系统：账号池](subsystems/pool.md)。

## 协议转换子系统

`internal/responses` 把 Responses 请求转成 Chat 请求、把 Chat 响应转回 Responses。
三类非标准工具的转换规则与 Codex 侧的事件消费面见
[子系统：Responses 协议转换](subsystems/responses-protocol.md)。

## 扩展点

| 要加的能力 | 落点 |
|---|---|
| 新的上游端点适配 | `internal/upstream/`，复用 `Classify` 与改写管线 |
| 新的入站协议 | `internal/server/` 加路由 + 复用 `executeChat`，像 `/v1/responses` 那样注入 hooks |
| 新的定时任务 | `internal/scheduler/`，并在 `internal/config/schedule.go` 加开关与时刻 |
| 账号处置策略 | `applyErrorPolicy` 的分类分支，配套改 `Classify` |

## 相关文档

- [子系统索引](subsystems/README.md)
- [开发入口](development.md)
- [决策记录](../.agents/notes/README.md)
