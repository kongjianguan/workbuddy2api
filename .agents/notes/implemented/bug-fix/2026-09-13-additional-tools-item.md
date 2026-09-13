# Agent Note: 识别 additional_tools item 与 namespace 嵌套 custom 工具（新版 Codex 工具调用两连修）

Status: implemented

## Problem

用户经本网关 `/v1/responses` 使用 Codex（0.154）时，模型"宣布要调工具后就没下文"：正文说
"我先列出目录内容"，然后流正常结束，工具调用从未发生。

本地起透传代理抓真实请求定位，剥出**两层**问题：

1. **additional_tools item 不被识别。** 新版 Codex 不再把工具放顶层 `tools` 字段，而是作为
   `{"type":"additional_tools","role":"developer","tools":[...]}` item 塞进 input（实测为
   `functions`/`clock`/`collaboration` 三个 namespace，内含 custom 类型的自由form工具）。
   转换器的 input item switch 不认识该类型，按既定的"未知 item 跳过"策略静默丢弃 →
   上游收到的请求 `tools` 为空 → 模型无工具可用，只能文字宣告意图。
2. **namespace 嵌套的 custom 工具丢失参数 schema。** 第一层修复后真机复测：模型开始调用
   `functions__exec`，但参数是 `{}`，Codex 返回 aborted，模型无限重试。原因：两条 namespace
   展开路径（NewToolContext / addDiscoveredTool）把嵌套工具一律 `functionToolToChatTool`
   转换——嵌套 custom（freeform）的参数 schema 本就是空的，模型看到的是"无参函数"。
   顶层 custom 有 `{"content": string}` 包装（customToolToChatTool），嵌套的没有。

旧版 Codex 的工具在顶层 `tools` 且无这套 namespace 形态，不受影响——所以此前与 deepseek
的全链路差分测试（含 apply_patch、多轮工具循环）全绿，没能拦住客户端这次演进。

## Decision

两处修复，共用既有的 custom 包装与 namespace 还原机制，不引入新协议形状：

1. 把 Convert 里"从 input 收集工具"的通道（`collectToolSearchOutputTools`，随本次改名
   `collectInputDiscoveredTools`）从只认 `tool_search_output` 扩为同时认 `additional_tools`：
   两者 `tools` 数组形状一致，走同一条 `addDiscoveredTool` 路径。收集仍在 `buildMessages`
   之前，保证工具参与本轮消息转换的类型还原。
2. namespace 展开时按 `nested.Kind` 分派：嵌套 custom 走 `customToolToChatTool`（content
   参数 + 文法进 description），函数名用扁平化名；ToolSpec 记
   `{Kind: ToolCustom, Name, Namespace}` 并登记 `customNames`。响应侧三处（流式 output_item、
   completed 快照、非流式）的 namespace 拆名条件从 `Kind == ToolNamespace` 放宽为
   `Namespace != ""`，custom 分支对嵌套工具同样拆名 + 带 namespace 字段 +
   `unwrapCustomArguments`——即嵌套 custom 的回程与顶层 custom 同语义
   （`custom_tool_call{name, namespace, input}`）。

## Alternatives considered

**在 buildMessages 的 item switch 里加 case（第 1 层）。** 收集点晚于"工具集参与消息类型
还原"的既定时机，且与 tool_search 的收集路径分裂成两处。放弃。

**对未知 item 类型 fail fast（第 1 层）。** 会把"客户端演进出的新 item"变成硬故障，违背本包
"容忍客户端演进"的既定策略（未知 item 一律跳过而非报错）。放弃。

**只补 schema、保持 function_call 回程（第 2 层）。** 模型会传 `{"content":...}` 而 Codex
拿到的是 JSON 字符串而非 JS 源码，仍不可用。既然 Codex 把 exec 声明为 `type:"custom"`，
回程就该是 `custom_tool_call`。放弃。

## Consequences

- 新版 Codex 的工具集（含 namespace 嵌套 custom）经 `/v1/responses` 全量可达上游，回程形状
  与 Codex 的声明一致；tool_search 旧路径行为不变
- 本修复只保证"工具可达且形状正确"。模型是否会用 `functions.exec` 这类 JS 编排工具、参数
  写得好不好，属模型侧行为
- Codex 未来再换工具载体时此通道需继续跟进；回归测试
  `TestBuildChatRequest_AdditionalToolsItem`（收集 + content 参数）与
  `TestStreamTranslator_NamespaceCustomToolUnwrapped`（拆名 + 解包）守住两处

## Testing

- 抓包复现：真 `codex exec`（0.154，`model=gpt-6-astra` 经本地透传代理）复现"宣布后停止"；
  抓到的请求顶层 `tools` 为空、工具全部在 `additional_tools` item 里
- 第 1 层修复后真机复测暴露第 2 层：模型调用 `functions.exec` 参数恒为 `{}`，
  Codex 回 `aborted`，重试死循环（测试会话已及时终止）
- 回归测试断言面：additional_tools 收集、嵌套 custom 的 content 参数、
  流式回程拆名 + 解包；`go vet` 干净、`go test ./...` 全绿
- 部署后用同一 codex exec 复测：exec 工具应真实执行并给出最终答案
