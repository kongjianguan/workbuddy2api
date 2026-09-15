# Agent Note: 心跳帧让看门狗失效，客户端挂到上游超时（实测 5 分钟）

Status: implemented

## Problem

客户端在工具调用中途长时间无响应：实测有三个请求卡了五分钟以上，只能重启网关
才能恢复。代理侧没有任何看门狗日志，看起来像看门狗没生效。

网关日志里大量 `tok=0` 的完成行，容易被读成「上游返回了空响应」。

## Decision

两层一起改：

1. **代理看门狗从「收到任意字节」改为「收到真实内容」**（`cmd/responses-proxy/watchdog.go`）。
   新增 `frameProgress` 按 SSE 行长解析，只有正文 / 思考 / 工具调用 / `[DONE]`
   才算推进；心跳（`: ` 开头的注释行）、空行、只带 `role` 或 `finish_reason`
   的空帧一律不算。并新增第二道闸门 `-stream-idle-timeout`（默认 45s），限制
   「已出内容之后」的连续无内容时长，首包仍由 `-watchdog-timeout`（15s）管。

2. **网关 `idle_timeout_seconds` 从 300 降到 90**（WSL `config.json`）。

## Root cause

上游在排队 / 思考期间会**持续发送 `: heartbeat` 注释帧**。而两处的存活判据都是
「读到底层数据就刷新计时」：

- 代理旧看门狗：`lastRead` 在任何 `Read` 返回数据时刷新
- 网关 `internal/upstream/idle.go:26` 的 `idleMonitoringBody.Read`：同样在
  `n > 0` 时刷新 `lastRead`，阈值取 `idle_timeout_seconds`

于是心跳把两边的计时器都无限续命，看门狗永不触发。请求一直挂到上游自己断开，
而网关阈值是 300 秒——正好对应实测的五分钟。

单靠代理修不彻底：代理掐断后，网关到上游的那条连接仍在（`internal/upstream/client.go`
用 `context.WithCancel(context.Background())`，下游断开不取消上游），会继续占着
该账号的 `max_in_flight` 名额约 2.5 分钟。2 账号 × 3 名额的池子里几个这样的孤儿
连接叠加，就会报 `all accounts unavailable`，与「账号被封」难以区分。

## 同时修掉的日志缺陷

`internal/server/handler.go` 流式分支写的是 `st.toks, _ = stats.Tokens()`，
丢掉了表示 usage 是否存在的布尔值。计数器零值是 0，于是**任何在上游 usage 帧之前
结束的流都会被打印成 `tok=0`**，与「真返回 0 token」无法区分。`logging.go` 的
注释与既有测试都表明缺失应显示 `-`。改为仅在 `ok` 时赋值。

## Alternatives considered

**只把代理的存活判据改严。** 不够：代理掐断后网关到上游的连接仍在，会继续占用
账号名额。必须同时把网关的 `idle_timeout_seconds` 从 300 秒降下来，孤儿连接的
占用窗口才真正收窄。

**只在网关侧按内容判活，不动代理。** 与「WSL 用原作者版本」的决定冲突，每次上游
更新都要 rebase；且 fork 的 `handler.go` 只做最小接线，不适合承载这类改动。

**给客户端加超时。** 治标：客户端超时后重建请求，孤儿连接依旧挂在网关上吃名额，
`all accounts unavailable` 仍会发生。

## Consequences

- 心跳不再续命：只有真实内容推进才重置计时，15s / 45s 两道闸门才真正有效
- `tok=0` 不再被误报；缺失显示 `-`，两者可区分
- 网关 `idle_timeout_seconds=90` 缩短孤儿连接占用名额的时间
- 大模型正常「长思考」若真的 45 秒不出任何 token 会被掐断并换号重试。实测正常
  响应的 TTFB 在 1.5~5.7 秒，45 秒留了足够余量；如需放宽用 `-stream-idle-timeout` 调

## Testing

`cmd/responses-proxy/watchdog_test.go`：

- `TestHeartbeatDoesNotKeepStalledStreamAlive` — 上游只发心跳与空 role 帧，断言
  看门狗仍按首包阈值掐断，且期间心跳次数不超过阈值窗口（旧实现会一直跑到测试超时）
- `TestIdleWatchdogAbortsAfterContentThenSilence` — 先出内容再只发心跳，断言中途
  闸门生效，且已到达的内容已透传
- `TestClassifyFrameClassification` — 固定「什么算内容推进」的判定表（该判定后来
  从 `frameProgress` 布尔升级为 `classifyFrame` 四分类，见
  [截断的 chat 流补写终止帧](2026-09-15-truncated-chat-stream-terminal-frame.md)）
- `TestNoDataLossWithHeartbeatsInterleaved` — 心跳与内容交错时不得丢内容
  （行扫描残留处理不当会吃掉相邻行）

`internal/server/logging_test.go`：

- `TestStreamMissingUsageLogsDashNotZero` — 无 usage 帧的流必须记 `tok=-`。
  已验证该测试在修复前失败、修复后通过。
