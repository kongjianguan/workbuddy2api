# Agent Note: 首包探测的双读者竞态吞掉 tool_calls 的 id 帧

Status: implemented

## Problem

Codex / ZCode 在一轮里发起**多个工具调用**时，客户端稳定报：

```
Turn execution failed
reason=unknown retryable=false
Expected 'id' to be a string.
```

单个工具调用时几乎不复现，一轮多个调用时必现。报错来自客户端解析工具调用增量
（`tool_calls[]` 按 `index` 合并）时，发现某个 index 的 `id` 不是字符串——即该
index 的首帧（携带 `id` 的那帧）从未到达客户端。

## Decision

重写 `cmd/responses-proxy` 的流包装为**单读者模型**（`watchdogStream`）。

唯一的读取 goroutine 独占 `resp.Body`，把块投递到 channel 供消费者读取；
另有一个看门狗 goroutine 只在「连续 `-watchdog-timeout` 无新数据」时关闭上游
连接并投递 `ErrStreamStalled`。首包探测（`readUntilActualContent`）与中途空闲
检测共用这一个读者。

## Root cause

早期 `peekFirstContent` 的实现是双读者：

1. 起一个 goroutine 从 `resp.Body` 读第一段，投进 buffered channel；
2. 主流程收到含真实内容的首块后返回 `io.MultiReader(首块, resp.Body)`，让消费者
   继续读 `resp.Body`。

**探测 goroutine 在返回后并未停止**：它继续从 `resp.Body` 读，往那个已经没有人
消费的 channel 塞（缓冲仅 8 块），塞满后这些数据块就被永久丢弃。

于是首包之后的一段数据凭空消失。一轮里只有一个工具调用时，丢失的数据多半是
`arguments` 参数分片（客户端续接得上，不报错）；有多个工具调用时，**第二个
调用的 `id` 首帧正好落进被丢弃的区间**，客户端合并出的 tool_call 缺 `id`，
报 `Expected 'id' to be a string.`

## Alternatives considered

**保留双读者、只把探测 goroutine 用 context 停掉。** 治标：`Read` 阻塞在
`resp.Body` 上时无法被 context 立即打断，停止与「已读但未投递」之间仍有丢数据
窗口。只做单读者才能从结构上消除竞态。

**只做首包 timeout，不做中途空闲检测。** 上游在 1 秒时发空 `role:assistant`
占位包骗过首包检测、随后挂死 154s 的场景会漏网（本项目实测遭遇过）。单读者模型
天然同时覆盖两者。

**在代理里改客户端请求 / 补 id。** 曾在 `sanitize.go` 里写过补空 id 的方案，
经核对：该文件从未被接线（死代码），且真实根因是**我们丢数据**而非客户端发错，
补 id 只会掩盖症状。已删除该文件。

## Consequences

- 首包探测与中途空闲检测由同一读者承担，不存在第二个读者，无丢数据窗口
- stream 中途连续 `-watchdog-timeout`（默认 15s）无数据即掐断，不再挂满上游的 154s
- 首个真实内容之前就断流（`ErrEmptyStream`）与中途空闲（`ErrStreamStalled`）
  都归入可静默重试，客户端在无损状态下换号重试
- chat 直通分支同样改为单读者；此前它用 `newStreamIdleWatchdogReader` 另起一个
  reader 包在 `peekBody` 外，同样存在双读者问题

## Testing

`cmd/responses-proxy/watchdog_test.go`：

- `TestNoDataLossAcrossPeekWithMultipleToolCalls` — 构造带延迟的多段 `tool_calls`
  流（两个 index 各自的 id 帧分开发送），断言客户端收到**全部** id 帧与工具名。
  这是本次 bug 的回归测试：旧实现在第二个 id 帧处失败。
- `TestStreamIdleWatchdogAbortsStalledStream` — 先发真实内容再挂死，断言看门狗
  在超时后终止连接，而不是让客户端一直等下去。

端到端：对 WSL 上运行的实例回放 ZCode 的真实请求（481 条消息 / 55 个工具）4 轮，
每轮断言每个 `tool_calls` index 都收到 id 帧；并用「一轮并发调用 3 个工具」的
请求验证 3 个 id 帧全部到达。
