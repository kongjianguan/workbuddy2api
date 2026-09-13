# Agent Note: 识别 additional_tools input item（新版 Codex 的工具集载体）

Status: implemented

## Problem

用户经本网关 `/v1/responses` 使用 Codex（0.154）时，模型"宣布要调工具后就没下文"：正文说
"我先列出目录内容"，然后流正常结束，工具调用从未发生。

本地起透传代理抓真实请求定位：新版 Codex 不再把工具放顶层 `tools` 字段，而是作为
`{"type":"additional_tools","role":"developer","tools":[...]}` item 塞进 input（实测为
`functions`/`clock`/`collaboration` 三个 namespace，内含 custom 类型的自由form工具）。
转换器的 input item switch 不认识该类型，按既定的"未知 item 跳过"策略静默丢弃 →
上游收到的请求 `tools` 为空 → 模型无工具可用，只能文字宣告意图。

旧版 Codex 的工具在顶层 `tools`，不受影响——所以此前与 deepseek 的全链路差分测试
（含 apply_patch、多轮工具循环）全绿，没能拦住客户端这次演进。

## Decision

把 Convert 里"从 input 收集工具"的既有通道（`collectToolSearchOutputTools`，随本次
改名 `collectInputDiscoveredTools`）从只认 `tool_search_output` 扩为同时认
`additional_tools`：两者的 `tools` 数组形状一致，走同一条 `addDiscoveredTool` 路径
（function/custom/namespace 全支持、重名跳过）。收集仍发生在 `buildMessages` 之前，
保证工具参与本轮消息转换的类型还原。

## Alternatives considered

**在 buildMessages 的 item switch 里加 case。** 收集点晚于"工具集参与消息类型还原"的
既定时机（Convert 依赖工具集在 buildMessages 前就绪），且与 tool_search 的收集路径
分裂成两处、日后要维护两遍。放弃。

**对未知 item 类型 fail fast。** 会把"客户端演进出的新 item"变成硬故障，违背本包
"容忍客户端演进"的既定策略（未知 item 一律跳过而非报错）。工具收集通道保持宽容，
真正的失败模式（请求被上游拒）已在别处有明确报错路径。

## Consequences

- 新版 Codex 的工具集经 `/v1/responses` 全量可达上游；tool_search 旧路径行为不变
- 本修复只保证"工具可达"。模型对 namespace 扁平名（`functions__exec` 等）的调用意愿
  属模型侧行为；若模型仍不调用，是模型能力问题，不是链路问题
- Codex 未来再换工具载体时此通道需继续跟进；回归测试
  `TestBuildChatRequest_AdditionalToolsItem` 守住 additional_tools 形状

## Testing

- 抓包复现：真 `codex exec`（0.154，`model=gpt-6-astra` 经本地透传代理）复现
  "宣布后停止"；抓到的请求顶层 `tools` 为空、工具全部在 `additional_tools` item 里
- 修复后转换器探针：同一请求的转换结果包含 `functions__exec` / `clock__now`
- 回归测试 `TestBuildChatRequest_AdditionalToolsItem`；`go vet` 干净、`go test ./...` 全绿
- 部署后用同一 codex exec 复测：shell 工具应真实执行并给出最终答案
