# qqbot

QQ 官方机器人（群 + 消息列表单聊）的 Go 单二进制骨架，按 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) 落地。

## 已实现（可运行）

| 模块 | 内容 |
|---|---|
| `internal/config` | TOML 配置、默认值、环境变量覆盖、启动校验 |
| `internal/httpx` | 统一出站：https 强制、域名白名单、令牌桶、重试与全抖动退避 |
| `internal/api` | access_token 缓存（到期前 60 s 刷新，落 `bot_state`）、网关地址、发消息 |
| `internal/gateway` | op10 → Identify → 心跳 → Resume 状态机，断线指数退避 |
| `internal/core` | 事件去重、落库、路由、按 peer 串行、被动回复额度、调度器、7 天 GC |
| `internal/store` | SQLite WAL + 单写连接 + 分批删除 + `incremental_vacuum` |
| `internal/host` | 插件契约与能力实现（KV / 只读 Query / 受白名单约束的 HTTP / 管理员判定 / 日志） |
| `internal/sysinfo` | 跨平台资源采集：CPU、内存、进程 RSS、分区容量、目录占用（无 cgo） |
| `plugins/echo` | 最小插件，演示 Manifest、配置读取、`!ping` / `!echo` |
| `plugins/adminping` | 仅管理员可用的 `/ping`：运行时长 + CPU / 内存 / 磁盘 / 目录占用 |
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
之后可用 `/ping` 查看运行状态，用 `!ping`、`!echo <文本>` 验证收发链路。

## `/ping`（仅管理员）

群聊需 @ 机器人后发送 `/ping`，单聊直接发。**非管理员默认完全静默**（连"无权限"都不回），避免在群里暴露该指令与机器信息；权限后端故障时同样静默并按 fail-closed 处理。

```toml
[plugins.adminping]
# 默认 true：非管理员 / 鉴权失败时不发任何消息
# false：回一句"该指令仅管理员可用。"或"权限查询失败，本次请求已忽略。"，便于调试权限链路
silent = true
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

- 第 8 章分级重置：`/reset R0–R3`、二次确认码、冷却、`reset_audit` 的完整流程（当前仅 `/claim` 可用，其余保留指令返回提示）；
- `GROUP_ADD_ROBOT` / `GROUP_DEL_ROBOT` 的异步清理（§8.5）：事件已解码并入库，尚未接业务动作；
- 媒体下载与配额淘汰（表结构与 GC 已就位，`media.enabled=false` 时只记 `has_media`）；
- `plugins/torn`：业务插件与 Torn 限流/缓存分层；
- 多分片（`shard_total > 1`）只做了协议字段，未做多连接编排。

## 目录

```
cmd/bot/          入口与唯一装配点
internal/gateway/ WSS 状态机、心跳、resume、事件解码
internal/core/    路由、幂等、频控、调度、GC、认领
internal/host/    Host 能力实现（插件唯一可见的 API）
internal/sysinfo/ 跨平台资源采集（build tag 分 windows / linux）
internal/store/   schema、迁移、分批 GC
internal/httpx/   统一出站客户端
internal/api/     QQ 开放平台 REST 封装
plugins/echo/     示例插件
plugins/adminping/ 管理员 /ping
config/           配置样例与本地配置（后者不入 git）
deploy/           systemd 单元
```
