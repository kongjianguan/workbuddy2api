# 开发

## 前置

- Go ≥ 1.22（源码构建）
- Docker + Docker Compose（推荐部署方式）
- 一个或多个已注册的 CodeBuddy 账号

## 命令

```sh
# 构建与检查
go build ./...
go vet ./...
go test ./...                    # 完整测试套件

# 单包测试
go test ./internal/responses/    # 协议转换
go test ./internal/pool/         # 账号池

# 本地运行
go run ./cmd/server -config config.json
```

Go 模块代理必须走国内镜像，否则 `go mod download` 直连 `proxy.golang.org` 会超时：

```sh
GOPROXY=https://goproxy.cn,direct go build ./...
```

`Dockerfile` 已内置 `ENV GOPROXY=https://goproxy.cn,direct`，容器构建无需额外设置。

## 部署

```sh
cp config.example.json config.json   # 至少设置 api_key
./login.sh                            # OAuth 登录，凭证落到 auths/
docker compose up -d --build
curl -s http://localhost:7863/healthz
```

## 常见陷阱

**`docker compose` 挂载 `./config.json` 时文件必须已存在。**
文件不存在时 Docker 会把它当目录创建，容器随后读配置失败。先 `cp config.example.json config.json`。

**所有定时任务默认开启。** 四类任务（签到 / 活跃上报 / 猫猫旅行 / token 保活）
各自独立排程，`*_hours: []` 或 `null` 表示**回落默认值而非禁用**，
真正关闭要用 `*_enabled: false`。

**请求体上限是网关侧判断。** 超过 `server.max_body_mb` 直接返回 413，不打上游、不罚账号。

**模型名可能需要在 `model_aliases` 中映射。** 客户端用自带 slug（Codex 是 `deepseek-flash`），
上游只认自己的模型名（`deepseek-v4.1-flash`）。未命中映射的名字原样透传。

## 文档库约定

本仓库用 `docs/`（人读参考）与 `.agents/notes/`（决策记录）承载项目知识。
放置规则与写作规范由 [文档标准](AGENTS.md) 与
[Agent Notes](../.agents/notes/README.md) 定义，动手前先读。

校验：

```sh
python3 ~/.agents/skills/project-doc-library/scripts/verify_project_docs.py --root .
python3 ~/.agents/skills/project-doc-library/scripts/lint_prose.py --root .
```

非平凡改动必须在同一次变更里补一条 Agent Note。
