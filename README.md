# qqbot

QQ 官方机器人（群 + 消息列表单聊）的 Go 单二进制骨架，按架构文档 `docs/ARCHITECTURE.md` 落地（该文档与 `docs/adr/` 只在本地维护，不入库）。

## 已实现（可运行）

| 模块 | 内容 |
|---|---|
| `internal/config` | TOML 配置、默认值、环境变量覆盖、启动校验 |
| `internal/httpx` | 统一出站：https 强制、域名白名单、按域名分桶的令牌桶、重试与全抖动退避 |
| `internal/api` | access_token 缓存（到期前 60 s 刷新，落 `bot_state`）、网关地址、发消息、富媒体上传（`/files`）；401 时作废内存与 `bot_state` 里的 token，换新 token 重试一次（并发 401 只刷新一次） |
| `internal/gateway` | op10 → Identify → 心跳 → Resume 状态机，断线指数退避；关闭码决定重连姿态（4006/4007/4900–4913 重新 Identify、4009 Resume、4008 至少等 60s、下架/封禁等永久错误停止重连）；握手 30s 截止；连续三次 <5s 断连冷却 60s；只有 READY 后活过 30s 才复位退避 |
| `internal/core` | 事件去重、群准入（白/黑名单）、落库、路由、按 peer 串行、被动回复额度、发图（上传 + `msg_type=7`）、调度器、7 天 GC、`/reload` 自重启、`/reset` 分级重置 |
| `internal/buildinfo` | 版本与 VCS commit 识别（`dev cc908d87+dirty`），供启动日志、`/ping`、`/reload` 对比 |
| `internal/store` | SQLite WAL + 单写连接 + 分批删除 + `incremental_vacuum` |
| `pkg/host` | 插件契约与 Host 能力实现（KV / 只读 Query / HTTP 转发（域名白名单由核心注入的客户端保证）/ 管理员判定 / 收件人目录 / 日志）；只依赖标准库与叶子依赖，不 import `internal/*` |
| `pkg/rpc` / `pkg/rpcsdk` | 插件线协议：长度前缀帧、方法表、错误码表；插件侧入口是 `rpcsdk.Serve(host.Plugin)`（ARCHITECTURE §3.9） |
| `internal/pluginproc` | 把 `plugins/bin/` 下的可执行文件变成 `host.Plugin`：`--manifest` 探测、spawn、1/2/4/8/16 分钟退避重启与熔断、RPC→SIGTERM→Kill 三级关闭 |
| `internal/pluginbundle` | 内嵌的出厂插件副本（`dist/<os>_<arch>/`），只作 `qqbot init --plugins` 的释放源；启动期不释放 |
| `internal/upgrade` | 自更新：检查发布、下载、SHA-256、同卷落位、pending 标记、`VACUUM INTO` 快照与回滚 |
| `internal/logx` | slog 文本/JSON handler；终端里 `WARN` 黄、`ERROR` 红，JSON 永不上色 |
| `internal/termcolor` | 是否该上色的唯一判定：真终端才上色，落盘与 journald 保持纯文本 |
| `internal/sysinfo` | 跨平台资源采集：CPU、内存、进程 RSS、分区容量、目录占用（无 cgo） |
| `tools/build` | 固定构建顺序：先编插件产物，再编核心（核心的 `//go:embed` 依赖上一步） |
| `plugins/echo` | 示例插件（独立可执行文件）：Manifest、配置读取、`!ping` / `!echo` |
| `plugins/adminping` | 仅管理员可用的 `/ping`：运行时长 + CPU / 内存 / 磁盘 / 目录占用 |
| `plugins/bootnotice` | 启动/重启后主动私聊管理员：上线时间、距上次启动间隔、平台与版本 |
| `deploy/qqbot.service` | systemd 单元：`StartLimitBurst`、`MemoryMax`、可写的 `/var/lib/qqbot`（自更新要换盘） |

插件是**独立进程**（ADR-0005）：核心启动时扫描 `<datadir>/plugins/bin/`，对每个文件跑 `--manifest` 探测，再 spawn 成子进程走 stdio RPC。加插件不用重编译核心，删文件就是卸载。

认证信息按约定写在**本地 config**（默认 `config/config.local.toml`，已 gitignore），不再依赖 `EnvironmentFile`；仍支持 `QQ_APP_ID` / `QQ_APP_SECRET` 覆盖。

## 首次安装

本地（Windows / Linux 直接用源码跑）：

```bash
# 1. 编产物。顺序是硬约束：核心的 go:embed 要在编译期看到插件产物
go run ./tools/build all
# Windows 产出 ./bot.exe；Linux 产出 ./bot
# 插件产物落在 internal/pluginbundle/dist/<os>_<arch>/

# 2. 生成配置 + 释放四个出厂插件（默认写 config/config.local.toml，已被 gitignore）
./bot.exe init

# 3. 填 [app].id / [app].secret（QQ 开放平台 → 机器人 → 开发设置）
$EDITOR config/config.local.toml

# 4. 启动（不带 -c 时读同一个默认路径）
./bot.exe
```

`init` 的输出就是它做的三件事（写配置 0600、建数据目录、释放插件）：

```text
已生成配置：config/config.local.toml
数据目录：.temp\data
出厂插件：adminping.exe, bootnotice.exe, echo.exe, md.exe → .temp\data\plugins\bin
```

> 数据目录按 `[store].path`（默认 `./.temp/data/qqbot.db`）的目录相对**当前工作目录**解析，不是相对配置文件。换目录安装时要么 `cd` 过去再 init，要么在配置里写绝对路径（见下面的 Debian 步骤）。上面是仓库内开发的形态，数据落在 `./.temp/data/`（已 gitignore）。

启动日志里能确认插件真的装上了（这几行都在连网关之前，网关连不上照样打）：

```text
[INFO] 发现插件 component=pluginproc plugin=echo version=0.1.0 api_version=12 commands=2 optional=0
[INFO] 核心已启动 plugins=4 sandbox=true
[INFO] qqbot 启动完成 config=config/config.local.toml ... version="dev 4a19ec1e" platform=windows/amd64 plugins=4 plugin_names=adminping,bootnotice,echo,md
```

`plugin_names` 是"哪些插件真的装上了"的唯一权威来源：`bootnotice` 不再报插件清单（插件进程看不到别的插件，契约里也没有这个访问器），启动日志这一行才是事实。

`init` 的两个开关：

| 参数 | 用途 |
|---|---|
| `--plugins` | 只释放**缺失**的出厂插件，不碰配置。删掉一个插件后补回来就靠它（已存在的文件不覆盖） |
| `--force` | 覆盖已存在的配置。会丢掉你填过的 `[app].id` / `[app].secret`，只在重来一遍时用 |

不带 `--force` 时 `init` 拒绝覆盖：`init: 配置文件 ... 已存在，不覆盖（要覆盖请加 --force；只补插件请用 --plugins）`。

Debian 上的安装（systemd）：

```bash
# 1. 目录与二进制（ExecStart 指向 /var/lib/qqbot/bin/qqbot，与数据目录同卷可写）
sudo install -d -o qqbot -g qqbot -m 0750 /etc/qqbot /var/lib/qqbot /var/lib/qqbot/bin
sudo install -m 0755 bot /var/lib/qqbot/bin/qqbot

# 2. 以 qqbot 身份初始化（cd 到数据目录，让默认的相对 [store].path 落在正确的位置）
sudo -u qqbot bash -c 'cd /var/lib/qqbot && ./bin/qqbot init -c /etc/qqbot/config.toml'

# 3. 把 [store].path 改成绝对路径，再让插件落到新的数据目录
sudo -u qqbot vi /etc/qqbot/config.toml              # path = "/var/lib/qqbot/qqbot.db"
sudo -u qqbot /var/lib/qqbot/bin/qqbot init --plugins -c /etc/qqbot/config.toml
sudo rm -rf /var/lib/qqbot/.temp                    # 第 2 步按相对路径释放的那一份

# 4. 装 unit 并启动
sudo install -m 0644 deploy/qqbot.service /etc/systemd/system/qqbot.service
sudo systemctl daemon-reload
sudo systemctl enable --now qqbot
```

第 3 步的两条命令不是仪式：`<datadir>/plugins/bin` 与 `<datadir>/upgrade` 都由 `[store].path` 的目录决定，改完路径必须让 `init --plugins` 在新位置再释放一次，否则启动日志里 `plugins=0`。

权限与目录的完整清单（缺一个 unit 就起不来）：

| 路径 | 谁创建 | 权限 |
|---|---|---|
| `/etc/qqbot/config.toml` | `init` | 0600，属主 qqbot（含 AppSecret） |
| `/var/lib/qqbot` | 第 1 步 `install -d` | 0750，属主 qqbot |
| `/var/lib/qqbot/bin/qqbot` | 第 1 步 `install` | 0755（`/upgrade` 会在同目录换盘） |
| `/var/lib/qqbot/qqbot.db` | 首次启动 `store.Open` | 属主 qqbot |
| `/var/lib/qqbot/plugins/bin/*` | `init` | 0755（要能 exec） |
| `/var/lib/qqbot/upgrade/` | 首次 `/upgrade apply` 或启动时回滚判定 | 属主 qqbot |
| `/run/qqbot` | systemd `RuntimeDirectory` | 0700 |

启动日志里会出现：

```
WARN 未检测到管理员，已生成一次性认领令牌 ttl=10m0s token=<64位十六进制>
```

在目标群或单聊发送 `/claim <token>` 即成为 owner（一次性、10 分钟过期）。
之后可用 `/admin add <openid> [备注]` 添加辅助管理员，用 `/ping` 查看运行状态，用 `!ping`、`!echo <文本>` 验证收发链路。

## 升级

`/upgrade` 是**唯一**的更新入口：只在人敲命令时发请求，启动期不检查、不自愈、也不会未经允许换二进制（ADR-0006）。权限是 **owner-only**——升级通道就是远程代码执行通道，管理员不该有这个能力。

| 命令 | 行为 |
|---|---|
| `/upgrade` | 预览：当前版本、最新版本、核心产物大小、插件升/跳过清单。**不改动任何文件** |
| `/upgrade apply` | 核心升级：快照数据库 → 下载 → 校验 SHA-256 → 同卷落位 → 回消息 → 重启换盘 |
| `/upgrade plugin <名称\|all>` | 插件升级：只重启那个插件的子进程，不重启核心，也不需要二次确认 |
| `/upgrade status` | 当前版本、有没有未确认的升级、上次失败原因 |

配置（四键，`[upgrade]`）：

```toml
[upgrade]
enabled    = true                       # false 时连检查都不发请求
api_base   = "https://api.github.com"   # 元数据：index.json 与 checksums.txt
asset_base = "https://github.com"       # 二进制；可指向镜像，只在没有 token 时生效
token      = ""                         # 私有仓：细粒度 token，只需 Contents: read
```

- **校验和与二进制分两个通道**是结构约束：`checksums.txt` / `index.json` 永远从 `api_base` 取，镜像被投毒时二进制变了、校验和对不上。
- **填了 `token` 之后三份产物全部走 `api_base`**（私有仓的发布资产在镜像上匿名取不到），`asset_base` 随之失效；`Authorization` 头只发给 `api_base` 的主机，绝不发给用户可配置的镜像。
- 校验只比 SHA-256：覆盖传输损坏、截断、镜像投毒；**不覆盖**上游仓库或账号被 takeover。
- `version` 是 `dev` 时拒绝升级——本地 `go build` 的产物没有可比对的版本号（`/ping` 首行能看到当前版本）。
- 同一时刻只允许一次升级；`/upgrade apply` 进行中再敲会得到「已有一个升级在进行中」。

一次核心升级的现场（`/upgrade` 然后 `/upgrade apply`）：

```text
自更新预览（未改动任何文件）
当前 0.3.1｜linux/amd64
最新 0.4.0（v0.4.0）
核心产物 qqbot-0.4.0-linux-amd64（31.0 MiB）
插件（只列已加载的插件）
  echo 0.1.0 → 0.1.1  升
  md 0.3.0 → 0.3.0  跳过（已是最新）
用 /upgrade apply 应用核心升级：会先快照数据库，再重启进程。
```

```text
升级已就位：0.3.1 → 0.4.0
数据库快照 /var/lib/qqbot/upgrade/snapshots/qqbot-20261023-151230.000.db
产物已校验并落位到 /var/lib/qqbot/bin/qqbot.new
插件（只列已加载的插件）echo 0.1.0 → 0.1.1
接下来重启进程换盘；重启后走 Resume，不额外消耗 Identify 配额。用 /ping 对比版本与启动时间即可确认生效。
```

预览里的插件清单只包含**已加载**的插件：被 `[plugins].disabled` 停用或加载失败的插件，核心不知道它们的版本号。

插件升级是独立的一条路：

```text
/upgrade plugin all
插件升级（发布 v0.4.0｜linux/amd64）
  echo 0.1.0 → 0.1.1  升（2.9 MiB）
  md 0.3.0 → 0.3.0  跳过（已是最新）
耗时 412ms
```

前置条件（不满足时 `/upgrade apply` 会在下载之前就失败）：

1. `deploy/qqbot.service` 的 `ExecStart` 指向 `/var/lib/qqbot/bin/qqbot`——升级要在**同卷**把 `<exe>.new` rename 成 `<exe>`，而 `ProtectSystem=strict` 让 `/opt` 只读；
2. `[store].path` 的目录（`datadir`）对服务账号可写：`<datadir>/upgrade` 要写标记与快照；
3. 版本不是 `dev`。

## 回滚

两条路，自动的只管"新版本起不来"，其余靠手工。

**自动（核心）**：`/upgrade apply` 在换盘前写 `<datadir>/upgrade/state.json`（目标版本、旧文件、尝试次数）并对数据库做 `VACUUM INTO` 快照（保留最近 2 份）。新进程起来后观察 30 秒（`core.Start` 完成 + 网关协程存活），稳定就删掉标记。若下一次启动还看到标记，说明上一版没活过观察窗：`attempts` 加一，`> 1` 时自动把 `qqbot.old` 换回来并重启旧版本，日志是一条 Error。

```text
[ERROR] 新版本启动后未能稳定运行，已回滚到旧版本并重新启动 reason="上次升级到 0.4.0 后进程未能稳定运行（这是第 2 次尝试启动），回滚到 0.3.1"
```

判定用"存活 30 秒"而不是"网关连上"：网络抖动不该触发回滚。已知边角——30 秒内敲 `/reload` 会让 `attempts` 多加一次，可能误判回滚；代价是退回上一版，再敲一次 `/upgrade` 即可。

**自动（插件）**：插件替换先停旧进程、换文件、起新进程，**5 秒内没完成 `init` 握手**就自动把旧产物换回来并重启它，成功则记一条 Warn「插件热升级失败，已回滚到旧版本」。插件没有 pending 标记，也没有数据库快照——它无状态，回滚就是换回文件。

**手工（核心）**：自动回滚没救回来、或想立刻回到上一版：

```bash
sudo systemctl stop qqbot
sudo rm -f /var/lib/qqbot/upgrade/state.json          # 不清标记，下次启动还会再判一次回滚
sudo install -m 0755 /var/lib/qqbot/bin/qqbot.old /var/lib/qqbot/bin/qqbot
sudo systemctl start qqbot
```

`qqbot.old` 是上一次换盘留下的旧映像（Unix 是硬链接，Windows 是改名），启动稳定后会被清掉，所以它只在"刚升级过"的这一段时间里存在。

**数据库也要回滚**时：回滚二进制不等于回滚 schema。安全回滚窗口只有一个版本，且要求迁移只增不减；跨破坏性迁移必须配快照还原：

```bash
sudo systemctl stop qqbot
sudo cp /var/lib/qqbot/upgrade/snapshots/qqbot-20261023-151230.000.db /var/lib/qqbot/qqbot.db
sudo rm -f /var/lib/qqbot/qqbot.db-wal /var/lib/qqbot/qqbot.db-shm
sudo systemctl start qqbot
```

快照是 `VACUUM INTO` 出来的完整库文件，不是日志；拷回去就等于回到那一刻。

## 离线恢复

机器上不了网、或者升级把二进制换坏了，就手工换文件——这条路不需要 `/upgrade`，也不需要 Go 工具链。

```bash
# 1. 停服务
sudo systemctl stop qqbot

# 2. 从 U 盘 / 内网拷贝换上去（权限必须是 0755）
sudo install -m 0755 /media/usb/qqbot /var/lib/qqbot/bin/qqbot

# 3. 清掉可能残留的升级状态，否则下次启动会按"未确认的升级"再回滚一次
sudo rm -f /var/lib/qqbot/upgrade/state.json /var/lib/qqbot/bin/qqbot.new

# 4. 补回被删掉的出厂插件（不碰配置、不覆盖已存在的文件）
sudo -u qqbot /var/lib/qqbot/bin/qqbot init --plugins -c /etc/qqbot/config.toml

# 5. 起来，用启动日志确认插件数量
sudo systemctl start qqbot
journalctl -u qqbot -n 30 --no-pager | grep '启动完成'
```

第 4 步的语义是「只补缺失」：`plugins/bin/` 里已有的文件一律不动，所以你单独升过的插件不会被降级；反过来说，**删掉一个插件就是卸载它**，启动期不会自愈。

干净克隆自编的二进制没有出厂插件可释放：`internal/pluginbundle/dist/<os>_<arch>/` 里只有 `.gitkeep`（`go:embed` 不能出包目录），此时 `init --plugins` 会明确报错：

```text
init: 释放出厂插件: 本产物没有出厂插件副本（dist 目录里只有 .gitkeep）：请使用发布版，或先运行 `go run ./tools/build plugins` 再编译核心
```

先跑 `go run ./tools/build plugins`（或 `all`）再编核心，就有了。

## 调试模式

打开后，每收到一条事件都会在控制台输出一段人工可读的轨迹：谁发的、命中哪个插件的哪条命令、插件返回了什么、最终发出的正文。两种打开方式，**命令行优先于配置文件**：

```bash
go run ./cmd/bot -debug     # 临时开，不改文件
```

```toml
[debug]
enabled = true   # 常开
level = "full"   # full（默认）｜plugin
raw = false      # 额外打印平台原始 JSON（仅 full 级别）
```

`-debug` 会顺带把 `log.level` 降到 `debug`，两边细节保持一致。轨迹走 stdout，时间戳在终端里是青蓝色；同一套判定也用在日志上（`WARN` 黄、`ERROR` 红），一旦重定向到文件就全部退回纯文本，落盘的内容可以直接 grep。

普通日志与轨迹共用同一副行首，扫起来是连续的：

```
[13:50:33] [INFO] adminping 插件已初始化 plugin=adminping 权限=bot-admin
[13:50:34] [WARN] 队列已满，丢弃事件 peer=g1
[13:50:34] [ERROR] 插件处理失败 plugin=md err="i/o timeout"
```

值只有必要时才加引号（空格、等号、引号、控制字符），所以 `peer=g1` 能直接 grep；`format = "json"` 时仍是标准 slog JSON，供机器读取。

### `level = "full"`（默认）

```
[15:24:01] 收到 群聊消息 · GROUP_AT_MESSAGE_CREATE
群    ：摸鱼办事处(1ABC...)
发送者：[user]小明(EF9A...)[admin]
peer  ：1ABC...
事件  ：ROBOT1.0_...
内容  ：!ping
[15:24:01] 命中插件 echo v0.1.0 · 命令 "!ping" · 优先级 100
[15:24:01] 插件 echo 返回回复（1.20ms，4 字）
[15:24:01] 已发送（被动回复 群聊 → 1ABC...） seq=1 · 16ms
内容  ：pong
```

`群` 行是 `群名(group_openid)`：名字取自本地 `qq_groups`（平台接口对普通机器人返回 `11253`，多数群没有名字，此时只剩括号里的 openid）。

`发送者` 是 `[Bot 角色]昵称(openid)[群内角色]`：

- 第一个方括号是 `admins` 表里的 Bot 角色（`owner` / `admin` / `user`），全局有效，且在去重与群闸门之前就解析好——被闸门丢掉的消息也看得出是谁发的；
- 第二个方括号是平台给的群内角色，只有群聊、且载荷里确实带 `member_role` 时才追加；
- 单聊不下发昵称（`author.username` 实测恒为空串），也没有群角色，所以只剩 `[user](openid)`。

### `level = "plugin"`

群里绝大多数消息与机器人无关，`full` 会把它们全部打出来。`plugin` 只保留"触发了谁、回了什么"，不输出事件 ID、发送者与 peer：

```
[15:55:28] 命中插件 echo v0.1.0 · 命令 "!ping"
内容  ：!ping
[15:55:28] 被动回复 群聊 · 16ms
内容  ：pong
```

该级别下未命中消息、重复事件、原始 JSON 一律静默；核心指令（`/claim`、`/admin`、`/reload`）只留一行指令名。`raw = true` 与 `level = "plugin"` 同时配置会被启动校验拒绝。

五个刻意的取舍：

- **在去重之前打印**（`full`）：重复投递也会留一行 `重复事件，已跳过`，排查"为什么没回复"时不会误判成插件没命中；
- **正文只打一次**：插件返回时只报字数，真正发出去时再打全文，同一条内容不会出现两遍；
- **`raw = true` 才打原始 JSON**：一条事件可能几十行，只在确认平台字段时打开；
- **群名读本地登记**：不为了显示名字去调接口，读不到就只留 openid；
- **分级而不是删信息**：默认仍是 `full`，需要安静时切 `plugin`，不动代码也不改日志格式。

生产环境保持 `enabled = false`：轨迹里有消息正文与 openid，不适合进 journald。

## 权限与群准入

### 三级角色（全局）

权限判定在**核心**里完成，插件只声明"需要什么"，代码里不写判权语句。

| 角色 | 产生方式 | 能做什么 |
|---|---|---|
| `user` 普通用户 | 默认 | 触发公开指令 |
| `admin` Bot 管理员 | 所有者用 `/admin add` 添加 | 全部管理指令，唯独**不能增删管理员**，也不能把别人提成 owner |
| `owner` Bot 所有者 | `/claim` 认领（一次性令牌，全局唯一） | 全部权限，**并且能增删 admin** |

Bot 角色是**全局**的：openid 跨场景同值，群里添加与私聊添加效果一致，
不存在"只在某个群生效"的委托。

另有一个正交维度是**平台给出的群内角色**（`author.member_role`）：群成员 / 群管理员 / 群主。
它用于"群管级"权限点，**不是** Bot 管理员——群主不会因此获得重启机器人或看主机指标的权限；
反过来，Bot 管理员在任何群里都按群管级处理。私聊没有群，角色默认等价于 `member`。

| 指令 | 权限 | 说明 |
|---|---|---|
| `/admin list` | Bot 管理员 / 所有者 | 列出全部授权与来源场景 |
| `/admin add <openid> [备注]` | 仅所有者 | 新增 admin；备注可写 QQ 号，方便人眼识别 |
| `/admin rm <openid>` | 仅所有者 | 删除 admin；所有者本身不可删 |
| `/admin whoami` | **所有人** | 只回自己的 openid / union / Bot 角色 / 群内角色 |
| `/group ...` | Bot 管理员 / 所有者 | 群准入管理，见下 |
| `/reload [说明]` | Bot 管理员 / 所有者 | 重启进程加载磁盘上的新二进制，见下节 |

群聊里需 @ 机器人后发送，单聊直接发。

插件侧的声明方式（`plugins/adminping` 就是范例）：

```go
Commands: []host.Command{{
    Prefix:     "/ping",
    Permission: host.PermBotAdmin,      // 核心在路由阶段拦截
    DenyReply:  "该指令仅管理员可用。",   // 是否真的发出由 [permission].deny_reply 决定
}},
```

### 群准入（白名单 / 黑名单）

群准入决定"哪些群能跟机器人说话"，和权限一样由核心负责，插件无法接管。

| 指令 | 说明 |
|---|---|
| `/group list [allow\|deny\|all]` | 列出已登记的群：openid ↔ 名称 ↔ 名单（含已退出标记） |
| `/group status [openid]` | 私聊不带参数看总览；群聊不带参数看当前群 |
| `/group allow [openid]` | 加入白名单 |
| `/group deny [openid]` | 加入黑名单 |
| `/group clear [openid]` | 移出黑白名单 |
| `/group name [openid] <名称>` | 更新群名称 |
| `/group refresh` | 重新获取所有**在群**群的名称（后台跑，完成后私聊汇报） |

群聊里省略 `openid` 表示当前群；按 id 操作通常在私聊进行（id 是 32 位十六进制）。

- 两套开关独立，默认都开；黑名单优先于白名单。
- 判定发生在去重、落库、路由**之前**：未放行的群消息不落库、不触发插件，
  调试轨迹里只留一行 `群未放行，已丢弃（原因）`。
- 群事件仍会登记 `group_openid`，所以被拦下的群也能在 `/group list` 里看到。
- **例外一**：Bot 管理员在"未进白名单"的群里发 `/group allow`，用于新群自助接入；
  黑名单是硬阻断，群内任何人都不能自助解除，只能从私聊操作。
- **例外二**：入群/退群通知不走准入判定——新群此刻必然未放行，走闸门这条信息就丢了。

**群生命周期**（平台事件 `GROUP_ADD_ROBOT` / `GROUP_DEL_ROBOT`）：

| 事件 | 行为 |
|---|---|
| 机器人被拉进群 | 立即建档 + 取群名，并私聊通知（默认只发所有者） |
| 机器人被移出群 | 标记 `left_at`，并私聊通知；**名单与名称保留**，重新拉回群时不用再授权 |

- 通知收件人由 `[group].notify` 控制：`owner`（默认）｜`admin`（含管理员）｜`off`。
  通知走主动私聊，受 `[active].enabled` 与每日预算约束。
- 已退出的群不再计入"在群"、不再被 `/group refresh` 刷新，但仍出现在 `/group list`
  末尾并标出退出时间。
- **自愈**：收到该群任何消息都会清掉退出标记——断线期间丢失的入群事件靠这条兜住。

**群名**来自 `GET /v2/groups/{id}/info`（该接口需要内邀，错误码 `11253`）：
能取到就自动记录，取不到留空，用 `/group name` 补；手工名字（`name_source=manual`）
不会被自动抓取或 `/group refresh` 覆盖。

```toml
[group]
whitelist  = true    # 只允许白名单群
blacklist  = true    # 丢弃黑名单群（优先于白名单）
fetch_name = true    # 见群/入群时尝试调用群信息接口取名称
notify     = "owner" # 入群/退群通知：owner｜admin｜off
```

> **升级提示**：白名单默认开启且初始为空，升级后所有群消息都会被丢弃。
> 先私聊机器人 `/group list` 找到目标群 id（收到过该群消息、或机器人被拉进群时都会登记），
> 再 `/group allow <id>`；或者以 Bot 管理员身份在群里直接发 `/group allow`。

### 为什么是 openid 而不是 QQ 号

QQ 机器人 v2 **不下发 QQ 号**。但事件载荷里的 `author.id` 是平台声明的用户唯一标识，
群聊的 `member_openid` 与单聊的 `user_openid` 对同一个人取同一个值（实测与官方示例一致），
所以 openid 可以直接当跨场景主键用，`union_openid` 只是兜底（可能为空）。

添加管理员的实际流程：

1. 对方在群里或私聊发送 `/admin whoami`，拿到自己的 openid；
2. 把 openid 发给所有者；
3. 所有者执行 `/admin add <openid> 123456789`（备注写 QQ 号）。

授权是全局的：无论在哪添加，之后在任何群与私聊都生效。
`identities` 表仍记录 openid ↔ union_openid 的对应关系，`AdminTargets` 优先用它解析
单聊投递地址；解析不到时直接用同一 openid（跨场景同值），所以群里的管理员也能收到启动通知。

## 启动通知与主动消息

`plugins/bootnotice` 在进程启动（含重启）后，等网关就绪，主动私聊全部可联系的管理员：

```
机器人已上线
版本 dev 4a19ec1e
启动 10-17 14:56:19｜windows/amd64
上次启动 10-17 12:51（2小时5分前）
```

正文里**不再有"启用插件 N 个"那份清单**：插件已经是独立进程（ADR-0005），本插件拿不到"核心装了哪些插件"——契约里没有这个访问器，为一行日志加一个访问器要动 `api_version`。要确认装了哪些插件，看核心启动日志的 `plugin_names`（那才是权威来源），或者看 `/ping` 的版本行与插件报错。

主动消息在本项目里是**受控通道**，而不是默认放开的能力：

| 闸门 | 位置 | 行为 |
|---|---|---|
| 总开关 | `[active] enabled` | 关闭后所有 `SendActive` 直接返回 `ErrActiveDisabled` |
| 插件白名单 | `[active] plugins` | 只有名单内插件能发；插件拿到的 Sender 是带名字的包装 |
| 每日预算 | `[active] daily_per_peer` | 每 (场景, 收件人) 每天最多 N 条，落库记账（重启不清零、跨日按调度时区 `Asia/Shanghai` 换桶，与 cron 同源），先扣预算再发 HTTP；平台明确拒绝时归还，网络失败不归还 |
| 重启抑制 | `[plugins.bootnotice] min_interval` | 距上次启动不足该间隔则整轮跳过，防崩溃重启刷屏 |
| 重试 | `[plugins.bootnotice] retry_delay` | 失败只重试一次；主动消息没有 `msg_id` 幂等键，多试等于重复推送 |

```toml
[active]
enabled = true
plugins = ["bootnotice"]
daily_per_peer = 5

[plugins.bootnotice]
enabled = true
delay = "10s"
min_interval = "10m"
retry_delay = "30s"
```


## `/reload`：重启进程验证更新

`/reload` 的做法是**重新执行磁盘上的核心二进制**，用来回答"新构建到底部署上没有"。

插件已经是独立进程（ADR-0005），所以这里的边界和 P1 时代完全不同：

- **插件代码改动不需要 `/reload`，也不需要重编译核心**：把新产物放进 `<datadir>/plugins/bin/` 就是装上（同名文件覆盖后敲一次 `/reload`，或对单个插件用 `/upgrade plugin`）；
- `/reload` **不重新编译任何东西**。它重启的是核心进程，新进程在装配期会重新扫 `plugins/bin/`，所以新放进去的插件会随之加载、被删掉的第三方插件就此消失。出厂插件（四个示例）是例外：启动期会用内嵌副本补回缺失的文件，要停用请写进 `[plugins].disabled`（#52）；
- 只想换一个插件时用 `/upgrade plugin <名称>`，它只重启那个插件的子进程，核心与其它插件不重启。

```
/reload              重启
/reload 上线 v0.2     重启，备注写进日志
/reload help         只看用法，不重启
/restart             同 /reload
```

执行顺序（每一步都有原因，不能颠倒）：

1. **先回一条消息**——被动回复窗口只在收到消息时存在，重启后这条就发不出去了；
2. 网关断开并把 `session_id` / `seq` 落盘，下次启动直接 Resume；
3. 关停核心：调度收尾 → 派发排空 → 插件子进程按 RPC close → SIGTERM → Kill 三级关闭；
4. 关闭 SQLite，避免新进程与旧进程抢写锁；
5. 重新执行自身：
   - Linux / macOS：`execve` 原地替换进程映像，**PID 不变**，systemd 的 cgroup、日志句柄与控制台都沿用原来的；
   - Windows：没有 `execve`，拉起新进程后旧进程退出；此时网关与数据库都已关闭，不会出现两个实例抢同一个 session。

`/upgrade apply` 走的是同一条序列，只在第 5 步之前多一次换盘（`qqbot.new` → `qqbot`）。

重启后 `bootnotice` 会私聊「机器人已上线」，`/ping` 首行显示版本，两边对比就能确认：

```
版本 dev 3f2a1c9d｜go1.26.1
运行 8秒（启动于 10-17 15:40:02）
```

版本串带 `+dirty` 表示构建时工作区有未提交改动——部署的不是干净构建时一眼能看出来。

### 开发期：让改动用 `/reload` 生效

用 `tools/devrun` 托管进程，它在前台持有终端，每次重启前跑一遍 `go run ./tools/build all`（先插件、后核心）再换二进制拉起：

```bash
go run ./tools/devrun -c config/config.local.toml
```

编译失败时它会打印错误并用**旧二进制**继续运行，机器人不会掉线。Windows 上换二进制要先把旧的改名成 `.old`，若两次改名之间被中断（`bot.exe` 缺了、`.old` 还在），下次启动会先把它从 `.old` 恢复回来，不会变成"二进制凭空消失"。

原理：bot 在 `QQBOT_SUPERVISOR=1` 下把 `/reload` 从「自己换映像」改为**以退出码 75 退出**，由父进程接手重编译。Windows 没有 `execve`，这一步还顺带解决了「每次重载都把终端丢回提示符」的问题——终端始终由 devrun 占着，日志连续。

生产环境不设置该变量，`/reload` 仍是「换文件 + execve」，与上一节一致。

### 发布一个新版本

```bash
# 本机交叉编 linux/amd64（发布流水线用同一套工具，见 .github/workflows/release.yml）
go run ./tools/build all -target linux/amd64 -o dist/qqbot-linux-amd64
```

正式发布走 tag：push `v*` 之后 CI 依次跑「三个平台的插件产物 → GoReleaser 编核心与 checksums.txt → `tools/index` 生成 index.json → 下载回来冒烟」。版本号只经 `-ldflags -X .../buildinfo.Version` 注入，本地 `tools/build` 不注入（所以本地产物是 `dev`，`/upgrade` 会拒绝自更新——这正是防止本地构建混进生产的闸门）。

一次发布里的资产形状（#52）：

| 资产 | 什么时候有 |
| --- | --- |
| `qqbot-<ver>-<os>-<arch>[.exe]` | 每次发布，3 个平台 |
| `checksums.txt` / `index.json` | 每次发布 |
| `qqbot-plugin-<name>-<ver>-<os>-<arch>` | **只在该插件版本号变了时上传**；没变就沿用上一版资产，index.json 里的 url 仍指向旧 tag |

插件版本号没变 = 没更新。所以**改了插件代码必须涨 `Manifest().Version`**；契约版本（`host.APIVersion`）变化时由启动期对齐兜底，不依赖版本号。核心每次发布都带着一份内嵌插件副本，覆盖升级核心后启动时会把插件对齐到内嵌版本（只升不降；磁盘版本更高时保留并记一条 info）。

服务器上不需要 `scp` + `systemctl restart` 了：tag 发出去之后在 QQ 里敲 `/upgrade` 看一眼、`/upgrade apply` 应用即可。要手工换文件（离线 / 救火）见上面的「离线恢复」。

### 几个边界

- `go run ./cmd/bot` 下 `/reload` 只会重启同一份临时构建，**不会重新编译**；开发时请改用 `tools/devrun`（见上一节）。
- `tools/devrun` 只在开发工作区有意义：它需要源码与 Go 工具链。服务器上部署的是二进制。
- 同一进程只接受一条重启请求，重复发只回「已有重启请求」。
- `/reload` 与 `/reset` 无关：前者重启进程并重读配置，后者按级别清数据（见下一节）。
- 插件不能声明 `/claim` `/admin` `/group` `/reset` `/reload` `/restart` `/upgrade` 这些核心前缀，否则启动即拒绝加载。

## `/reset`：分级重置

四个级别，越往下清得越多、身份要求越高、冷却越长：

| 指令 | 清什么 | 身份 | 冷却 |
|---|---|---|---|
| `/reset reconnect`（`r0`） | 不动数据，断开并立即重连（保留 session，走 Resume） | owner | 60 s |
| `/reset session`（`r1`） | 本会话的插件状态（`plugin_kv` 里 `peer` 非空的行）与会话接管（`sessions` 该 peer 的行） | Bot 管理员 | 10 min |
| `/reset data`（`r2`） | 本会话的消息、媒体文件、任务记录、主动消息日额度（`active_usage` 按 peer 清）与会话接管（`sessions` 按 peer 清） | Bot 管理员 + 确认码 | 1 h |
| `/reset all`（`r3`） | 全部业务数据 | owner + 确认码 + 短语 | 24 h |
| `/reset status` | 各档冷却与最近 5 条记录 | Bot 管理员 | — |

群聊里作用于当前群，私聊里作用于你自己；也可以显式指定 `/reset data <group_openid>`。

```
管理员：/reset data
bot   ：将清空该会话的消息、媒体文件与任务记录：当前消息 128 条、媒体 6 个、任务记录 12 条，不可恢复。
        保留：管理员、群名单、网关凭证、任务定义。
        60 秒内回复 /reset data 481902 确认。
管理员：/reset data 481902
bot   ：已清空本会话数据：消息 128 条、媒体 6 个（文件 6）、任务记录 12 条。
        同类重置的冷却到 13:05。
```

几条容易踩的：

- **预览不删任何东西**。数字取自库（不是估算），确认码绑定发起者 + 目标会话 + 级别：换个会话用不了，换个人代答不了，换个级别也不行。重复发起会顶掉旧码并发新的。
- **R3 要短语**：`/reset all <code> 确认`。确认码防误触，短语防"看都没看就按了两次"。
- **任何级别都不动**：`admins`（删了就永久锁在门外）、`bot_state`（token / session / seq）、`qq_groups`（群名单，清掉等于对所有群静默拉黑）。
- **R2 清 `job_runs` 但保留 `jobs`**：执行历史是该群的业务数据，任务定义（含 per-peer 实例行）是配置。
- **媒体先删文件再删行**；配了 `[media].dir` 时只删该目录内的路径，库里的 `path` 若被写坏，不会变成一次任意文件删除。
  入站媒体不下载（见「发图片」），所以这一步现在扫不到任何行——路径保留，接回下载时不用重写。
- 冷却按 `(级别, 会话)` 计：连清两个群互不影响；R0/R3 是全局冷却。
- 每次尝试都进 `reset_audit`，包括失败与未确认就过期的那些。执行失败标 `failed`，`/reset status` 显示「失败（未占冷却）」，可以立即重试——冷却只认 `done`。

## 发图片（富媒体）

插件通过 `Reply.Image` 发图，上传与发送都在核心完成——插件不碰平台接口。

```go
// 本地文件：发送时才读取，所以路径可以稍后才有
&host.Reply{Image: host.ImageFromFile("/tmp/chart.png")}

// 内存字节：文件名按内容嗅探（png/jpg/gif/webp/bmp）
&host.Reply{Image: host.ImageFromBytes(png)}

// 公网地址：平台侧下载转存，本机不出站
&host.Reply{Image: host.ImageFromURL("https://example.com/a.png")}
```

平台约束直接决定了实现形态：

| 约束 | 后果 |
|---|---|
| 先 `POST /v2/{groups\|users}/{openid}/files` 换 `file_info`，再以 `msg_type=7` 发送 | 一次发图 = 两次 HTTP；**上传失败不消耗被动配额**（先上传、后占位） |
| 一条消息只有一种 `msg_type` | `Content` 与 `Image` 同设时拆成两条：先文本、后图片，**消耗两格被动配额** |
| `file_data` 是 base64 且 ≤ 5 MB | 超了本地直接报错；分片上传未实现，大图请用 `ImageFromURL` |
| 群与单聊的上传接口互不通用 | 场景由核心按事件的 `Scene` 决定，插件指定不了 |
| 图片软限 20 MB，超过降级成文件消息 | 5 MB 直传上限更先撞上，实际走不到这里 |

几处行为细节：

- `Content` 为空时只发图片；`MsgType=7` 却没设 `Image` 会被核心拒绝——发出去只会被平台打回。
- 图文拆分后第二条失败，错误里带「前 N 条已发出」：别把两条都重发，否则文字会重复。
- 主动消息同样能发图，但日预算**先扣再上传**（按天计的额度比 5 分钟的被动窗口稀缺）。
- 上传超时放宽到 30 s：默认的 `[http].timeout=5s` 传不完 5 MB 的 base64 请求体。

## 网关断开：谁决定重连

关闭码不是日志脚注，它决定下一轮该 Identify、Resume，还是干脆不再连：

| 关闭码 | 处置 |
|---|---|
| 4006 / 4007 / 4900–4913 | 清 `session_id` / `seq`，下一轮重新 Identify（不再 Resume 死 session） |
| 4009 | 保留 session，下一轮 Resume |
| 4004 | 作废 access_token 后重连 |
| 4008 | 至少等 60s，抖动只加长不缩短 |
| 4001 / 4002 / 4010–4014 / 4914 / 4915 | 永久错误：停止重连，进程以退出码 1 退出，由 systemd `StartLimit` 收敛 |

表外的关闭码按退避重连、不动 session。握手（Identify/Resume 到 READY/RESUMED）有 30s 截止，等不到就断开重连——否则平台"收下 WS、心跳也 ACK、就是不回 READY"会让读循环永久阻塞。退避只在连接真正稳定后复位：READY 后活过 30s 才回到 1s 起；「连上就断」（READY 后不足 30s）不复位，连续三次寿命 <5s 的连接直接进 60s 冷却。

## 单实例：同一台机器上的同一 bot 只能跑一个进程

启动时按「先 appID、后数据目录」的顺序取两把**操作系统级**文件锁，拿不到就直接退出，不连网关、不开库：

- **appID 维度**：`%TMP%/qqbot-<sha256(appID) 前 16 位>.lock`（可用 `QQBOT_LOCK_DIR` 换目录，见下），约束"这台机器上的这个 bot"——换个数据目录也躲不掉；
- **数据目录维度**：`<store.path>.lock`，约束"这份 SQLite 只被一个进程打开"。

```
启动失败: 另一个实例正在运行：appID 102xxxxx 已被占用——pid 20384（D:\vscode\go\qqbot\bot.exe，启动于 2026-10-18 11:52:07，主机 DESKTOP-8H2K）持有锁 C:\Users\knight\AppData\Local\Temp\qqbot-3f2a1c9d4b7e0a15.lock
```

数据目录冲突的文案会额外点出占用者的 appID，并提示改 `[store].path`——两种冲突该做的事完全不同（去结束那个进程 vs 换一份库），所以错误必须能区分。

这不是保守限制。两个进程连同一个 appID 会抢同一个网关 session —— 日志里反复出现 `4902 reset by resume`，两边轮流被踢下线；若还共用一份库，同一个 `msg_id` 会被两边各当成一条事件，用户看到重复回复，被动额度翻倍扣。

锁是**内核持有的锁，不是"锁文件存不存在"**。所以 `kill -9`、任务管理器结束、断电重启
都不会留下"需要手动删掉的残留锁"；也正因如此，锁文件本身永不删除（删掉别人正持有的
锁会让第三个进程误以为没人占用）。

| 场景 | 行为 |
|---|---|
| 正常退出 / Ctrl-C | 显式释放，下一个进程立刻能拿 |
| 崩溃 / 强杀 | 句柄随进程销毁，内核释放 |
| `/reload` 拉起的进程 | 旧进程已经先放手，正常一次就能拿到；仍留 15 s 余量 |
| 另一个实例 | 立刻退出（wait=0），带着上面的错误 |

错误里带 pid、可执行文件路径与主机名，是为了**能精确结束那个进程**而不是让你猜：Windows 在任务
管理器里搜 `qqbot`，Linux 用 `ps -p <pid> -o pid,etime,cmd`。

appID 锁落在系统临时目录，**临时目录不得对运行中的 bot 做自动清理**（`tmpwatch`、`systemd-tmpfiles` 之类）：Unix 的 `flock` 跟的是 inode，文件被删掉后下一个进程会在同路径另建一个，两把锁互不相识，"单实例"当场失效。

systemd 单元默认带 `PrivateTmp=true`，`/tmp` 对每个 unit 是**私有命名空间**：两个 unit 会各看到一个 `/tmp`，appID 锁互不可见，"同一 bot 只能跑一个进程"被绕过。所以 `deploy/qqbot.service` 里配了 `RuntimeDirectory=qqbot` + `Environment=QQBOT_LOCK_DIR=/run/qqbot`，把锁放到同机共享、随服务生命周期创建与清理的目录。设了 `QQBOT_LOCK_DIR` 就只用它（不会静默回退到 `/tmp`，否则等于把这个坑埋回去）；手工起进程时记得指向同一个目录。

同一 appID 配两份数据目录仍是双开，由 appID 锁挡住；只有 `[bot].shard_id` 不同的分片部署才是合法的多进程形态。

## `/ping`（仅管理员）

群聊需 @ 机器人后发送 `/ping`，单聊直接发。鉴权由核心完成：`adminping` 在 Manifest 里
声明 `Permission = PermBotAdmin`，非管理员在路由阶段就被拦下，插件根本不会被调用。

**默认完全静默**（连"无权限"都不回），避免在群里暴露该指令与机器信息；权限查询失败
同样按 fail-closed 处理。

```toml
[permission]
# 默认 false：权限不足时不发任何消息
# true：发出插件声明的拒绝语（如"该指令仅管理员可用。"），便于调试权限链路
deny_reply = false
```

```
/pong
运行 2小时13分（启动于 10-17 14:16）｜windows/amd64
CPU  12.3%（8 核）
内存 5.1 GB / 15.6 GB（32.7%）
本进程 48.2 MB（峰值 96.7 MB）
磁盘 可用 120.3 GB / 共 466 GB（已用 74.2%）
目录 /var/lib/qqbot 36.1 MB，312 个文件（不含 .git）
```

整机内存与本进程内存是两行、两个数据源：整机取不到不影响本进程那一行；本进程取不到时该行写成 `本进程 采集失败（原因）`，而不是整行消失。

采集口径：

| 指标 | Windows | Linux |
|---|---|---|
| CPU | `GetSystemTimes` 差值，200 ms 采样窗口，结果缓存 5 s | `/proc/stat` 差值（`idle` 含 `iowait`） |
| 内存 | `GlobalMemoryStatusEx`（TotalPhys / AvailPhys） | `/proc/meminfo`（MemTotal / MemAvailable） |
| 本进程内存 | `GetProcessMemoryInfo` 的 `WorkingSetSize` + `PeakWorkingSetSize` | `/proc/self/status` 的 `VmRSS` + `VmHWM` |
| 分区余量 | `GetDiskFreeSpaceExW` | `statfs` 的 `Bavail` |
| 目录占用 | 遍历当前工作目录，跳过 `.git`、不跟随符号链接/junction，1.5 s 预算 | 同左 |

目录统计有 1.5 秒预算，超出即返回下限并标注"未走完"；`.git` 不计入（非运行时数据且可能极大），命中时回复里会注明。


生产构建（交叉编 linux/amd64）：

```bash
go run ./tools/build all -target linux/amd64 -o bin/qqbot
```

顺序不能反：`plugins` 先产出 `internal/pluginbundle/dist/linux_amd64/`，核心的 `//go:embed` 才编得过。`all` 的 `-o` 只作用于核心——插件必须写进 embed 目录，否则下一步编译直接失败。

> 需要 Go 1.26+：`modernc.org/sqlite v1.60` 的 `go` 指令要求，无需 cgo。实测（Go 1.27.3 + `-s -w`）linux/amd64 核心产物 **32.5 MB**，其中四个内嵌出厂插件共 11.7 MB（各自 2.88–2.99 MB）；windows/amd64 是 33.4 MB。ADR-0005 当初估的是"核心 19 MB → 40 MB"，实际只涨到 32–33 MB：每个插件 4–6 MB 的估计偏高（示例插件实际 ~3 MB），而核心甩掉四个插件的业务代码后本体仍在 20 MB 上下（内嵌字体 6.2 MB 是大头）。

## 频控边界（为什么不把限流全交给平台返回值）

官方只保证两件事：被动回复有次数/时效上限（群 5 分钟 5 次、单聊 60 分钟 4 次），超限返回 HTTP 429（body 为 `err_code` + `message` + `trace_id`）。**没有剩余额度查询接口，也没有承诺 `Retry-After`。** 因此分三层：

| 层 | 位置 | 职责 |
|---|---|---|
| 协议账本（必须本地） | `internal/core/sender.go` | 被动窗口与次数、`msg_id + msg_seq` 原子分配（同一 `msg_id` 下 seq 单调递增，窗口过期只重置次数）；文档明确同一 `msg_id + msg_seq` 重复发送会失败 |
| 接口限流（粗粒度） | `internal/httpx` | 全进程出站令牌桶，可按域名分桶（`[http.domains]`）；发消息走 `PostJSONOnce`，不自动重试；429 **不自动重试**，按 `Retry-After`（秒数/HTTP-date，缺省 60s）冷却域名桶，冷却期内该桶不出网 |
| 返回值校正 | `sender` / `dispatch` | 429 → `ErrRateLimited` 并告警，核心侧对 `(scene, peer)` 记同一窗口，冷却期内不出网；401 → 作废内存 + `bot_state` 的 token，换新 token 重试一次 |
| 业务错误 | `internal/api` | 2xx + `err_code`≠0（access-token 为 `code`）→ `api.BusinessError`，不算已发送、不重试；平台明确拒绝时归还被动占格与主动日预算，网络失败不归还 |

不能只依赖返回值的三个原因：

1. **被动配额用尽 = 消息直接丢失**。第 6 次（群）/第 5 次（单聊）平台才报错，此时已没有补救机会；本地预检可以在发送前改成合并回复。
2. **结果不确定时会重复发送**。超时/5xx 可能已经送达；主动消息没有 `msg_id` 幂等键，自动重试就是重复推送。
3. **429 不带维度**。同一状态码可能是 Bot 维度 qpm、单关系 20 qpm、每日 1000 条或接口 100 QPS，退避时长从 1 分钟到次日不等，只能本地按维度记账。

可以交给返回值的是：接口级 100 QPS 的瞬时超限、平台风控（如 `1100100 消息被限频`），以及用户关闭主动消息这类非频控失败。

## 尚未实现

> 2026-10-18 收敛了整份清单：明确不做的写「不做」，暂不排期的写「搁置」，
> 剩下的才是真的待做。不要把「搁置」当成「下一步」。

- **不做**：入站媒体下载。机器人只处理文本，图片/音视频只记 `has_media` 与 `msg_type`，不下载、不落盘；`[media]` 配置段与 `media` 表保留但未启用。发图走的是另一条路（见「发图片」）；
- **不做**：入群欢迎语。`GROUP_ADD_ROBOT` 载荷没有 `msg_id`，只能走主动消息，不值（§8.5）；
- **不做**：对外 REST 接口。进程不监听任何入站端口，全部为出站调用（§11）；
- **搁置**：分片上传。出站图片只支持 ≤ 5 MB 的 base64 直传，更大的图本地直接拒绝；大图请用 `ImageFromURL`，下载发生在平台侧；
- **在插件仓做**：`plugins/torn` 业务插件与 Torn 限流/缓存分层。核心侧的前置能力（带请求头的 GET、只发一次、按请求放宽超时、按域名分桶、配置解码）已就绪，见 `docs/ARCHITECTURE.md §6` 与插件仓的设计文档；
- **待做**：多分片（`shard_total > 1`）只做了协议字段，未做多连接编排。

`/grant` / `/revoke` 已删除（不是搁置）：功能由 `/admin add|rm` 取代，指令与保留前缀一并清理。

## 目录

```
cmd/bot/          入口（启动 / init / 重启）、装配点
internal/gateway/ WSS 状态机、心跳、resume、事件解码
internal/core/    路由、幂等、频控、调度、GC、认领、/admin 权限、/upgrade 命令面
pkg/host/         插件契约与 Host 能力实现（插件唯一可见的 API）
pkg/rpc/          线协议：长度前缀帧、方法表、错误码表
pkg/rpcsdk/       插件侧入口（rpcsdk.Serve），插件可执行文件的 main 只调它
internal/pluginproc/ 把 plugins/bin/ 下的可执行文件变成 host.Plugin（探测 / spawn / 退避重启 / 三级关闭）
internal/pluginbundle/ 内嵌的出厂插件副本（dist/<os>_<arch>/），只作 init 的释放源
internal/upgrade/ 自更新：检查、下载、校验、落位、pending 标记与快照
internal/sysinfo/ 跨平台资源采集（build tag 分 windows / linux）
internal/store/   schema、迁移、分批 GC、管理员与身份映射
internal/instance/ 单实例文件锁（flock / CreateFile 独占共享）
internal/httpx/   统一出站客户端
internal/api/     QQ 开放平台 REST 封装
internal/config/  配置结构、默认值、校验；示例配置 config.example.toml（init 写出的是它的内嵌副本）
plugins/echo/     示例插件（cmd/echo/main.go 是产物入口）
plugins/adminping/ 管理员 /ping
plugins/bootnotice/ 启动/重启主动私聊通知
plugins/<repo>/   第三方插件：独立 module，放好产物即可，不进核心构建
tools/build/      构建入口（插件 → 核心的固定顺序）
deploy/           systemd 单元
```

运行期数据（`<datadir>` = `[store].path` 的目录）：

```
<datadir>/qqbot.db                  SQLite（含 -wal / -shm）
<datadir>/qqbot.db.lock             数据目录维度的单实例锁
<datadir>/plugins/bin/<name>        已安装的插件产物（在就是装着，删掉就是卸载）
<datadir>/upgrade/state.json        pending 升级标记
<datadir>/upgrade/snapshots/        VACUUM INTO 快照（保留 2 份）
```
