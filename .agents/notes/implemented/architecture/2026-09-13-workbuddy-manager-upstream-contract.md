# Agent Note: workbuddy-manager 上游契约

Status: implemented

## Problem

账号池只有命令行：加号要跑脚本、看状态要 `curl /status`、发密钥没有界面。
[ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) 是针对这类需求写的
Web 控制台（FastAPI + Next.js，独立部署在 `:7864`，自带 OpenAI 兼容反代网关），但它面向
上游 `Sliverkiss/workbuddy2api`，安装脚本默认克隆的也是该仓库，与同名 fork 不是同一个仓库。

需要判断：本 fork 能否直接充当它的上游、要付出什么代价、以及为了让这件事长期成立，
本 fork 有哪些东西不能再随便改。

## Decision

本 fork 不改代码即作为管理端上游，集成只发生在部署层：安装时用 `UPSTREAM_REPO` 指定本 fork，
运行时用 `WB_UPSTREAM_REPO` / `WB_UPSTREAM_API_REPO` 让版本检测也对着本 fork。

依据是管理端对本网关的依赖全部落在已有接口与文件上，且管理端自身声明「不改动
workbuddy2api 一行代码」——双方以 HTTP 接口和磁盘文件为边界，没有进程内耦合，
因此不需要适配层。实测方式与全部依赖项见
[cookbook：接入 workbuddy-manager](../../../../docs/cookbook/integrate-workbuddy-manager.md)。

### 由此产生的负向保证

管理端不感知版本，下列面一旦改变就会**静默失效**（表现为账号页空白、任务页无记录或容器重载失败），
改动前必须先改管理端或接受集成中断：

- `auths/workbuddy-<uid>.json` 的文件名形态与嵌套 `account` / `auth` 结构
- `GET /status` 的 `accounts[]` 字段名（`uid`、`credits`、`cooling`、`disabled`、
  `success_count`、`in_flight`、`breaker_fails`、`last_success`）
- 调度日志的 `<kind> <uid8>: ...` 形态（`travel` / `activity` / `checkin` / `keepalive` / `user-resource`）
- `docker-compose.yml` 的 `container_name: workbuddy2api`，以及 `auths/` 保持 bind mount
- `config.json` 中管理端可视化写入的段与键名

其中 `config.json` 的未知键被忽略、不会导致启动失败，因此该段是**加法友好**的：
管理端新增键不需要本网关同步跟进。

## Alternatives considered

**在本 fork 内实现热重载（SIGHUP）以减少重启。** 管理端的重载路径固定为 `docker restart`，
不会发 SIGHUP；加了没有调用方，属无收益的新面。

**把管理端 vendored 进本仓库。** 会把 Python/Next 前后端与本 Go 网关耦合成同一仓库，
而本 fork 的既定节奏是 rebase 跟随上游、只对上游文件做最小接线；管理端又有自己的一键安装器
与 Release 流程。绑进来只增加双方的合并成本，不产生新的集成能力。

**给管理端提 PR 让它原生识别本 fork。** 控制权在对方仓库，能否被接受不可控；
而两行环境变量已足够解决同一问题。

**什么都不做（口头说明即可）。** 该契约是跨仓库的隐式约定，且已经有真实演进在破坏它：
上游已把 `/app/data` 从 bind mount 改成 named volume，若本 fork 照抄，管理端安装脚本
`chown auths data` 与「读宿主 auths 目录」的假设就会失效。把负向保证写进文档与决策记录，
是让后来者能看见它的最低成本。

## Consequences

- 用户得到完整控制台（扫码纳管、签到、密钥分发、调用日志、用量统计），本 fork 零代码改动、无适配层
- 契约靠约定维持：未来若改账号文件名、`/status` 字段、调度日志形态、容器名或 `auths` 挂载方式，
  集成会静默中断，须同步修管理端（负向保证清单即检查点）
- 两处能力不等价，需在文档中讲清：管理端的「模型映射」作用于其 `:7864` 入口，与本网关
  `config.json` 的 `model_aliases` 是两套映射；本 fork 独有的 `/v1/responses` 不在管理端网关的转发面内，
  Codex 类客户端要直连 `:7863`
- 上游目录的代码更新由管理端执行 `git reset --hard origin/<分支>`，`config.json`、`auths/`、`data/`
  因在 `.gitignore` 中而安全；未提交改动会被丢弃

## Testing

用管理端自身的 `server/services/wb2api.py`（原样拷出、指向本网关运行）验证契约，账号为合成凭证：

- `get_status()` 连通，`list_auth_accounts()` 读到 `workbuddy-*.json`，`merge_pool_status()` 把
  `credits` / `cooling` / `disabled` / `in_flight` / `breaker_fails` / `last_success` 合并进账号
- `get_models()` 返回模型表；`load_upstream_config()` 读到配置且 `api_key` 已掩码
- `save_upstream_config()` 写入 `pool` / `schedule` / `session_sticky` / `prompt` 后，
  本网关能用该 `config.json` 正常启动，并打印 `每号 7 条` 等新值；越界与格式非法的值被管理端拒绝
- `GET /healthz` 返回 `{"healthy":1,"total":1,"service":"workbuddy2api"}`

边界：未在真实 Docker 环境跑管理端安装脚本（本仓库未改 Docker 面），
上表为其依赖面的逐项验证；`docker restart` / `docker logs` 两项依赖按 compose 与日志形态静态核对。
