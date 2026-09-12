# AGENTS.md

WorkBuddy2API：把腾讯 CodeBuddy 账号包装成 OpenAI 兼容 API 的自托管网关。

## 会话开始

1. 读 `memory/` 下的项目笔记（若存在）
2. 读 [docs/architecture.md](docs/architecture.md) 建立整体认识
3. 动手改代码前，读 [docs/development.md](docs/development.md) 的常见陷阱一节

## 硬性约束

**凭证绝不进版本库。** `auths/` 存放明文 `accessToken` / `refreshToken`。
`.gitignore` 已排除 `auths/`、`data/`、`config.json`、`*.key`、`*.pem`、`*.env`。
改动 `.gitignore` 后必须验证这些规则仍然生效：

```sh
git check-ignore -v auths/x.json config.json data/state.json
```

**文档白名单是本 fork 特有。** 上游把 `docs/` 与全部 `*.md` 排除；
本 fork 用 `!.agents/**` / `!docs/**` 放行项目文档库。
增删白名单前先确认没有放行敏感文件。

**改上游共享文件时保持小改动面。** 本 fork 定期 `git sync-upstream`（rebase 到上游）。
新增能力优先放新包 / 新文件，对上游已有文件只做最小接线。

## 改动纪律

不保留向后兼容：过时的直接删，不加兼容层、不写 migration、不留 fallback。
选满足当前需求的最简单实现，不做预防性抽象。

## 文档归属

一个事实只有一个归属地。放置规则见 [docs/AGENTS.md](docs/AGENTS.md)：

| 事实 | 归属 |
|---|---|
| 当前架构与扩展点 | [docs/architecture.md](docs/architecture.md) |
| 子系统契约 | [docs/subsystems/](docs/subsystems/README.md) |
| 操作步骤 | [docs/cookbook/](docs/cookbook/) |
| 决策理由与取舍 | [.agents/notes/](.agents/notes/README.md) |
| 事故证据与护栏 | [docs/postmortem/](docs/postmortem/README.md) |

非平凡改动必须在同一次变更里补 Agent Note。改动行为、架构、跨文件契约、
配置格式、测试策略，都算非平凡。

## 验证

```sh
go test ./...      # 完整测试套件
go vet ./...
```

文档库检查：

```sh
python3 ~/.agents/skills/project-doc-library/scripts/verify_project_docs.py --root .
python3 ~/.agents/skills/project-doc-library/scripts/lint_prose.py --root .
```

## Git

- 上游：`upstream` remote（只读，推送地址已封死）
- 本 fork：`origin` remote，唯一推送目标
- 同步上游：`git sync-upstream`（fetch + rebase），随后 `git push --force-with-lease origin master`
