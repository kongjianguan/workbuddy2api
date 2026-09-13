# 独立运行 Responses 转换代理

原作者网关只提供 `/v1/chat/completions`。Codex 只发 `/v1/responses`。
本仓库的 `cmd/responses-proxy` 把两者接上：只做协议转换，账号池仍由上游网关负责。

## 形态

```text
Codex  →  :7865  responses-proxy
              POST /v1/responses  →  转成 Chat  →  :7863 /v1/chat/completions
              其它路径（/v1/models、/healthz、chat）原样转发
```

转换逻辑就是 `internal/responses`（与曾经做进 fork 网关的那套相同），包括
`additional_tools`、namespace 嵌套 custom 工具。

## 启动

上游网关（原作者 `workbuddy2api`）先在 `:7863` 跑着。

```sh
go run ./cmd/responses-proxy -listen :7865 -upstream http://127.0.0.1:7863
```

Codex 的 `base_url` 指 `http://127.0.0.1:7865/v1`，`wire_api = "responses"`。
密钥仍用上游 `config.json` 的 `api_key`（代理原样转发 `Authorization`）。

验证：

```sh
curl -s http://127.0.0.1:7865/healthz
# {"ok":true,"service":"responses-proxy"}
```

空 Responses 请求应得到转换后的上游错误（鉴权失败或协议错误），而不是 404。

## systemd 用户服务（WSL 示例）

```ini
[Unit]
Description=Responses-to-Chat proxy in front of workbuddy2api
After=network.target

[Service]
WorkingDirectory=/home/misaka/workbuddy2api-fork
ExecStart=/home/misaka/workbuddy2api-fork/responses-proxy -listen :7865 -upstream http://127.0.0.1:7863
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
```

二进制先在本仓库 `go build -o responses-proxy ./cmd/responses-proxy`。
工作目录不必是原作者部署目录；代理无状态，不读 `auths/` 或 `config.json`。

## 不做的事

- 不选号、不刷新 token、不改 `config.json`
- 不实现模型别名（别名仍在上游网关）
- 不缓冲请求体以外的大文件；请求体上限 8 MiB，与网关默认一致
