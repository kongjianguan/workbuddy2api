# 子系统


本目录描述真实子系统及其连接契约，是「有归属的参考页」索引，不是包摘要集合。

| Page | Owns |
|---|---|
| [账号池](pool.md) | 选号权重、两条退避线、冷却类型、粘性路由、账号处置判据 |
| [Responses 协议转换](responses-protocol.md) | Responses ↔ Chat 的转换规则、三类非标准工具映射、上游契约出处 |

## 一个子系统一页

当某个子系统拥有自己的术语、边界、生命周期、扩展点或失败模式时，为它建页。
页名保持稳定，加入上表，并从 `docs/architecture.md` 链接过来。

每页回答：

```markdown
# <Subsystem>

## Purpose

State the responsibility and the boundary.

## Ownership and dependencies

State who owns the boundary and which dependencies it may call.

## Core concepts

Define the types, states, and invariants that readers must share.

## API and extension points

Describe the supported entry points and the rules for extending them.

## Failure behavior

Describe errors, recovery, and observable diagnostics.

## Verification

List the focused tests, commands, or generated checks that establish the contract.
```

## 与源码等价的事实

类型签名、公开选项、生成的 API 区域等源自源码的事实，归属权归源码。
链接到生成的参考或从源声明重新生成，不要用散文手工维护第二份签名清单。

## 边界

明确写出依赖方向与归属。子系统页可以链接决策记录说明理由，但当前契约属于本页或它所描述的源码。

## 评审标准

页面要精确到可验证、简短到可扫读。若一页包含多个不相关边界或读者群，拆开。
增删或重命名子系统时同步更新架构图。
