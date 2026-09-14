# Agent Note: 修不成对的工具调用 id，避免上游 400 model_param_invalid

Status: implemented

## Problem

客户端报 `all accounts unavailable (cooling/disabled): upstream client (http 400)`，
上游返回 `{"code":11133,"extError":{"code":"model_param_invalid"}}`，
界面显示「请求参数不符合当前模型要求」。

触发条件是**会话历史里的工具调用 id 不成对**：某一轮工具调用失败后，客户端把
`assistant.tool_calls[].id` 与紧随其后 `role:"tool"` 的 `tool_call_id` 都留成了
空字符串。这类历史一旦进入后续请求，上游每次都拒收；网关把该账号打入冷却，
换号后同样被拒，最终报「所有账号不可用」。

表面上看像是账号被封或上下文超长，实际与二者都无关。

## Decision

在 `cmd/responses-proxy` 增加 `sanitizeToolCallIDs`，在请求出站前修复 id 配对：

- assistant 侧 id 为空 -> 就地生成 `call_autofix_<n>`
- tool 侧 `tool_call_id` 为空、或引用了一个从未被声明过的 id -> 按顺序绑到
  最近声明且尚未消费的 id 上
- 全部合法时原样返回，不做任何改写

`/v1/responses`（转换后的 Chat 体）与 `/v1/chat/completions` 两个入口都接。

## 上游约束是实测得出的

逐变体重放真实请求（每个变体前 `docker restart` 清账号冷却，避免 503 污染判定）：

| 变体 | 结果 |
|---|---|
| 两侧都是空串（原样） | 400 |
| 只补 assistant 的 id | 400 |
| 只补 tool 的 tool_call_id | 400 |
| 两侧都补但**不相等** | 400 |
| 两侧都补且**相等** | 200 |

结论：上游要求两侧**都存在且完全相等**。只补一侧、或补成不同值都会被拒。
因此修复必须保证「有且相等」，不能各补各的。

对照实验还排除了另外两类怀疑：同一请求里两条 `arguments` 内容损坏的历史消息
（176/178）在 id 配对修好后并不影响结果（变体 4 未改动它们仍 200）；
去掉 `dsh_plugin_packages` 等非标顶层字段也不能救活原请求。病灶只在 id 配对。

## Alternatives considered

**让客户端改历史。** 历史已经写进会话，用户只能弃用整个对话；而这类空 id 是
客户端工具调用失败时的固有残迹，随时可能再产生。

**继续靠账号轮转重试。** 换号不改变请求体，换个账号照样 400，只会把整个账号池
一起打进冷却。

**只在报错时提示用户新建会话。** 治标且不可靠——用户无法判断哪条历史有问题。

## Consequences

- 空/悬空 id 的历史会被透明修复，带这类历史的会话可以继续用
- 修复是顺序绑定的，只处理「空」与「悬空」两种确定坏掉的情况；不去猜测
  合法的但语义可疑的配对，避免误改正常历史
- 修复后请求体与客户端原始请求不再逐字节一致（转储里同时保留
  `raw_client_body` 与 `converted_chat_body`，便于对照）
- `arguments` 内容损坏（非法 JSON）的历史仍然原样发出。实测这类不触发 400，
  故不做处理，避免过度清洗

## Testing

`cmd/responses-proxy/sanitize_test.go`：

- `TestSanitizeToolCallIDsBindsBothSides` — 两组空 id，断言修复后声明与引用
  两侧都有值且逐组相等（直接编码上表结论）
- `TestSanitizeToolCallIDsRebindsDanglingReference` — 引用不存在 id 时重绑到真实 id
- `TestSanitizeToolCallIDsLeavesValidHistoryUntouched` — 合法历史原样返回
- `TestSanitizeToolCallIDsHandlesMissingMessages` — 无 `messages` 字段时安全返回

端到端：取含空 `tool_call_id` 的真实失败转储，经 `:7865` 回放，确认上游返回 200。
