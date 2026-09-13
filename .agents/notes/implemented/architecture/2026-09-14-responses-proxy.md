# Agent Note: Responses 转换器作为独立反向代理

Status: implemented

## Problem

WSL 上的账号池已切回原作者 `workbuddy2api`，该进程没有 `/v1/responses`。
Codex 只发 Responses。转换逻辑已经在本 fork 的 `internal/responses` 里，
且不依赖账号池——但之前只作为网关内部路由存在，切回原作者后 Codex 直连会 404。

## Decision

新增 `cmd/responses-proxy`：独立进程，默认 `:7865`，把 `POST /v1/responses`
转成 `POST /v1/chat/completions` 打到 `-upstream`，再把 Chat SSE / JSON 转回
Responses。其它路径（含 `/v1/models`、chat）用反向代理原样转发。
鉴权头原样传递，本进程不校验密钥、不读配置文件。

转换实现继续住在 `internal/responses`，代理只做 HTTP 接线。fork 网关内的
`/v1/responses` 路由保持不变，两处共用同一包。

## Alternatives considered

**继续改原作者网关、把转换编进去。** 与「换回原作者版本」的决定冲突，
每次上游更新都要 rebase。放弃。

**让 Codex 改用 chat wire。** Codex 已去掉 chat 变体，配置会直接报错。放弃。

**再引 LiteLLM。** 正是当初做原生转换要去掉的额外进程与内存。放弃。

## Consequences

- Codex 指 `:7865`，原作者网关仍占 `:7863`；多一跳本机 HTTP，无额外 Python 进程
- 模型别名、选号、冷却仍在上游网关；代理无状态，可随时重启
- 上游 4xx/5xx 原样回传，不包装成 Responses 错误（Codex 能显示上游 JSON）
- 流式路径在写出 SSE 之前已经 `WriteHeader(200)`（`StreamTranslator.Run` 既有行为）

## Testing

`go test ./cmd/responses-proxy/` 覆盖：流式转换发出 `response.created` /
`response.completed` 且上游路径为 `/v1/chat/completions`；非流式产出
`object=response`；上游 401 原样转发。
