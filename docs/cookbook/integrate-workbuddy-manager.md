# 接入 workbuddy-manager 控制台

用第三方 Web 控制台 [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)
管理本网关的账号池：扫码纳管、自动签到、密钥分发、IP 管控、调用日志与用量统计。

管理端独立部署在 `:7864`，账号轮询、并发、熔断仍由本网关（`:7863`）负责；
两者只通过 HTTP 接口与磁盘文件交互，本网关不需要为它改动代码。

## 前置条件

- 本网关已按 [README](../../README.md) 的 Docker Compose 方式部署：容器名 `workbuddy2api`，
  `config.json`、`auths/`、`data/` 都在部署目录内（默认 `/opt/workbuddy2api`）
- 管理端与本网关同机（默认经 `127.0.0.1:7863` 访问），且运行管理端的用户能执行 `docker`
  （重载与任务日志都靠 `docker restart` / `docker logs`）
- 管理端需要 Python 3.11+；用 Release 包可省去 Node 构建

## 运行形态与启动顺序

管理端**不是容器**：它是宿主上的 Python 进程（venv + `uvicorn server.main:app`，systemd 单元
`workbuddy-web`，端口 `7864`），前端由 `next build` 静态导出到 `web/out/`，交给 FastAPI 托管。
它只是在需要重载上游、采集任务日志时，调用宿主的 `docker` CLI（`docker restart` / `docker logs`）。

上游是本网关的容器：compose 服务 `wb2api`、`container_name: workbuddy2api`、端口 `7863`。

两者端口互不重叠，管理端不接管 `7863`：它只是本网关的 HTTP 客户端，进程本身只监听
`WB_MANAGER_PORT`（默认 `7864`）。下游若用管理端密钥，链路是
`下游 → :7864（管理端反代）→ :7863（本网关）→ 腾讯`，管理端是多一跳而非替换。
安装脚本对两个端口只做「已被占用」告警，不阻断安装。

要把本网关换到别的端口，管理端有三处要跟着改：compose 的端口映射、`WB2API_BASE`，
以及 `WB_UPSTREAM_PORT`——最后一项只被管理端的更新器用于端口收敛与健康检查，
`.env.example` 里没有它，需自己写进 systemd unit。

接入管理端**不改变既有调用方**：直连 `:7863`、或经域名反代到 `:7863` 的链路都照旧，
端口与密钥都不用动。只有当你想用管理端的密钥分发、配额、IP 管控与调用日志时，
才把走 `chat/completions` 的下游改指 `:7864`，并把密钥从 `config.json` 的 `api_key`
换成界面上新建的 `wbk_` 密钥（模型名不用改，本网关的 `model_aliases` 仍在下游生效）。

启动顺序是**先上游、后管理端**，`deploy/install.sh` 正是这个顺序（1/4 装并启上游 →
2/4 装管理端 → `systemctl enable --now workbuddy-web`）。

**启动管理端不会启动上游。** 管理端只重启/读取上游容器，不会拉起它；systemd 单元的
`Wants=docker.service` 只保证 Docker 守护进程在运行，与容器无关。上游容器靠 compose 的
`restart: unless-stopped` 在 Docker 启动时自行恢复，所以机器重启后两者都会回来，但这是
容器策略的效果，不是管理端拉起的。上游没起来时管理端照样运行，账号页显示未连接；
管理端自己的 `/healthz` 会实时探测上游 `/healthz`，可用来判断两者是否接上。

日常运维命令：

```sh
systemctl status workbuddy-web      # 管理端状态
systemctl restart workbuddy-web     # 重启管理端（改 unit 后先 daemon-reload）
journalctl -u workbuddy-web -f      # 管理端日志；首次的管理员密码也打印在这里
docker compose -f /opt/workbuddy2api/docker-compose.yml up -d   # 单独拉起上游
```

首次部署执行 `sudo bash deploy/install.sh` 即完成注册与启动，无需再手工 `systemctl start`。

### 常驻（默认）与开销观测

`deploy/install.sh` 收尾执行 `systemctl enable --now workbuddy-web`，常驻是默认形态；
从按需改回常驻用 `systemctl enable --now workbuddy-web`。

常驻只多一个 Python 进程（前端是静态产物，运行期没有 Node 进程），没有常驻子进程；
唯一的周期性工作是采集：每 45 秒 spawn 一次 `docker logs --tail 3000` 读容器日志、
解析后按去重键落库。该采集**没有独立开关**（`tasklog.start_collector()` 随应用启动无条件执行），
想停掉这份开销只能停整个服务。

观测方式（systemd 自带 cgroup 记账）：

```sh
systemctl status workbuddy-web        # 摘要行有 Memory / CPU 归因
systemd-cgtop -1 | grep workbuddy     # 实时 CPU / 内存排序
ps -o pid,rss,%cpu,etime -p "$(pgrep -f 'uvicorn server.main:app')"
```

参考量级（临时实例实测，macOS arm64 / Python 3.14）：空闲 RSS 约 55 MB，
30 秒内 CPU 时间仅 +0.04 秒，100 次 `/api/healthz` 请求后约 57 MB；同环境仅
`import fastapi/uvicorn/httpx` 的基线约 53 MB——即开销基本等同于「一个 Python 进程 + Web 框架」，
服务器上的数字通常相近或更低。

### 本地预览（不装 systemd）

只想先看看界面时不必走安装脚本：Release 包内含已构建的 `web/out`（不需要 Node），
解压后装四个依赖直接前台起：

```sh
tar xzf workbuddy-manager-<版本>.tar.gz && cd workbuddy-manager-*
python3 -m venv venv && ./venv/bin/pip install -r server/requirements.txt
WB_DATA_DIR=$PWD/data \
WB_AUTH_DIR=/opt/workbuddy2api/auths \
WB_UPSTREAM_CONFIG=/opt/workbuddy2api/config.json \
WB_STATIC_DIR=$PWD/web/out \
WB_MANAGER_HOST=127.0.0.1 WB_MANAGER_PORT=7864 \
WB_ADMIN_PASSWORD=<自定义密码> \
./venv/bin/python -m uvicorn server.main:app --host 127.0.0.1 --port 7864
```

首次启动按 `WB_ADMIN_PASSWORD` 建管理员（留空则随机生成并打印到日志），随后登录
`http://127.0.0.1:7864`。`WB_DATA_DIR` 独立，管理端自己的库与 `users.json` 不会落进网关目录。

预览实例读的是真实 `auths/` 与 `config.json`：界面上「保存设置」会重写 `config.json` 并
`docker restart` 上游容器，「重启上游」「删除账号」同理。只是看看就先别点这几处。

Release 附件挂在 `github.com/.../releases/download/...`；该域名不通时（国内服务器常见），
用 GitHub API 取资源地址再下载，绕开 `github.com` 直连：

```sh
curl -sS https://api.github.com/repos/ithtelab/workbuddy-manager/releases/latest \
  | python3 -c "import json,sys;print([a['url'] for a in json.load(sys.stdin)['assets'] if a['name'].endswith('.tar.gz')][0])" \
  | xargs -I{} curl -sSL -H 'Accept: application/octet-stream' -o m.tgz {}
```

### 无 root 部署：bind mount 的属主问题

以普通用户部署（目录在自己家目录、无 sudo）时，容器读不到账号也写不回刷新后的 token：
镜像内进程是 `app`（uid 10001），而 `auths/` 与 `data/` 是宿主 bind mount，属主是宿主用户。
把凭证收紧到 `chmod 600` 会让容器直接读不到（启动日志出现 `loaded 0 account(s)`），
而即使放宽到 644，容器也**无法回写** `SaveAtomic` 的刷新结果——重启后仍用旧 refresh token，
时间一长会把账号拖成 session dead。

无 root 时无法 `chown 10001`，改为让容器以宿主 uid 运行。放在
`docker-compose.override.yml`（未被跟踪）而不是改 `docker-compose.yml`：
管理端的「上游更新」会执行 `git checkout -- .` 丢弃已跟踪文件的改动，override 文件不受影响。

```yaml
services:
  wb2api:
    user: "1000:1000"   # 换成宿主 uid:gid（`id -u` / `id -g`）
```

`docker compose up -d` 生效（顺带解决 `data/state.json` 的落盘权限）。此时 `auths/*.json`
保持 `600` 即可，容器以同一 uid 读写。

管理端自身也要以该用户常驻：`systemctl --user` 服务 + `loginctl enable-linger <user>`，
这样无需 root 也能随系统启动并在登出后继续运行。

用户级服务下界面上的「系统更新」用不了，两个原因叠在一起：更新器的收尾动作是
`systemctl restart workbuddy-web`（系统级 scope，用户服务不在其内），而更新包与
`git fetch` 都要走 `github.com`——该域名在被墙的机器上不通。更新改为手工：
取 Release 包替换 `server/` 与 `web/out/`、同步 `.version`，再
`systemctl --user restart workbuddy-web`（`CHANGELOG.md` 一并复制，否则「更新日志」页报错）。

被墙的机器上若要让网关仓库自己能拉取，不必每次搬包：`github.com:443` 不通时
`ssh.github.com:443` 往往仍可达，在部署机上生成一对密钥、把公钥登记为仓库的
**只读部署密钥**，并给 `~/.ssh/config` 加一个 `HostName ssh.github.com` / `Port 443`
的别名，`git fetch origin` 即可照常用。此后上游更新就是标准三步：

```sh
cd <部署目录> && git fetch origin && git reset --hard origin/master
docker compose up -d --build
systemctl --user restart workbuddy-web   # 仅更新了管理端时才需要
```

`auths/`、`data/`、`config.json` 都在 `.gitignore` 中，`git reset --hard` 不会动它们；
未跟踪的 `docker-compose.override.yml`（见上）也保留。注意别用 `git clean -fdx`。

### 按需启动（不常驻）

管理端对网关是单向依赖，停掉它**不影响网关**：账号轮询、并发、熔断、以及四类定时任务
（签到 / 旅行 / 活跃上报 / 保活）全在网关内运行，`config.json` 与 `auths/` 也不由管理端托管。
按需启动只需取消开机自启，保留 unit 供手动启停：

```sh
sudo systemctl disable workbuddy-web    # 取消开机自启（不加 --now 则当前进程继续跑）
sudo systemctl start  workbuddy-web     # 需要时启动
sudo systemctl stop   workbuddy-web     # 用完停掉
```

停掉管理端会同时失去两样东西，按需使用前需确认可接受：

- **`:7864` 反代网关**随之不可用：走管理端密钥（`wbk_`）的下游、密钥配额、模型映射、
  入站 IP 管控与调用日志全部中断。直连网关 `:7863` 的下游（含用 `/v1/responses` 的
  Codex）不受影响。
- **任务记录的采集**停摆。采集在管理端进程内每 45 秒读一次容器日志（最近 3000 行）并落库，
  启动时会立即执行一次，因此再启动即可**幂等补采**（按时间戳+内容去重）仍在日志缓冲里的部分；
  但超出这 3000 行、或容器被重建（`docker compose up -d --build`）后的更早记录无法找回。
  网关会为每个请求打日志，日志行消耗很快，长期停用期间的任务历史会成段丢失。
  积分变动流水同理：它由查询余额时比对上一次快照产生，停用期间没有快照，
  再启动后首次查询只会把累计差额记成一条。

也可以完全不用 systemd，前台起一个临时实例（Ctrl-C 结束）：

```sh
cd /opt/workbuddy-manager && ./venv/bin/python -m uvicorn server.main:app --host 127.0.0.1 --port 7864
```

前台方式读不到 unit 里的环境变量，部署路径非默认时需自行 `export`（`WB_AUTH_DIR`、
`WB_UPSTREAM_CONFIG`、`WB2API_BASE` 等）；且此时**不要用界面上的「系统更新」**——
它的收尾动作是 `systemctl restart workbuddy-web`，会去拉起 unit 而不是你前台跑的这个进程。

## 管理端依赖的契约

下表是管理端对本网关的全部依赖，已用管理端自身的 `server/services/wb2api.py`
逐项实测（见文末「验证」）。改动本网关时不要打破其中任何一项。

| 管理端要做什么 | 交互面 | 本网关提供 |
|---|---|---|
| 账号列表 + 运行时状态 | `GET /status`（`Authorization: Bearer <config.json 的 api_key>`），按 `uid` 合并 | `accounts[]`：`uid` / `nickname` / `credits` / `cooling` / `disabled` / `success_count` / `in_flight` / `breaker_fails` / `last_success` |
| 模型列表 | `GET /v1/models`（同鉴权） | 动态模型表，失败回落静态表；含 `model_aliases` 解析后的名字 |
| 存活探测 | `GET /healthz`（无鉴权） | `{"healthy","total","service"}`；无可用账号返回 503 |
| 纳管 / 删除账号 | 直接读写 `<部署目录>/auths/workbuddy-<uid>.json`（嵌套 `account` / `auth`） | 启动时扫描该目录；同名文件覆盖即换凭证 |
| 可视化设置 | 读写 `<部署目录>/config.json` 的 `schedule` / `pool` / `cooldown` / `session_sticky` / `prompt` / `server` / `upstream` / `features` / `upstash` 段 | 启动时读取；未识别的键忽略，不会因多出的键启动失败 |
| 配置生效（重载） | `docker restart workbuddy2api` | 容器名由 `docker-compose.yml` 的 `container_name` 固定为 `workbuddy2api` |
| 任务记录页 | `docker logs --timestamps workbuddy2api`，按 `<kind> <uid8>: ...` 解析 | 调度日志保持 `travel` / `activity` / `checkin` / `keepalive` / `user-resource` 前缀与 uid 截 8 位形态 |
| 实际扣费列 | 读上游响应 `usage.credit` | `usage` 原样透传，上游未返回时管理端显示 `—` |

## 步骤 1：让管理端把本 fork 当作上游

管理端的默认上游仓库是 `Sliverkiss/workbuddy2api`，与同名 fork 是**两个仓库**，
不会自动选中本 fork。全新部署时显式指定：

```sh
sudo UPSTREAM_REPO=https://github.com/kongjianguan/workbuddy2api.git bash deploy/install.sh
```

本网关已另行部署、只装管理端时，用 `--skip-upstream` 跳过安装上游那一步：

```sh
sudo bash deploy/install.sh --skip-upstream
```

随后按本网关的实际位置调整管理端 systemd unit 里的四项（默认值与标准路径一致时无需改动）：

```ini
Environment=WB2API_BASE=http://127.0.0.1:7863        # 本网关地址
Environment=WB_AUTH_DIR=/opt/workbuddy2api/auths     # 本网关的 auths 目录
Environment=WB_UPSTREAM_CONFIG=/opt/workbuddy2api/config.json
Environment=WB_UPSTREAM_DIR=/opt/workbuddy2api       # 供「系统更新」定位上游目录
```

## 步骤 2：把「系统更新」的版本检测指向本 fork

管理端的版本检测默认查询 `Sliverkiss/workbuddy2api`，在 fork 部署下会长期提示
「上游有新版本」。加上这两项后检测对象变为本 fork：

```ini
Environment=WB_UPSTREAM_REPO=https://github.com/kongjianguan/workbuddy2api.git
Environment=WB_UPSTREAM_API_REPO=kongjianguan/workbuddy2api
```

```sh
systemctl daemon-reload && systemctl restart workbuddy-web
```

上游目录里的代码更新走该目录的 `origin` remote。用步骤 1 的 `UPSTREAM_REPO=` 安装时
`origin` 就是本 fork，更新即跟随本 fork；若部署目录的 `origin` 指向上游仓库
（用默认参数装过），先改回来：

```sh
cd /opt/workbuddy2api && git remote set-url origin https://github.com/kongjianguan/workbuddy2api.git
```

管理端的「仅上游更新」会执行 `git checkout -- .` 再 `git reset --hard origin/<分支>`。
`config.json`、`auths/`、`data/` 都在 `.gitignore` 中，不会被动到；但上游目录里的
**未提交改动会被丢弃**——包括管理端自己施加的端口收敛（`"7863:7863"` →
`"127.0.0.1:7863:7863"`），该改动每次更新后会重新施加，属预期。

## 步骤 3：验证

1. 管理端「账号」页列出 `auths/` 下的账号，并带积分、冷却、在途、最近成功时间
2. 直接确认上游状态可读：

   ```sh
   cd /opt/workbuddy2api
   KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")
   curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:7863/status | python3 -m json.tool
   ```

3. 管理端「设置」改一个值并保存（例如活跃上报条数改为 6）→ 容器自动重启，
   本网关日志出现 `活跃上报已启用：[...] 点（每号 6 条 ...）`
4. 「任务」页点「立即采集」→ 出现签到 / 旅行 / 活跃上报记录，说明容器名与日志形态都对

## 自建 manager fork

管理端的版本检测与「系统更新」默认对着 `ithtelab/workbuddy-manager`，自建 fork 后要让它们
也指向自己的仓库，否则更新按钮会把官方版本装回来。

```sh
gh repo fork ithtelab/workbuddy-manager --clone    # 或网页上 Fork
cd workbuddy-manager
( cd web && npm ci && npm run build:export )       # git clone 部署需先自建前端产物
sudo bash deploy/install.sh --skip-upstream        # 上游已有，跳过
```

在 systemd unit 里指向自己的 fork，然后 `systemctl daemon-reload && systemctl restart workbuddy-web`：

```ini
Environment=WB_MANAGER_REPO=kongjianguan/workbuddy-manager
```

「系统更新」拉取的是该仓库**最新 Release 的 `.tar.gz` 附件**：fork 尚未打过 tag 时接口返回 404，
更新不可用，需要自己发版——仓库自带 tag 触发的打包 workflow：

```sh
git tag v1.0.1 && git push origin v1.0.1
```

附件地址在 `github.com` 上，该域名不通的服务器（国内常见）点更新会失败，改用手工下载重装，
下载方式见上文「本地预览」里的 API 取址命令。

更新会覆盖 `server/`、`web/out/`、`deploy/` 并把旧版本备份到 `backup-<时间戳>/`；
`data/`（SQLite 库与 `users.json`）与 unit 里的环境变量不受影响。

网关侧无需另建 fork：本仓库的 `origin` 已经是 `kongjianguan/workbuddy2api`。

## 与上游的差异会怎么影响接入

| 本 fork 的差异 | 接入时的影响 |
|---|---|
| 原生 `/v1/responses`（本 fork 独有） | 管理端的反代网关只转发 `/v1/chat/completions`、`/v2/chat/completions` 与 `/v1/models`。只用 Responses 协议的客户端（Codex 等）要**直连本网关 `:7863`** 并用 `config.json` 的 `api_key`，不能走管理端 `:7864` |
| `model_aliases`（本网关，写在 `config.json`） | 与管理端「模型映射」（存管理端自己的库，作用于 `:7864` 入口）是两套独立映射，各管一层入口；改一处不影响另一处 |
| `data/` 仍是 bind mount（上游已改 named volume） | 管理端安装脚本对 `auths/` 与 `data/` 做 `chown 10001:10001`，正好适配 bind mount；`auths/` 必须保持 bind mount，否则管理端读不到宿主上的账号文件 |

改动可能打断契约的具体项（账号文件名与结构、`/status` 字段名、调度日志形态、
容器名、`auths` 挂载方式）见
[Agent Note：workbuddy-manager 上游契约](../../.agents/notes/implemented/architecture/2026-09-13-workbuddy-manager-upstream-contract.md)。
