# Agent Note: 截断的 chat 流补写终止帧，避免客户端报协议异常

Status: implemented

## Problem

上游在思考中途假死时，代理看门狗会按 `-stream-idle-timeout` 掐断连接。但
`/v1/chat/completions` 直通分支掐断后**只是关连接**：客户端拿到的是一片既没有
`finish_reason`、也没有 `[DONE]` 的裸 SSE。

症状实测：DSH 作为客户端时，界面停住约 45 秒（等于看门狗阈值），最后报
`本轮运行失败 SSE stream ended without [DONE]`（`STREAM_CLOSED`）。原因是
DSH 的 `parseSse` 要求流以 `[DONE]` 结束，缺了就直接抛错：

> Yields `[DONE]` as the final value and returns; throws `LlmError('STREAM_CLOSED')`
> when the stream ends without it (truncated response — the model call cannot be trusted)

也就是说，真正的问题是**收尾语义缺失**，不是超时没生效——看门狗工作正常。用户
感知到的「卡住」是超时窗口，感知到的「报错」是缺终止帧。

`/v1/responses` 分支没有这个问题：`StreamTranslator.finish("failed")` 会发
`response.failed`。只有 chat 直通分支是裸关。

## Decision

把「一行 SSE 属于哪一类」从布尔判定升级成分类，并据此在异常结束处补写终止帧。

**1. `classifyFrame` 取代 `frameProgress` 的布尔语义**（`cmd/responses-proxy/watchdog.go`）

四类：`frameContent`（正文/思考/工具调用，真实推进）、`frameDone`（`[DONE]`）、
`frameFinish`（带 `finish_reason`）、`frameOther`（心跳/空行/空占位帧）。
`frameProgress` 保留为薄封装（`frameContent || frameDone`），首包闸门的语义不变。

`watchdogStream` 增加 `sawFinish` / `sawDone` 两个原子标记，由行扫描顺带记录。

**2. 异常结束时按三种情形补帧**（`cmd/responses-proxy/main.go` chat 分支）

`terminationFrame()` 决定补什么：

| 上游状态 | 补写内容 | 理由 |
|---|---|---|
| 已发 `[DONE]` | 不补 | 正常结束，重复补会破坏客户端解析 |
| 已发 `finish_reason`，缺 `[DONE]` | 只补 `data: [DONE]` | 保留上游声明的真实结束原因（如 `length`） |
| 两者都没有（真截断） | `finish_reason=timeout` + `[DONE]` | 让客户端得到具名错误而非协议异常 |

补写点在流拷贝循环的 `rerr != nil` 分支。此处已写过响应头，无法再改状态码，
补帧是唯一的收尾手段。

**`finish_reason` 取 `"timeout"` 而非 `"stop"`。** OpenAI 兼容客户端把 `stop`
读成「模型正常说完」。这里是看门狗掐断了截断的流，写 `stop` 等于把残缺回答
谎报成完整回答——属于伪造成功。非标值会被客户端映射成具名错误（DSH 的
`mapFinishReason` 把未知值变成 `{kind:error, code:<大写>}`），既诚实，又恰好
落在其默认可重试码表里，使上游能自愈而不是把错误抛给用户。

`frameFinish` **不算内容推进**：它不重置看门狗计时，否则上游「发一帧 finish
后挂死」就能绕开中途闸门。

**3. `ResponseHeaderTimeout` 收到 45s，与网关对齐**（`cmd/responses-proxy/main.go`）

两道看门狗都要等拿到响应头才开始计时：`client.Do` 尚未返回时没有 body 可读，
`-watchdog-timeout` 与 `-stream-idle-timeout` 都不在跑。上游连响应头都不给时
（排队静默 / 半开连接），唯一上限就是这个传输层超时。原值 120s，叠加
`-max-retries 1` 后客户端最坏要等 240s。

新值 45s 与网关的 `header_timeout_seconds` 一致。链路是代理 → manager → 网关 →
腾讯，网关自己就在 45s 上掐，代理设得更宽只会把「中间那跳本身卡住」的情形拖长。
实测正常首字节 1.5~5.7s，45s 余量充足。

该错误文案（`net/http: timeout awaiting response headers`）落在 `isStallError` 的
匹配表里，因此仍会静默换号重试——等头阶段不会变成新的硬失败点。

## Alternatives considered

**只在客户端配置里把 `STREAM_CLOSED` 加进可重试码（DSH 的 `llm-deepseek.retryPolicy`）。**
一行 YAML、立即生效，确实能让 DSH 不再把错误抛给用户。放弃为主方案的原因：它只
修一个客户端。裸关连接对任何 OpenAI 兼容客户端都是协议异常——同一份配置对 Codex
完全无效，Codex 走自己的 Responses 客户端，不会读 DSH 的 `retryPolicy`。代理侧
补帧则一次修好所有客户端。两者不冲突，客户端配置可作为额外一层。

**补 `finish_reason: "stop"`。** 兼容性最好，客户端不会报任何错。放弃原因：它把
截断伪装成完整回答，用户会拿到半截答复且无从察觉，违背诚实收尾。

**改上游网关的 `idleMonitoringBody` 让它按内容判活。** 与「WSL 用原作者版本」
的决定冲突，每次上游更新都要 rebase。放弃。

**把 `ResponseHeaderTimeout` 一并降到看门狗量级。** 曾考虑拆成独立改动；实际落地
为同一变更的第 3 项，因为它与补帧修的是同一条链路上的相邻缺口（中间断流 / 根本
不开始），且都需要「客户端不该无限等」这一条结论支撑。

## Consequences

- 截断的 chat 流以 `finish_reason=timeout` + `[DONE]` 收尾，客户端得到具名失败
- 上游已声明结束原因的流不被覆盖，只补缺失的哨兵
- 正常流不受影响：`sawDone` 为真时补写内容为空串
- `finish_reason` 不再重置看门狗计时，中途闸门无法被「一帧 finish 后挂死」绕开
- 仍是失败语义：客户端不会把截断读成成功回答
- 等头阶段的上限从 120s 降到 45s：最坏等待从 240s 收窄到 90s（45s × 2 次尝试），
  且该阶段仍走静默换号重试
- 上游若真的排队超过 45s 才回头，会被当作可重试失败换号——实测正常首字节
  1.5~5.7s，代价可接受；嫌紧可调 `upstreamHeaderTimeout`

## Testing

`cmd/responses-proxy/watchdog_test.go`：

- `TestChatTruncatedStreamGetsTerminalFrame` — 真截断必须补 `finish_reason=timeout`
  与 `[DONE]`，且不得出现 `finish_reason=stop`
- `TestChatUpstreamFinishWithoutDoneOnlyGetsSentinel` — 上游已发 `finish_reason=length`
  但缺哨兵时，只补 `[DONE]`，保留 `length`，且 `finish_reason` 恰好出现一次
- `TestChatNormalStreamNotDoubleTerminated` — 正常流 `[DONE]` 恰好一次，不补 timeout
- `TestClassifyFrameClassification` — 固定四类判定表，含「同帧带内容与
  finish_reason 时内容优先」

前两条已实证在修复前失败、修复后通过（去掉补帧代码后，两者都因缺 `[DONE]` 失败）。

`cmd/responses-proxy/main_test.go`：

- `TestUpstreamHeaderTimeoutIsWatchdogAligned` — 等头超时必须等于 `upstreamHeaderTimeout`
  且不宽于网关的 45s。已实证：把常量改回 120s 时该测试失败
- `TestHeaderTimeoutIsStallError` — 模拟上游永不回响应头，断言该错误被判为可静默重试
- `TestHeaderTimeoutTriggersSilentRetry` — 第一次不回响应头、第二次正常返回，
  断言重试发生且重试内容送达客户端

端到端在 WSL 上用部署后的二进制验证：

- 模拟「发部分内容后只发心跳」的上游，客户端收到的字节以
  `finish_reason":"timeout` + `data: [DONE]` 收尾
- 模拟「发 `finish_reason=length` 后挂死」的上游，只补出 `[DONE]`，
  `finish_reason` 保持 `length`、`timeout` 出现 0 次
- 模拟「永不回响应头」的上游，实测单次尝试 45s、两次尝试共 90s 后返回 502，
  日志可见 `timeout awaiting response headers, will retry`
