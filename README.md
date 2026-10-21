# qqbot

QQ 官方机器人（群 + 消息列表单聊）的 Go 单二进制骨架，按 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) 落地。

## 已实现（可运行）

| 模块 | 内容 |
|---|---|
| `internal/config` | TOML 配置、默认值、环境变量覆盖、启动校验 |
| `internal/httpx` | 统一出站：https 强制、域名白名单、按域名分桶的令牌桶、重试与全抖动退避 |
| `internal/api` | access_token 缓存（到期前 60 s 刷新，落 `bot_state`）、网关地址、发消息、富媒体上传（`/files`） |
| `internal/gateway` | op10 → Identify → 心跳 → Resume 状态机，断线指数退避 |
| `internal/core` | 事件去重、群准入（白/黑名单）、落库、路由、按 peer 串行、被动回复额度、发图（上传 + `msg_type=7`）、调度器、7 天 GC、`/reload` 自重启、`/reset` 分级重置 |
| `internal/buildinfo` | 版本与 VCS commit 识别（`dev cc908d87+dirty`），供启动日志、`/ping`、`/reload` 对比 |
| `internal/store` | SQLite WAL + 单写连接 + 分批删除 + `incremental_vacuum` |
| `internal/host` | 插件契约与能力实现（KV / 只读 Query / 受白名单约束的 HTTP / 管理员判定 / 收件人目录 / 日志） |
| `internal/logx` | slog 文本/JSON handler；终端里 `WARN` 黄、`ERROR` 红，JSON 永不上色 |
| `internal/termcolor` | 是否该上色的唯一判定：真终端才上色，落盘与 journald 保持纯文本 |
| `internal/sysinfo` | 跨平台资源采集：CPU、内存、进程 RSS、分区容量、目录占用（无 cgo） |
| `plugins/echo` | 最小插件，演示 Manifest、配置读取、`!ping` / `!echo` |
| `plugins/adminping` | 仅管理员可用的 `/ping`：运行时长 + CPU / 内存 / 磁盘 / 目录占用 |
| `plugins/bootnotice` | 启动/重启后主动私聊管理员：上线时间、距上次启动间隔、启用插件清单 |
| `deploy/qqbot.service` | systemd 单元，含 `StartLimitBurst` 与 `MemoryMax` |

认证信息按约定写在**本地 config**（`config/config.local.toml`，已 gitignore），不再依赖 `EnvironmentFile`；仍支持 `QQ_APP_ID` / `QQ_APP_SECRET` 覆盖。

## 快速开始

```bash
cp config/config.example.toml config/config.local.toml
# 填入 [app] id / secret，保持 sandbox = true

go run ./cmd/bot          # 不填 -c 时默认读相对路径 config/config.local.toml
```

默认路径是**相对当前工作目录**的 `config/config.local.toml`；换目录启动或改用 `/etc/qqbot/config.toml` 时用 `-c <路径>` 指定。

私有插件放进 `plugins/<repo>/` 后，跑一次 `go generate ./cmd/bot` 把它编进二进制。生成物 `cmd/bot/plugins_gen.go` 不入库，所以核心仓克隆下来只带示例插件（见 ARCHITECTURE §3.5）。启动日志的 `plugins` 数量后面跟着 `plugin_names`，可以直接看出新插件是否真的链接进来了。

启动日志里会出现：

```
WARN 未检测到管理员，已生成一次性认领令牌 ttl=10m0s token=<64位十六进制>
```

在目标群或单聊发送 `/claim <token>` 即成为 owner（一次性、10 分钟过期）。
之后可用 `/admin add <openid> [备注]` 添加辅助管理员，用 `/ping` 查看运行状态，用 `!ping`、`!echo <文本>` 验证收发链路。

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
启动 10-17 14:56:19｜windows/amd64
上次启动 10-17 12:51（2小时5分前）
启用插件 3 个
· adminping v0.2.0（/ping）
· bootnotice v0.1.0
· echo v0.1.0（!ping、!echo）
```

主动消息在本项目里是**受控通道**，而不是默认放开的能力：

| 闸门 | 位置 | 行为 |
|---|---|---|
| 总开关 | `[active] enabled` | 关闭后所有 `SendActive` 直接返回 `ErrActiveDisabled` |
| 插件白名单 | `[active] plugins` | 只有名单内插件能发；插件拿到的 Sender 是带名字的包装 |
| 每日预算 | `[active] daily_per_peer` | 每 (场景, 收件人) 每天最多 N 条，落库记账（重启不清零、跨日按本地时区换桶），先扣预算再发 HTTP |
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

Go 插件是编译期静态链接的，改动无法热替换；`/reload` 的做法是**重新执行磁盘上的二进制**，用来回答"新构建到底部署上没有"。

这里有一个容易误解的点：`/reload` **不重新编译，也不扫 `plugins/`**。插件靠 `host.Register` 在 `init()` 注册，哪些插件包被链接进来由 `cmd/bot/plugins_gen.go` 的 blank import 决定。所以新克隆一个插件仓库后直接 `/reload`，不会有任何变化——磁盘上的二进制必须先被换掉。

```
/reload              重启
/reload 上线 v0.2     重启，备注写进日志
/reload help         只看用法，不重启
/restart             同 /reload
```

执行顺序（每一步都有原因，不能颠倒）：

1. **先回一条消息**——被动回复窗口只在收到消息时存在，重启后这条就发不出去了；
2. 网关断开并把 `session_id` / `seq` 落盘，下次启动直接 Resume；
3. 关闭 SQLite，避免新进程与旧进程抢写锁；
4. 重新执行自身：
   - Linux / macOS：`execve` 原地替换进程映像，**PID 不变**，systemd 的 cgroup、日志句柄与控制台都沿用原来的；
   - Windows：没有 `execve`，拉起新进程后旧进程退出；此时网关与数据库都已关闭，不会出现两个实例抢同一个 session。

重启后 `bootnotice` 会私聊「机器人已上线（/reload 重启）」，`/ping` 首行显示版本，两边对比就能确认：

```
版本 dev 3f2a1c9d｜go1.26.1
运行 8秒（启动于 10-17 15:40:02）
```

版本串带 `+dirty` 表示构建时工作区有未提交改动——部署的不是干净构建时一眼能看出来。

### 开发期：让新插件随 `/reload` 生效

用 `tools/devrun` 托管进程，它在前台持有终端，循环执行「`go generate` → `go build` → 换二进制 → 拉起」：

```bash
go run ./tools/devrun -c config/config.local.toml
```

此后每次 `/reload` 都会重新扫描 `plugins/`，新放入的插件随之加载。编译失败时它会打印错误并用**旧二进制**继续运行，机器人不会掉线。

原理：bot 在 `QQBOT_SUPERVISOR=1` 下把 `/reload` 从「自己换映像」改为**以退出码 75 退出**，由父进程接手重编译。Windows 没有 `execve`，这一步还顺带解决了「每次重载都把终端丢回提示符」的问题——终端始终由 devrun 占着，日志连续。

生产环境不设置该变量，行为与下一节完全一致。

### 部署更新的完整步骤

```bash
# 1. 生成插件装配并构建（少了 go generate，新放进 plugins/ 的插件不会进二进制）
go generate ./cmd/bot && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o qqbot ./cmd/bot
# 2. 覆盖服务器上的二进制（Linux 允许替换正在运行的文件，不需要先停服务）
scp qqbot server:/opt/qqbot/bin/qqbot.new
ssh server 'mv /opt/qqbot/bin/qqbot.new /opt/qqbot/bin/qqbot'
# 3. 在 QQ 里发
/reload 上线 v0.2
```

约 1 分钟内应收到上线私聊。**没收到就查 journalctl** —— 重启失败时进程已经退出，只能从日志里找原因。

### 几个边界

- `go run ./cmd/bot` 下 `/reload` 只会重启同一份临时构建，**不会重新编译**；回复里会明确提示这一点。开发时请改用 `tools/devrun`（见上一节）。
- `tools/devrun` 只在开发工作区有意义：它需要源码与 Go 工具链。服务器上部署的是二进制，`/reload` 仍是「换文件 + execve」。
- 同一进程只接受一条重启请求，重复发只回「已有重启请求」。
- `/reload` 与 `/reset` 无关：前者重启进程并重读配置，后者按级别清数据（见下一节）。
- 插件不能声明 `/reload` / `/restart` 前缀，否则启动即拒绝加载。

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
- 每次尝试都进 `reset_audit`，包括失败与未确认就过期的那些。

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

## 单实例：同一份数据只能跑一个进程

启动时会在数据库旁边取一把**操作系统级**文件锁（`<store.path>.lock`）。拿不到就直接退出，
不连网关、不开库：

```
启动失败: 另一个实例正在运行：pid 20384（D:\vscode\go\qqbot\bot.exe，启动于 2026-10-18 11:52:07）持有 D:\vscode\go\qqbot\.temp\data\qqbot.db.lock
```

这不是保守限制。两个进程共用一份库会：

- 抢同一个网关 session —— 日志里反复出现 `4902 reset by resume`，两边轮流被踢下线；
- 各回一遍消息：同一个 `msg_id` 被两边各当成一条事件，用户看到重复回复，被动额度翻倍扣。

锁是**内核持有的锁，不是"锁文件存不存在"**。所以 `kill -9`、任务管理器结束、断电重启
都不会留下"需要手动删掉的残留锁"；也正因如此，锁文件本身永不删除（删掉别人正持有的
锁会让第三个进程误以为没人占用）。

| 场景 | 行为 |
|---|---|
| 正常退出 / Ctrl-C | 显式释放，下一个进程立刻能拿 |
| 崩溃 / 强杀 | 句柄随进程销毁，内核释放 |
| `/reload` 拉起的进程 | 旧进程已经先放手，正常一次就能拿到；仍留 15 s 余量 |
| 另一个实例 | 最多等 2 s，然后带着上面的错误退出 |

错误里带 pid 与可执行文件路径，是为了**能精确结束那个进程**而不是让你猜：Windows 在任务
管理器里搜 `qqbot`，Linux 用 `ps -p <pid> -o pid,etime,cmd`。

**不同 `[store].path` 就是不同实例**，可以同时跑。但同一 appID 的多个进程即使库不同也仍会
抢 session——只有 `[bot].shard_id` 不同的分片部署才是合法的多进程形态。

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
内存 5.1 GB / 15.6 GB（32.7%）｜本进程 48.2 MB
磁盘 可用 120.3 GB / 共 466 GB（已用 74.2%）
目录 /opt/qqbot 36.1 MB，312 个文件（不含 .git）
```

采集口径：

| 指标 | Windows | Linux |
|---|---|---|
| CPU | `GetSystemTimes` 差值，200 ms 采样窗口，结果缓存 5 s | `/proc/stat` 差值（`idle` 含 `iowait`） |
| 内存 | `GlobalMemoryStatusEx`（TotalPhys / AvailPhys） | `/proc/meminfo`（MemTotal / MemAvailable） |
| 本进程内存 | `GetProcessMemoryInfo` 的 WorkingSetSize | `/proc/self/statm` 的常驻页数 |
| 分区余量 | `GetDiskFreeSpaceExW` | `statfs` 的 `Bavail` |
| 目录占用 | 遍历当前工作目录，跳过 `.git`、不跟随符号链接/junction，1.5 s 预算 | 同左 |

目录统计有 1.5 秒预算，超出即返回下限并标注"未走完"；`.git` 不计入（非运行时数据且可能极大），命中时回复里会注明。


生产构建：

```bash
go generate ./cmd/bot && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/qqbot ./cmd/bot
```

> 需要 Go 1.26+：`modernc.org/sqlite v1.60` 的 `go` 指令要求。实测 `GOOS=linux` 交叉编译产物约 16 MB（`-s -w` 后更小），无需 cgo。

## 频控边界（为什么不把限流全交给平台返回值）

官方只保证两件事：被动回复有次数/时效上限（群 5 分钟 5 次、单聊 60 分钟 4 次），超限返回 HTTP 429（body 为 `err_code` + `message` + `trace_id`）。**没有剩余额度查询接口，也没有承诺 `Retry-After`。** 因此分三层：

| 层 | 位置 | 职责 |
|---|---|---|
| 协议账本（必须本地） | `internal/core/sender.go` | 被动窗口与次数、`msg_id + msg_seq` 原子分配（同一 `msg_id` 下 seq 单调递增，窗口过期只重置次数）；文档明确同一 `msg_id + msg_seq` 重复发送会失败 |
| 接口限流（粗粒度） | `internal/httpx` | 全进程出站令牌桶，可按域名分桶（`[http.domains]`）；发消息走 `PostJSONOnce`，不自动重试 |
| 返回值校正 | `sender` / `dispatch` | 429 → `ErrRateLimited` 告警；401 → 刷新 token |

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
cmd/bot/          入口与唯一装配点
internal/gateway/ WSS 状态机、心跳、resume、事件解码
internal/core/    路由、幂等、频控、调度、GC、认领、/admin 权限
internal/host/    Host 能力实现（插件唯一可见的 API）
internal/sysinfo/ 跨平台资源采集（build tag 分 windows / linux）
internal/store/   schema、迁移、分批 GC、管理员与身份映射
internal/instance/ 单实例文件锁（flock / CreateFile 独占共享）
internal/httpx/   统一出站客户端
internal/api/     QQ 开放平台 REST 封装
plugins/echo/     示例插件
plugins/adminping/ 管理员 /ping
plugins/bootnotice/ 启动/重启主动私聊通知
plugins/<repo>/   私有插件：独立仓的嵌套克隆，未跟踪（见 ARCHITECTURE §3.5）
config/           配置样例与本地配置（后者不入 git）
deploy/           systemd 单元
```
