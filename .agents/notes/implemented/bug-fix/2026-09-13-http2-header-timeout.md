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

HTTP/1.1 每请求一条连接（keep-alive 仍复用，但没有 h2 多路复用流），半死连接
不会拖累后续请求。这是针对当前故障形态的最小修复，不引入连接健康探测。

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

- `TestNewChatClientNoTotalTimeoutAndSharedTransport` 断言 `ForceAttemptHTTP2=false`
  且 `IdleConnTimeout=30s`
- `go vet ./...` 与 `go test ./internal/upstream/` 覆盖连接池共享与超时语义
