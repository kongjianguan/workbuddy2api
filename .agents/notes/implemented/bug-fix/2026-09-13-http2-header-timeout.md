# Agent Note: 出站强制 HTTP/1.1，消除半死 HTTP/2 连接导致的 header timeout

Status: implemented

## Problem

部署到 WSL 后，聊天请求每隔几分钟出现
`http2: timeout awaiting response headers`。重启容器立刻恢复，过几分钟再复发。
429 限流是秒回错误；这次是传输层卡住，一直等到 `ResponseHeaderTimeout`（120s）。

证据：WSL 到 `copilot.tencent.com` 的 TCP/TLS 本身通（0.36s）；同一时段内成功请求
仍是 200；失败只发生在复用已有出站连接时。Go 默认 `ForceAttemptHTTP2=true`，
上游或中间路径把 HTTP/2 流挂死后，连接仍留在空闲池（`IdleConnTimeout=90s`），
后续请求继续撞同一条半死连接。

## Decision

出站 `http.Transport` 用空的 `TLSNextProto` 真正关掉 HTTP/2（`ForceAttemptHTTP2=false`
只在自定义 Dial 时生效，默认 TLS 仍会 ALPN 出 h2——线上已验证关掉该开关后日志仍报
`http2: timeout awaiting response headers`）。空闲连接超时从 90s 收到 30s，并补
`TLSHandshakeTimeout=10s`。聊天传输层失败时调用 `CloseIdleConnections()`，避免
下一次请求再捡到刚失败的连接。

关掉 HTTP/2 之后，故障变成 `write tcp ... connection timed out`，单次 TTFB 到
936s（Linux 对半开 TCP 的重传窗口）。根因换成：keep-alive 复用了已被对端/NAT
掐掉的连接，默认 Dialer 没有短 keepalive、也没有 10s Dial timeout。

因此再关掉连接复用（`DisableKeepAlives`），并给 Dialer 设 `Timeout=10s`、
`KeepAlive=15s`。每次聊天新建 TCP+TLS，半开连接无法再被下一请求捡到。

关掉复用之后，多数 glm 请求 TTFB 仍是 200–400ms，但偶发请求 TTFB 到 130s / 178s / 942s。
`ResponseHeaderTimeout` 要等请求体写完才开始计时，半开 TCP 卡在 write 时 120s 上限用不上。
给每条出站连接加 20s 写超时（`timedConn`），并把 `ResponseHeaderTimeout` 降到 20s，
失败后换号最多约 20s 而不是 2–15 分钟。

## Alternatives considered

**保留 HTTP/2，加 ping / ReadIdleTimeout。** Go 标准库 Transport 对 HTTP/2 没有
公开的 ping 旋钮；要健康探测得换 `http2.Transport` 或第三方，改动面大，且当前
故障已经能用 HTTP/1.1 消掉。放弃。

**只 CloseIdleConnections、仍走 HTTP/2。** 失败那一次仍要等满 120s 才清池，用户
感知的卡顿还在。强制 HTTP/1.1 让单次失败也只卡那一条连接。放弃单独清池。

**把 HeaderTimeout 降到几秒。** 只能让失败更快暴露，治不了半死连接被复用。
上游首包偶尔本来就慢（成功请求 TTFB 曾到 16s），过短会误杀。放弃。

## Consequences

- 出站聊天与短 RPC 都走 HTTP/1.1；连接建立略多，延迟与稳定性换的是不再被
  一条坏 h2 流拖死
- 传输层失败会清掉空闲池，瞬时并发可能多一次握手
- 若上游日后强制 HTTP/2-only，本设置会让 TLS 协商失败，那时再开 h2 并加探测

## Testing

- `TestNewChatClientNoTotalTimeoutAndSharedTransport` 断言空 `TLSNextProto`、
  `DisableKeepAlives`、`ResponseHeaderTimeout=20s`、`IdleConnTimeout=30s`
- `go vet ./...` 与 `go test ./internal/upstream/` 覆盖连接池共享与超时语义
