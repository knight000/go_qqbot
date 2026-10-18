# qqbot

QQ 官方机器人（群 + 消息列表单聊）的 Go 单二进制骨架，按 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) 落地。

## 已实现（可运行）

| 模块 | 内容 |
|---|---|
| `internal/config` | TOML 配置、默认值、环境变量覆盖、启动校验 |
| `internal/httpx` | 统一出站：https 强制、域名白名单、令牌桶、重试与全抖动退避 |
| `internal/api` | access_token 缓存（到期前 60 s 刷新，落 `bot_state`）、网关地址、发消息 |
| `internal/gateway` | op10 → Identify → 心跳 → Resume 状态机，断线指数退避 |
| `internal/core` | 事件去重、群准入（白/黑名单）、落库、路由、按 peer 串行、被动回复额度、调度器、7 天 GC、`/reload` 自重启 |
| `internal/buildinfo` | 版本与 VCS commit 识别（`dev cc908d87+dirty`），供启动日志、`/ping`、`/reload` 对比 |
| `internal/store` | SQLite WAL + 单写连接 + 分批删除 + `incremental_vacuum` |
| `internal/host` | 插件契约与能力实现（KV / 只读 Query / 受白名单约束的 HTTP / 管理员判定 / 收件人目录 / 日志） |
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

`-debug` 会顺带把 `log.level` 降到 `debug`，两边细节保持一致。轨迹走 stdout、不加颜色，重定向到文件也能直接读。

### `level = "full"`（默认）

```
[15:24:01.122] 收到 群聊消息 · GROUP_AT_MESSAGE_CREATE
    发送者：EF9A533C3FB062916F7FEF7E39DD532D
    union ：—（平台未下发）
    peer  ：1ABC...
    事件  ：ROBOT1.0_...
    内容  ：!ping
[15:24:01.123] 命中插件 echo v0.1.0 · 命令 "!ping" · 优先级 100
[15:24:01.124] 插件 echo 返回回复（1.20ms，4 字）
[15:24:01.140] 已发送（被动回复 群聊 → 1ABC...） seq=1 · 16ms
    内容  ：pong
```

### `level = "plugin"`

群里绝大多数消息与机器人无关，`full` 会把它们全部打出来。`plugin` 只保留"触发了谁、回了什么"，不输出事件 ID、union、发送者与 peer：

```
[15:55:28.093] 命中插件 echo v0.1.0 · 命令 "!ping"
    内容  ：!ping
[15:55:28.093] 被动回复 群聊 · 16ms
    内容  ：pong
```

该级别下未命中消息、重复事件、原始 JSON 一律静默；核心指令（`/claim`、`/admin`、`/reload`）只留一行指令名。`raw = true` 与 `level = "plugin"` 同时配置会被启动校验拒绝。

四个刻意的取舍：

- **在去重之前打印**（`full`）：重复投递也会留一行 `重复事件，已跳过`，排查"为什么没回复"时不会误判成插件没命中；
- **正文只打一次**：插件返回时只报字数，真正发出去时再打全文，同一条内容不会出现两遍；
- **`raw = true` 才打原始 JSON**：一条事件可能几十行，只在确认平台字段时打开；
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
| 每日预算 | `[active] daily_per_peer` | 每 (场景, 收件人) 每天最多 N 条，先扣预算再发 HTTP |
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

### 部署更新的完整步骤

```bash
# 1. 构建
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o qqbot ./cmd/bot
# 2. 覆盖服务器上的二进制（Linux 允许替换正在运行的文件，不需要先停服务）
scp qqbot server:/opt/qqbot/bin/qqbot.new
ssh server 'mv /opt/qqbot/bin/qqbot.new /opt/qqbot/bin/qqbot'
# 3. 在 QQ 里发
/reload 上线 v0.2
```

约 1 分钟内应收到上线私聊。**没收到就查 journalctl** —— 重启失败时进程已经退出，只能从日志里找原因。

### 几个边界

- `go run ./cmd/bot` 下 `/reload` 只会重启同一份临时构建，**不会重新编译**；回复里会明确提示这一点。
- 同一进程只接受一条重启请求，重复发只回「已有重启请求」。
- `/reload` 与 `/reset` 无关：前者重启进程，后者（分级重置数据）尚未实现。
- 插件不能声明 `/reload` / `/restart` 前缀，否则启动即拒绝加载。

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
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/qqbot ./cmd/bot
```

> 需要 Go 1.26+：`modernc.org/sqlite v1.60` 的 `go` 指令要求。实测 `GOOS=linux` 交叉编译产物约 16 MB（`-s -w` 后更小），无需 cgo。

## 频控边界（为什么不把限流全交给平台返回值）

官方只保证两件事：被动回复有次数/时效上限（群 5 分钟 5 次、单聊 60 分钟 4 次），超限返回 HTTP 429（body 为 `err_code` + `message` + `trace_id`）。**没有剩余额度查询接口，也没有承诺 `Retry-After`。** 因此分三层：

| 层 | 位置 | 职责 |
|---|---|---|
| 协议账本（必须本地） | `internal/core/sender.go` | 被动窗口与次数、`msg_id + msg_seq` 原子分配；文档明确同一 `msg_id + msg_seq` 重复发送会失败 |
| 接口限流（粗粒度） | `internal/httpx` | 全 Bot 出站令牌桶；发消息走 `PostJSONOnce`，不自动重试 |
| 返回值校正 | `sender` / `dispatch` | 429 → `ErrRateLimited` 告警；401 → 刷新 token |

不能只依赖返回值的三个原因：

1. **被动配额用尽 = 消息直接丢失**。第 6 次（群）/第 5 次（单聊）平台才报错，此时已没有补救机会；本地预检可以在发送前改成合并回复。
2. **结果不确定时会重复发送**。超时/5xx 可能已经送达；主动消息没有 `msg_id` 幂等键，自动重试就是重复推送。
3. **429 不带维度**。同一状态码可能是 Bot 维度 qpm、单关系 20 qpm、每日 1000 条或接口 100 QPS，退避时长从 1 分钟到次日不等，只能本地按维度记账。

可以交给返回值的是：接口级 100 QPS 的瞬时超限、平台风控（如 `1100100 消息被限频`），以及用户关闭主动消息这类非频控失败。

## 尚未实现（下一步）

- 第 8 章分级重置：`/reset R0–R3`、二次确认码、冷却、`reset_audit` 的完整流程（`/reset` 仍是占位回复；`/grant`、`/revoke` 已由 `/admin add|rm` 取代）；
- `GROUP_ADD_ROBOT` / `GROUP_DEL_ROBOT` 的异步清理（§8.5）：事件已解码并入库，尚未接业务动作；
- 媒体下载与配额淘汰（表结构与 GC 已就位，`media.enabled=false` 时只记 `has_media`）；
- `plugins/torn`：业务插件与 Torn 限流/缓存分层；
- 多分片（`shard_total > 1`）只做了协议字段，未做多连接编排。
- 单实例保护：两个进程共用同一个库时会互相 `4902 reset by resume`，当前靠部署约束（systemd 单实例），未做文件锁。

## 目录

```
cmd/bot/          入口与唯一装配点
internal/gateway/ WSS 状态机、心跳、resume、事件解码
internal/core/    路由、幂等、频控、调度、GC、认领、/admin 权限
internal/host/    Host 能力实现（插件唯一可见的 API）
internal/sysinfo/ 跨平台资源采集（build tag 分 windows / linux）
internal/store/   schema、迁移、分批 GC、管理员与身份映射
internal/httpx/   统一出站客户端
internal/api/     QQ 开放平台 REST 封装
plugins/echo/     示例插件
plugins/adminping/ 管理员 /ping
plugins/bootnotice/ 启动/重启主动私聊通知
config/           配置样例与本地配置（后者不入 git）
deploy/           systemd 单元
```
