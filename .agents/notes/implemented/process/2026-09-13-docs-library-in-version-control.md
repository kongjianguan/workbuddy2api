# Agent Note: 文档库随仓库版本化，偏离上游的 docs/ 排除纪律

Status: implemented

## Problem

上游 `.gitignore` 排除 `docs/` 与除 README 外的全部 `*.md`，README 明确写了这条纪律：
「工作文档不进版本库」。这适用于上游的协作场景——作者不希望在仓库里堆积设计草稿。

本 fork 需要项目文档库（架构参考、子系统契约、决策记录）随仓库版本化：
Codex desktop 等工具在项目目录内工作时需要直接读到它们，
且跨会话、跨机器的接力依赖版本控制而非本地文件。

两者直接冲突：文档库落在被忽略的路径上会被 `git clean -xdf` 抹掉，且新克隆的仓库里没有。

## Decision

在 `.gitignore` 末尾追加白名单，放行文档库路径：

```gitignore
!.agents/
!.agents/**
!docs/
!docs/**
!AGENTS.md
!CLAUDE.md
```

放宽的**只是版本控制**。`auths/`、`data/`、`config.json`、`*.key`、`*.pem`、`*.env`
仍受原有规则约束，不受白名单影响。改动后已逐项验证：

```sh
git check-ignore -v auths/x.json config.json data/state.json   # 仍被忽略
git check-ignore -v docs/AGENTS.md .agents/notes/README.md    # 已放行
```

## Alternatives considered

**保持被忽略，文档库仅本地存在。** 完全遵守上游纪律、零 `.gitignore` 改动。
放弃原因：文档不会被提交，`git clean -xdf` 直接抹掉，
且新克隆的仓库里没有文档——接力场景下这等于文档不存在。

**文档库放仓库外（如 `~/workspace/agentspace/memory/`）。** 不碰上游任何规则。
放弃原因：工具在项目目录内工作时看不到，接力需要额外传递路径；
且文档与代码版本脱钩，代码演进后文档容易失同步。

**向第 33-35 行插入白名单而非追加到末尾。** 放弃原因：末尾追加让改动集中在文件尾，
`sync-upstream` 冲突时更容易识别与解决；上游在这几行有历史改动（`docs/` 排除是后加的）。

## Consequences

- 文档库进入版本控制，可回滚、可跨机器、工具直接可读
- 偏离上游纪律，需在 `sync-upstream` 时留意 `.gitignore` 冲突
- 本 fork 对 `.gitignore` 的改动是唯一一处，冲突面小，且改动集中在文件末尾
- 新增敏感文件类型时必须检查白名单是否意外放行：白名单只针对文档路径，
  但 `!.agents/**` 这类通配需要留意未来在该目录下放入敏感内容的可能

## Testing

```sh
# 白名单未放行敏感文件（期望：全部仍被忽略）
for p in auths/workbuddy-x.json data/state.json config.json backup.key secret.pem .env; do
  git check-ignore -q "$p" && echo "OK ignored: $p" || echo "LEAK: $p"
done

# 文档库路径可入库（期望：全部可入库）
for p in docs/AGENTS.md .agents/notes/README.md AGENTS.md; do
  git check-ignore -q "$p" || echo "OK tracked: $p"
done
```
