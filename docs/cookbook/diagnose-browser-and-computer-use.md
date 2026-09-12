# 排查 Codex 的 in-app browser 与 computer use

**状态：未解决。** 本文记录已排除的方向与已验证的事实，供接力者继续。

## 症状

Codex Desktop 接入第三方模型（本网关）后，in-app browser 与 computer use 不可用。

## 已排除：不是上游 Responses 端点支持不良

这是最初的假设，已用实测排除。工具**能列出、能调用、runtime 正常响应**，
转换层没有让工具不可见或不可调用。

实测方式：

```sh
codex exec --skip-git-repo-check \
  -c 'model_providers.custom.base_url="http://127.0.0.1:4100/v1"' \
  'Use mcp__cua_repl__js to evaluate: cua.getState(); Report the raw output.'
```

把 `4100` 换成一个转发到本网关的抓包代理，即可同时观察请求与响应。

结果：

```text
apps=[...39 个应用...]     ← computer use 能列出本机应用，底层是通的
browsers=[]                ← 浏览器后端为空
```

尝试创建 in-app browser 标签页：

```text
Browser is not available: iab
```

对已有应用调用 `getApp()`：

```text
Computer Use was not approved to use Google Chrome
Computer Use was not approved to use com.apple.Notes
```

## 已验证的两个机制

### 1. in-app browser 后端由宿主注册

浏览器的三种 backend（`iab` / `extension` / `cdp`）由宿主进程注册，
定义见 `~/.codex/plugins/cache/openai-bundled/browser/<version>/scripts/browser-service.mjs`
里的 `Browsers.list()` 返回类型：

```typescript
type: "iab" | "extension" | "cdp"
```

`iab`（in-app browser）需要桌面应用的浏览器面板存在。
CLI 模式下没有这个面板，`browsers` 为空是预期结果，不是故障。

排除方向：先确认 Desktop 的浏览器面板是否已注册后端，再怀疑协议层。

### 2. app 授权按 session 存储

授权文件在 `~/.codex/computer-use/sessions/<session-id>.toml`：

```toml
[apps]
allowed = ["com.google.Chrome", "com.apple.finder", "com.tencent.xinWeChat"]
```

**每个 session 一份白名单。** 新会话不在列表里时白名单为空，
所有 app 调用都被拒。这可能是「功能失效」观感的来源之一——
同一功能在不同会话下表现不同。

验证方式：列出已有授权与当前 session id，比对是否命中。

```sh
ls ~/.codex/computer-use/sessions/
cat ~/.codex/computer-use/sessions/<当前session>.toml
```

## 未验证：Desktop 是否对第三方 provider 差异化处理

这是原假设的剩余部分，**没有证据**。要证伪需要一次官方 provider 的对照实验：

```sh
codex exec --skip-git-repo-check \
  --model gpt-5.2-codex \
  -c 'model_provider="openai"' \
  'Use mcp__cua_repl__js to evaluate: cua.createBrowserTab("iab", "https://example.com", {visible: true}); Report the raw output or error.'
```

本机执行此命令时 `api.openai.com` 直连超时（需要代理），实验未完成。
**这是接力者最应该先做的一步**：同一台机器、同一 Desktop 环境，只换 provider，
对比 `browsers` 数组与 `createBrowserTab` 的结果。

## 排查顺序

1. 确认 Desktop 主进程在运行，`launch.mjs` 与 `node_repl` 进程存在
2. 在**已授权的 session** 里重试，排除白名单因素
3. 检查 `~/.codex/config.toml` 的 `[mcp_servers.node_repl.env]`：
   `BROWSER_USE_AVAILABLE_BACKENDS` 应包含 `chrome,iab`
4. 挂代理跑官方 provider 对照实验，判定是否与 provider 有关
5. 若确与 provider 有关，抓 Desktop → app-server 的本地 JSON-RPC
   （浏览器与 computer use 走 app-server 协议，不走模型 API）

## 环境快照

```text
Codex CLI            0.154.0
Codex Desktop        26.903.61454
BROWSER_USE_AVAILABLE_BACKENDS   chrome,iab
SKY_CUA_SERVICE_PATH             ~/.codex/computer-use/Codex Computer Use.app
```

MCP 服务器（`computer-use` / `cua_repl` / `node_repl`）由 Desktop 启动时**动态注入**，
配置文件里的 `enabled = false` 不影响它们。
