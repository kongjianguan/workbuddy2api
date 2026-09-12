# 账号池

`internal/pool` 管理多个 CodeBuddy 账号的可用性、选号与故障隔离。

## Purpose

在多个账号之间分配请求，并在单个账号故障时自动隔离、恢复、轮转，避免雪崩。

边界：只管账号状态，不发起 HTTP 请求（那是 `internal/upstream` 的职责），
也不知道请求内容（除模型名用于模型级冷却豁免）。

## Ownership and dependencies

- 被 `internal/server` 的轮转循环调用
- 状态持久化到 `state_file`（默认 `./data/state.json`），可选镜像到 Upstash Redis
- 不依赖 `internal/upstream`

## Core concepts

### 选号三因子

`weight = credits 比例 × 10 + idleWeight + successRate × 3`

- `credits 比例` = 该号积分 / 候选集最大积分
- `idleWeight` = `min(闲置小时 × idle_weight_per_hour, idle_weight_max)`，从未使用给满分
- `successRate` = `successCount/(successCount+errTotal)`，无记录给中性 1.5

流程：过滤（禁用 / 冷却 / 熔断 / 在途占满）→ 取 Top-5 候选 → 加权随机。
防惊群：跳过 100ms 内刚被选中的账号。全冷却时从非禁用、非余额耗尽的账号中选最早到期者顶班。

### 两条独立的退避线

容易混淆，必须分清：

| | 熔断器 | 软冷却指数退避 |
|---|---|---|
| 计数器 | `fails` | `soft_streak` |
| 触发源 | 所有冷却入口与 5xx | 仅软限流（429 / 限流文案） |
| 退避 | `breaker_cooldown × 2^retryCount`，封顶 `breaker_cooldown_max` | `soft_rate × 2^(连续次数-1)`，封顶 `soft_rate_max` |
| 清零 | 任意成功 | 成功或签到解冻 |
| 持久化 | `state.json` | `state.json` |

### 冷却类型

| 类型 | 触发 | 时长 |
|---|---|---|
| 硬冷却 | 余额耗尽（402 / 余额关键词） | 至次日 04:00 本地时区，签到恢复即解冻 |
| 软冷却 | 429 / 限流文案 | `soft_rate` 起指数退避 |
| 模型级冷却 | 429 + `code 6004` 且带重置时间 | 至上游重置墙钟，封顶 `soft_rate_max`；改用其他模型视为可用 |
| 短冷却 | 上游 404 | 固定 60s，不随 `soft_rate` 退避 |

### 会话粘性

同一会话尽量复用同一账号，多轮对话不跳号。会话键提取顺序：
`metadata.conversation_id` → `metadata.conversationId` → `metadata.user_id` →
顶层 `conversation_id` → 顶层 `conversationId`。TTL 滚动续期（默认 30m），
失败自动解绑，成功后绑定跟随最终成功账号。

## API and extension points

| 方法 | 职责 |
|---|---|
| `PickExcludingForModel(tried, model)` | 选号；带 model 时启用 6004 模型级冷却豁免 |
| `PickByUID(uid)` | 粘性号优先选号（已校验 health + 在途未满） |
| `Acquire` / `Release` | 在途租约（CAS 兜底并发抢名额） |
| `NoteSuccess` / `NoteError` | 成功清零计数 / 累计失败喂熔断 |
| `Cooldown` / `CooldownSoftForModel` / `CooldownUntilTomorrow4AM` | 各类冷却 |
| `Disable` / `ReviveDisabled` | 人工禁用与复活 |
| `ServableNow` | 健康探测口径，与 chat 可达性一致 |

## Failure behavior

| 分类 | 账号处置 |
|---|---|
| 余额不足 | 硬冷却至次日 04:00 |
| 频控（429 / 限流文案） | 软冷却，连续触发指数退避 |
| Session 失效（12153） | **连续 3 次**才永久禁用（一次多为网络抖动） |
| 上游 404 | 固定 60s 短冷却 |
| 服务端错误（≥500） | 喂 `fails`，达阈值熔断 |
| 请求体解析失败（11101） | 不罚账号，但仍轮转 |
| 内容拦截 | 不罚账号，`passthrough` 模式走降级重试 |
| 其余 4xx / 业务 code≠0 | 不处罚，换号重试 |

「不罚账号」的判据：问题在请求内容或客户端，不在账号健康。

## Verification

```sh
go test ./internal/pool/
go test ./internal/session/
```

`/status` 端点透出每账号的积分、冷却状态与截止时间、`fails`、在途数；
disabled 账号带 `disabled_reason`。
