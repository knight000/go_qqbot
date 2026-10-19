# QQ 官方机器人架构设计（低配 Debian + 插件化）

> 状态：**v1.0 定稿**（技术栈与平台前提已锁定）。仍为设计文档，不含实现代码。
> 目标环境：Debian 12，1 vCPU / 1 GB RAM（若为 2 GB，第 1.3 节结论放宽）。
> 开发环境：本机 Windows，连官方沙箱。

## 0. 决策记录

| 编号 | 决策 | 结论 | 影响 |
|---|---|---|---|
| D1 | 语言栈 | **Go** | 单二进制、内存 15–40 MB、交叉编译直投服务器 |
| D2 | 插件形态 | **P1 编译期注册** | 无 IPC、无版本错配；加插件需重编译（5–10 s） |
| D3 | 平台场景 | **个人主体已可开发 QQ 群 + 消息列表单聊** | 群/单聊为主战场；频道作为可选扩展 |
| D4 | 出站 API | Torn 为机器人主动调用（非对外供接口） | 无需入站端口、无需反代与备案域名 |
| D5 | 管理员重置 | 对话指令触发，**自建鉴权**（平台只给群内身份，不给 Bot 运维身份） | 需 `admins` 表 + 认领机制 + 二次确认 |
| D6 | 群准入 | **白名单 / 黑名单两套独立开关，默认都开**；Bot 角色为全局角色（无群内委托） | 新增 `qq_groups` 表 + `/group` 指令；未授权群在去重前丢弃 |

D1/D2 是耦合决策：Go 缺少可靠的运行时动态插件机制（`.so` 插件要求工具链与依赖版本完全一致），因此选择 Go 即等于选择 P1。

D5 的前提有两层，必须分清：
- **平台确实给了群内身份**：群消息事件的 `author.member_role` 直接是 `member / admin / owner`，不需要额外接口（§8.1）；
- **但群内身份不等于 Bot 运维身份**：一个群主能管好这个群，不代表有权重启机器人、清库、看主机指标。因此“谁有权管理 Bot”仍由本系统的三级角色认定（§8.7），平台的群角色只用于“群管级”权限点。

---

## 1. 需求与约束

### 1.1 功能需求

| 编号 | 需求 | 设计含义 |
|---|---|---|
| R1 | 机器人**主动调用 Torn API** 查询数据 | 出站 HTTP，不需要入站端口；必须有缓存与令牌桶 |
| R2 | 消息记录**仅保留 7 天** | 定时 GC + 索引设计 + 分批删除 |
| R3 | 图片**不接收**（2026-10-18 定案：机器人只处理文本） | 只记 `has_media=1`，不下载、不落盘；`media` 表与 `[media]` 配置保留未启用 |
| R4 | 音视频**不接收** | 按 `msg_type` 在入口直接忽略，不落库 |
| R5 | 可能有**定时任务** | 库表驱动的调度器，不依赖外部 cron |
| R6 | 每个部件**插件化**，主体与功能插件分离 | 核心层只做接入/存储/调度，业务全在插件 |
| R7 | **管理员可主动重置机器人** | 需管理员认定 + 分级重置语义 + 二次确认 + 审计（见第 8 章） |
| R8 | **群准入可控**：未授权群不得进入机器人 | 群注册表 + 白/黑名单两套独立开关 + `/group` 管理指令（§8.8） |
| R9 | **群生命周期可见**：机器人进群/退群要有人知道 | 入群/退群通知私聊提醒 + 在群标记 + 群名批量刷新（§8.8） |
| R10 | 机器人**要能发图片** | 富媒体上传（`/files`）换 `file_info` → `msg_type=7` 发送；图文拆两条（§4.3） |

### 1.2 平台硬约束（已核实官方文档）

| 约束 | 事实 | 后果 |
|---|---|---|
| 鉴权 | `POST https://bots.qq.com/app/getAppAccessToken`，`{appId, clientSecret}` → `access_token` / `expires_in: 7200`；请求头 `Authorization: QQBot {token}` | 必须缓存，到期前 60 s 刷新；旧 `Bot appid.token` 已废弃 |
| 接入 | v2 仅 WebSocket 长连接：`/gateway/bot` → op10 → op2 Identify → op1 心跳 → op6 Resume | 纯出站；服务器零入站端口 |
| 环境 | 沙箱 `https://sandbox.api.sgroup.qq.com`；**IP 白名单仅作用于正式环境** | 本机可开发，动态 IP 无影响；上线需服务器固定 IP |
| 会话配额 | `session_start_limit: total 1000 / reset 86400000 / max_concurrency 1` | 重启风暴会耗尽日配额 → 必须退避重启 + 持久化 session |
| 被动回复 | 群：5 分钟内、最多 5 次；单聊：60 分钟内、最多 4 次 | 回复必须快；超时即失败，不能重试硬撑 |
| 主动消息 | 群：未认证 30 qpm / 认证 60 qpm；单聊：未认证 5 qps+30 qpm；单关系 20 qpm、每群/每用户每天 1000 条 | **默认全关**，仅白名单场景开启 |
| 富媒体 | 先 `POST /v2/{groups\|users}/{openid}/files` 换 `file_info`，再以 `msg_type=7` + `media.file_info` 发送；`file_data` 是 base64 且 ≤ 5 MB；图片软限 20 MB、硬限 200 MB；单聊与群聊的上传文件**互不通用** | 一次发图 = 两次 HTTP；超 5 MB 必须分片上传（未实现）；图文无法合并成一条消息 |

### 1.3 资源预算

| 项 | 预算 |
|---|---|
| 内存（核心） | 15–40 MB |
| 内存（每插件，同进程） | +2–8 MB |
| 二进制磁盘 | 19 MB 量级（实测：windows 19.22 MB / linux 19.03 MB，其中内嵌字体 6.2 MB） |
| SQLite（1000–10000 条/天，留 7 天） | 5–40 MB |
| 媒体配额 | 0 —— 入站不下载；出站图片存在平台侧，不占本地盘 |
| 日志（journald 限额） | 200 MB |
| 结论 | 20 GB 盘余量充裕；1 GB 内存下与 TS3/WebDAV 共存可行，但需 `MemoryMax` 约束 |

### 1.4 事件模型（三轴）

平台事件统一由三个字段描述，插件只认这三个维度：

| 轴 | 字段 | 取值 | 决定什么 |
|---|---|---|---|
| 大类 | `Kind` | `message` / `notice` / `request` / `meta` / `unknown` | 是否落库、能否进入插件路由 |
| 场景 | `Scene` | `group` / `c2c` / `guild` | `PeerID` 的含义、回复走哪个端点 |
| 平台事件名 | `Name` | 如 `GROUP_AT_MESSAGE_CREATE` | 只用于观测与解码表索引，插件不订阅它 |

- 本地解码覆盖全部 12 个群与单聊事件（2 个群消息 + 1 个单聊消息 + 9 个通知/请求），另有 4 个跨场景事件：`FRIEND_ADD`、`FRIEND_DEL`、`INTERACTION_CREATE`、`SUBSCRIBE_MESSAGE_STATUS`。
- `GROUP_AT_MESSAGE_CREATE`（@机器人）与 `GROUP_MESSAGE_CREATE`（全量模式：群主开启「接收所有消息」后群里每条消息都推）字段结构一致，**归一化为同一类 `message` 事件**，下游不区分。
- 解码表里没有的事件归为 `Kind=unknown`：进调试轨迹、可观测，**永不进入插件路由**。
- 订阅语法：`Manifest.Intents` 写 `<kind>` 或 `<scene>.<kind>`，例如 `message`、`group.message`；写错在装配阶段即拒绝加载。

决策背景见 `docs/adr/0001-event-model.md`。


---

## 2. 总体架构

```
                    ┌──────────────────────── 出站（无需入站端口） ────────────────────────┐
                    │                                                                     │
   QQ 开放平台 ◄────┼──── WSS 长连接（op 状态机）        HTTPS ──► api.sgroup.qq.com      │
   （沙箱/正式）     │                                        └──► bots.qq.com（token）    │
                    │                                        └──► api.torn.com（业务）    │
                    └─────────────────────────────────────────────────────────────────────┘
                                          ▲                    │
┌─────────────────────────────────────────┴────────────────────┴──────────────────────────┐
│  接入层  gateway/        连接管理 · opcode 状态机 · 心跳 · resume · 分片 · 事件解码        │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│  核心层  core/           ① 幂等去重  ② 消息落库  ③ 事件路由  ④ 频控/发送  ⑤ 调度器      │
│                          ⑥ 媒体策略  ⑦ 留存 GC   ⑧ 统一 HTTP 客户端（重试/退避/白名单）  │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│  插件层  plugins/*       每个插件只拿到 Host 暴露的能力，不直连 DB / 不直连 WS             │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│  存储层  store/          SQLite（WAL）+ 媒体目录（可关闭）+ journald                       │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

**关键分工原则**：核心层**不含任何业务词**（不出现 "torn"、"天气"、"签到"）。核心只负责“把消息安全地送到插件手里，把插件的回复安全地送出去，并管好数据生命周期”。这条原则是插件化能否真正落地的前提——一旦核心层出现 if (plugin == "xxx")，后续复用即失效。

### 2.1 进程与并发模型

- **单进程**，三组长期协程：`ws-loop`、`dispatch-pool`、`scheduler-loop`；外加一个周期性 `gc-loop`。
- **按 peer 串行**：同一 `peer_openid` 的事件进入同一队列，串行处理。理由有两个——① `msg_seq` 必须原子递增，否则并发回复同一 `msg_id` 会撞车；② 避免插件对同一用户并发查询。
- **跨 peer 并行**：工作池上限默认 16，超限排队而非丢弃。
- 全程无阻塞 IO 占用工作池：所有 HTTP 带 `context` 超时（默认 5 s）。
- **单实例由文件锁保证**，不是靠部署纪律。锁文件 `<store.path>.lock` 用操作系统
  级锁持有（Unix `flock` / Windows `CreateFile` 独占共享模式），进程结束由内核
  释放，崩溃或 `kill -9` 都不留残留，因此锁文件**永不删除**（删掉别人正持有的
  锁会引入两个实例同时认为自己独占的竞态）。第二个进程默认最多等 2 s 就退出，
  错误里带持有者的 pid、可执行文件路径与启动时间；`/reload` 拉起的进程等 15 s
  （旧进程已先放手，只是保险），因为它是交接而不是竞争。锁按**数据目录**划分——
  不同 `[store].path` 是两个独立
  实例，可以同时跑。但这不等于同一 appID 可以多进程：网关 session 是按
  appID + 分片分配的，同分片两个进程仍会互顶（`4902 reset by resume`），
  只有 `[bot].shard_id` 不同的分片部署是合法的多进程形态。

### 2.2 三条数据流

| 流 | 路径 |
|---|---|
| 入站 | WSS 事件 → 解码 → 去重 → 落库 → 路由 → 插件 `Handle` → 回复出站 |
| 定时 | scheduler 扫描 → 插件 `Job` → 回复/写库（不经过事件路由） |
| 出站 | 统一 `Sender` → 频控令牌桶 → HTTP → 429/限流退避 → 结果回填 |

---

## 3. 插件化设计

### 3.1 三种插件形态（已定稿：P1）

| 形态 | 加载方式 | 隔离 | 加插件成本 | 每插件内存 | 适用 |
|---|---|---|---|---|---|
| **P1 编译期注册** | 每个插件是一个 package，`main` 里 blank import 注册 | 无（同进程） | 重编译，Go 约 5–10 s | +2–8 MB | ✅ 自用、插件可信、追求低配 |
| **P2 同进程动态加载** | Python `entry_points` / Node 动态 `import` | 无 | 免重编译 | Python 基线 90–150 MB 起 | 需要热插拔、写插件的人多 |
| **P3 子进程 + JSON-RPC** | 核心 spawn 子进程，stdin/stdout 行分隔 JSON | 强（崩溃互不影响） | 免重编译、可独立升级 | +5–15 MB/进程 | 第三方/不可信插件、多语言 |

**已定稿：P1（Go 编译期注册）**——见决策记录 D1/D2。取舍理由保留如下，供日后复盘：

1. 无 IPC、无序列化开销、无版本错配，是所有形态里**故障面最小**的；本场景最怕的是“半夜挂了没人管”，不是“加插件慢 8 秒”。
2. 复用性并不因此受损：插件是独立 package，其他机器人项目 `import` 同一模块即可，复用在源码层而非二进制层。
3. P3 的隔离收益在“插件由他人编写”时才成立。若后续真出现第三方插件，P3 可**增量叠加**（核心的 Host 接口正好是进程边界上的契约），不需要推翻重来。

> 唯一硬伤：同进程插件 panic/死循环/内存泄漏会波及核心。对策见 3.6，但只能兜底，不能根治——这是选择 P1 时明确接受的代价。

若最终选择 Python 栈，则答案是 **P2**：`entry_points` 是 Python 生态的标准做法，热加载几乎免费，代价是内存基线高一个数量级。**插件形态的选择本质上被语言栈决定**，这两件事要一起定。

### 3.2 插件契约

```text
Manifest {
  name         唯一标识，同时是 KV 命名空间
  version      semver
  api_version  与核心的契约版本，不匹配则拒绝加载
  capabilities 申请的能力清单（见 3.3）
  intents      需要订阅的事件类型
  commands     匹配规则（前缀 / 正则 / @提及），含 priority
  broadcast    是否与同优先级插件共同处理（默认 false，首个命中即止）
  config_schema 配置项声明，用于启动时校验
}

Lifecycle:  Init(ctx, Host) → Start(ctx) → [Handle(ctx, Event) | Job(ctx, JobSpec)]* → Close(ctx)
```

`Manifest` 采用声明式而非“插件自己决定是否处理”，目的：**路由规则可在启动时静态校验**（例如两条同优先级正则同时匹配，启动即报错，而不是线上抢答）。

### 3.3 Host 能力（唯一对外通道）

| 能力 | 签名要点 | 门控 |
|---|---|---|
| `Sender.Reply` | 必须带原事件 ID，核心自动分配 `msg_seq`；`Reply.Image` 非空时先上传再发 `msg_type=7` | 走频控；图文拆分后**消耗两格**被动配额；超出被动窗口直接失败并告警 |
| `Sender.SendActive` | 主动消息 | **默认禁用**，需在配置中按 (插件, 场景) 白名单开启 |
| `KV` / `PeerKV(peer)` | 按插件命名空间隔离的 KV，支持 TTL；`PeerKV` 多一层会话维度 | 只能读写自己的命名空间；`/reset session` 只清 `PeerKV` 写入的部分 |
| `Store.Query` | 只读、参数化、行数上限 1000 | 不允许 DDL/写操作 |
| `HTTP.GetJSON` / `PostJSON` | 域名白名单 + 超时 + 重试 + 自动降级 | 未声明域名一律拒绝 |
| `Scheduler.Register` | 注册 Job（cron 表达式 + 超时；`PerPeer` 时按在群展开） | 仅启动期可注册 |
| `Log` | 结构化日志，自动带 plugin 字段 | 敏感字段脱敏 |
| `Renderer` | 把 markdown 画成图片（`Markdown(ctx, src) (Image, error)`），契约见 §3.8 | 未注入时返回拒绝性 stub（错误可被 `errors.Is` 分类为超时/超大/能力缺失），不是 nil |

**插件不直接持有 SQLite 连接、不直接持有 WS 连接、不读环境变量取密钥。** 这条边界决定了：插件可单测（Host 用 fake）、可复用（换核心只改 Host 适配层）、可审计（能力清单即权限清单）。

### 3.4 事件路由

```
事件 → 去重（event_id）→ 落库 → 按 intents 过滤 → 候选插件按 priority 排序
     → 依次匹配 commands → 首个命中且 broadcast=false 的处理并返回
     → 全未命中 → 交核心 fallback（默认静默，可配置默认提示）
```

- 路由在**插件的 matcher 表**上做，核心不解析业务语义。
- 单条消息只允许一个插件回复（除显式 `broadcast: true`），从根本上避免“两个插件同时回、消耗 5 次被动额度”的问题。
- **核心保留前缀**：`/claim`、`/admin`、`/group`、`/reset`、`/reload`、`/restart` 由核心以最高优先级独占处理，插件在启动期若声明了相同前缀，**直接拒绝加载并报错**。否则一个第三方插件就能劫持重置指令，等于把第 8 章的鉴权整个绕过去。清单必须与 `handleReserved` 的 case 严格对应——多列一个会白占名字，少列一个会让核心静默遮蔽插件声明的同名命令。`/grant`、`/revoke` 的功能已由 `/admin add|rm` 取代，指令删除后名字一并释放。

### 3.5 插件目录与复用方式

```
qqbot/
  cmd/bot/main.go              入口，集中注册插件（P1 的唯一“装配点”）
  internal/gateway/            WSS 状态机、心跳、resume、分片
  internal/core/               路由、幂等、频控、调度、媒体、GC
  internal/host/               Host 接口实现（插件唯一可见的 API）
  internal/store/              schema、迁移、GC
  internal/httpx/              统一出站客户端
  plugins/qqbot-plugin-torn/   插件：Torn 查询（独立仓的嵌套克隆，未跟踪）
  plugins/echo/                插件：最小示例，兼作契约回归用例
  config/config.toml           全局配置，插件配置在 [plugins.<name>]
  deploy/qqbot.service
  docs/ARCHITECTURE.md
```

**Go 构建不拆分**：私有插件不是独立 module，也不是 submodule——它是放在 `plugins/<repo>/` 下的一个**嵌套克隆**，被 `.gitignore` 忽略，随核心模块一起编译。

**双工作区**：

```
go_qqbot/                       核心工作区：只带示例插件，git pull 即更新
qqbot-plugin-torn/              插件工作区：核心仓的克隆
  plugins/qqbot-plugin-torn/    ← 插件仓（自己的 .git），commit/push 只影响它自己
  cmd/bot/private_plugins.go    ← 未跟踪：把插件编进二进制的唯一开关
```

- **核心更新**：两个工作区各自 `git pull`。插件目录被忽略，外层 pull 不会与插件的本地提交冲突，也不需要 bump 指针。
- **插件更新**：进入 `plugins/<repo>/` 提交推送，不产生任何核心仓提交。
- **编进二进制**：`cmd/bot/private_plugins.go` 里一行 blank import（`_ "github.com/knight000/go_qqbot/plugins/<repo>"`）。该文件不入库，所以核心仓永远只引用示例插件，克隆即可构建。
- **插件目录不能有 `go.mod`**：嵌套模块会让 `go build ./...` 静默跳过它。要单独跑测试就在工作区里 `go test ./plugins/<repo>/...`。

推论：同一个核心提交在不同工作区可能产出不同二进制（取决于该工作区有没有 `private_plugins.go`）。这是「核心不引用业务插件」的代价，换来核心仓克隆即可构建、插件仓有独立提交历史。

> 为什么不拆成独立 module：`internal/host` 按 Go 规则只能被 `github.com/knight000/go_qqbot/` 路径下的包 import，另一个模块路径必然编译失败；把契约移出 `internal/` 是另一件事，当前不做。

⚠️ `git clean -xdf` 会连同插件目录里的 `.git` 一起删掉——插件仓有未推送提交时不要跑。

### 3.6 隔离与超时

| 措施 | 参数 |
|---|---|
| panic 捕获 | 每个 `Handle`/`Job` 包 `recover`，记堆栈，回复降级文案 |
| 超时 | `Handle` 默认 3 s（硬上限 8 s）；`Job` 默认 60 s |
| 连续失败熔断 | 同一插件连续失败 5 次 → 自动禁用该插件并告警，**不影响其它插件** |
| 内存 | 进程级 `MemoryMax=256M`；插件级无法单独限制（P1 的固有短板） |

### 3.7 依赖清单（Go，D1/D2 定稿后）

设计取向（软约束，**不设条目上限**）：判据是**依赖传递链有多宽**，不是"这是第几个依赖"。
Go 项目的长期维护成本几乎全部来自依赖链，而不是自己的代码，所以零传递依赖的叶子模块
（`goldmark`）与只挂一个间接依赖的官方扩展库（`golang.org/x/image` → `x/text`）可以接受；
`viper` 那类会把依赖树放大一个数量级的仍然不引。

| 用途 | 选型 | 理由 |
|---|---|---|
| WSS 客户端 | `coder/websocket` | 原生支持 `context` 取消，重连/超时逻辑可统一；`gorilla/websocket` 生态更大但需自己包一层取消机制 |
| SQLite | `modernc.org/sqlite` | 纯 Go、免 cgo，Windows 交叉编译 Linux 产物一条命令搞定；`mattn/go-sqlite3` 的 cgo 交叉编译是公认的坑 |
| cron 解析 | `robfig/cron/v3` | 仅用于解析表达式算 `next_run_at`，调度仍由自研 30 s 扫描驱动 |
| 配置 | `BurntSushi/toml` | 轻量。**不引 viper**——它会把依赖树放大一个数量级，与本项目的低配目标冲突 |
| 日志 | 标准库 `log/slog` | 零依赖，自带 JSON handler，直接对接 journald |
| Markdown 解析 | `yuin/goldmark` | 纯 Go、GFM 语义完整（表格/任务列表/删除线）；`go.mod` 只有两行、零 require，是一棵没有分叉的叶子 |
| 字体光栅 | `golang.org/x/image` | TrueType/OpenType 解析、字形光栅与 `font.Drawer` 都在这里，自写光栅器不现实；间接引入 `x/text` |

后两项服务于**渲染**能力（见 §3.8），已随该能力落地进入 `go.mod`：解析在 `internal/render`，
契约（接口与哨兵）在 `internal/host/render.go`。

不上依赖的部分：HTTP 用标准库 `net/http`（连接复用、超时、退避自己写，约 100 行）；测试用标准 `testing` + fake Host，不引 testify。目标 Go 版本 1.26+（`slog` 与循环变量语义修复；`modernc.org/sqlite v1.60` 要求 1.26）。

---

### 3.8 渲染能力（定稿 2026-10-18，ADR-0002）

**定位**：核心提供的通用原语——把一段**文档源**画成**图片**。没有配置开关、没有插件白名单：
用不用由插件代码决定。渲染失败以错误返回调用方，核心不做降级——降级方式（发原文、拆图、放弃）
属于插件的内容判断，核心不猜。

**接口**（`host.Renderer`，经 `Host.Renderer()` 获取）：

```go
type Renderer interface {
	Markdown(ctx context.Context, src string) (Image, error)
}
```

只有一个入口：纯文本就是"没有任何标记的 markdown"。不设 `Text` 方法、不设样式参数——
将来要扩就加 `MarkdownWith(ctx, src, Style)`，不破坏既有签名。

**管道**：`goldmark`（GFM）→ AST → 自研布局器 → `x/image/font/opentype` 光栅 → PNG → `ImageFromBytes`。
真正的工作量在布局器：行内折行（含 CJK 禁则）、`margin collapsing`（相邻块取最大而不是相加）、
按 `font.Metrics` 的 ascent/descent 定位基线、表格两趟列宽分配。

**视觉基线**：GitHub markdown 的 **2× 换算**。颜色取 Primer light token
（`#f6f8fa` 代码块底、`#d0d7de` 表格与分隔线、`#d8dee4` 标题下划线、`#0969da` 链接、
`rgba(175,184,193,.2)` 行内代码底）。字号阶梯：正文 32px / 行高 48px、H1 64px、H2 48px、
H3 40px、代码 27px / 行高 39px。完整数值落在 `internal/render/style.go`，本文不复制一份以免漂移。

**尺寸与并发**：宽 1080 固定（"统一"的前提是所有图同宽）、高上限 4000px、渲染并发 2。
账：1080×4000 的 RGBA = 17.3 MB，2 并发约 35 MB，加编码缓冲约 50 MB，与 15–40 MB 的基线相加
仍在 `MemoryMax=256M` 之内；不设限时 16 个 worker 峰值 277 MB，会被内核直接杀掉。

**字体**：`go:embed` 内嵌 `Noto Sans`（拉丁）、`Noto Sans SC`（中文，GB2312 6763 字子集）、
`Noto Sans Mono`（代码），均取 Regular + Bold 的**静态实例**，随附 OFL 许可文本。
可变字体用不了——`opentype` 没有 variation axis。

**错误语义**：`host.ErrRenderTooLarge`（超 4000px，`host.RenderTooLargeError` 附带实际所需高度，
插件可据此拆图）、`host.ErrRenderTimeout`（ctx 到期或在等并发槽时被取消）、
其余错误原样包装返回。两个哨兵定义在 `host` 而不是实现包——插件只 import `host`，
放实现包里插件的 `errors.Is` 就拿不到。未注入实现时 `Host.Renderer()` 返回**拒绝性 stub**
（永不 nil），且其错误刻意不命中上述哨兵：环境没装配要与内容有问题可区分。
缺字**不是错误**：画方框 + warn 日志。

**明确不做**：HTML（需要浏览器内核）、图片 / 内嵌 HTML / 公式 / 脚注 / 语法高亮 / 彩色 emoji
（未识别语法按纯文本原样输出，不报错）、缓存、页脚时间戳、深色主题、自动分页。

**已知妥协**：代码块内出现中文时列不对齐——等宽与全角不同族，`1 汉字 ≠ 2 半角`。

---

## 4. 存储设计

### 4.1 表结构

```sql
-- 断点续连与凭证缓存（重启不必 Identify，省 session 配额）
CREATE TABLE bot_state (
  k          TEXT PRIMARY KEY,          -- access_token / access_token_exp / session_id / seq / shard
  v          TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 消息（留存 7 天，R2）
CREATE TABLE messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id      TEXT NOT NULL UNIQUE,
  msg_id        TEXT,
  scene         TEXT NOT NULL,          -- group | c2c | guild
  peer_openid   TEXT NOT NULL,
  sender_openid TEXT,
  msg_type      INTEGER NOT NULL,       -- 0 text / 2 markdown / 7 media
  content       TEXT,                   -- 截断 4 KB，防超长文本撑库
  has_media     INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_messages_created ON messages(created_at);
CREATE INDEX idx_messages_peer    ON messages(peer_openid, created_at);

-- 事件幂等（resume 会重放，3 天足够覆盖）
CREATE TABLE processed_events (
  event_id    TEXT PRIMARY KEY,
  received_at INTEGER NOT NULL
);
CREATE INDEX idx_pe_received ON processed_events(received_at);

-- 媒体（保留未启用：入站不下载，见 §4.3；表结构留作备查，实际不写入）
CREATE TABLE media (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id     INTEGER,
  sha256         TEXT NOT NULL,
  kind           TEXT NOT NULL,         -- image | file（audio/video 不入库）
  path           TEXT NOT NULL,
  bytes          INTEGER NOT NULL,
  created_at     INTEGER NOT NULL,
  last_access_at INTEGER
);
CREATE UNIQUE INDEX idx_media_sha     ON media(sha256);
CREATE INDEX        idx_media_created ON media(created_at);

-- 插件 KV（同时充当 Torn 查询缓存，带 TTL）
-- peer='' 是插件级全局键（跨会话共享）；peer 非空是该会话私有的键。
-- 分两列而不是靠"键名前缀约定"，是因为约定无法强制：插件漏写一次前缀，
-- R1 就会静默漏清，而这类问题在低量使用里几乎不会自己暴露。
CREATE TABLE plugin_kv (
  plugin     TEXT NOT NULL,
  peer       TEXT NOT NULL DEFAULT '',
  k          TEXT NOT NULL,
  v          BLOB NOT NULL,
  expires_at INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (plugin, peer, k)
);

-- 管理员（Bot 角色：平台群角色只描述群内身份，不参与这里的授权）
-- 角色是全局的：openid 跨场景同值，群里加与私聊加效果一致
-- source_scene 只记录来源，不参与判定
CREATE TABLE admins (
  openid       TEXT PRIMARY KEY,
  union_openid TEXT NOT NULL DEFAULT '',   -- 平台可能不下发；仅作兜底匹配
  role         TEXT NOT NULL,              -- owner | admin
  note         TEXT NOT NULL DEFAULT '',
  granted_by   TEXT,
  source_scene TEXT NOT NULL DEFAULT '',   -- group | c2c（来源记录）
  created_at   INTEGER NOT NULL
);

-- 群注册表 + 黑/白名单。
-- 平台没有"列出机器人加入的群"的接口，只能靠收到群事件时登记；
-- group_name 可能为空（群信息接口 11253 需内邀），由 /group name 手工补。
CREATE TABLE qq_groups (
  group_openid  TEXT PRIMARY KEY,
  group_name    TEXT NOT NULL DEFAULT '',
  name_source   TEXT NOT NULL DEFAULT '',  -- api | manual | ''
  mode          TEXT NOT NULL DEFAULT '',  -- ''=未设置 / allow / deny
  left_at       INTEGER NOT NULL DEFAULT 0,-- 0=在群；>0=退出时间（只标记不删行）
  first_seen_at INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_qq_groups_mode ON qq_groups(mode, last_seen_at);

-- 重置审计（不可省略：重置是不可逆操作）
-- status: pending（等确认码）/ done / cancelled（被同目标的新请求顶掉）/ expired
-- peer='*' 表示全局操作（R0 / R3），不属于任何会话
CREATE TABLE reset_audit (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  actor       TEXT NOT NULL,
  peer        TEXT NOT NULL DEFAULT '',
  level       TEXT NOT NULL,               -- reconnect | session | data | all
  status      TEXT NOT NULL DEFAULT 'done',
  code        TEXT NOT NULL DEFAULT '',    -- 6 位确认码，仅 pending 有效
  detail      TEXT,                        -- 执行摘要（清了多少行）
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL DEFAULT 0,  -- 确认码过期时间
  finished_at INTEGER NOT NULL DEFAULT 0   -- 冷却以它为起点
);
CREATE INDEX idx_reset_audit_ts ON reset_audit(created_at);

-- 定时任务（R5）
-- peer='' 是全局任务；peer=<group_openid> 是该群的 per-peer 实例（§5.1）
CREATE TABLE jobs (
  id                   TEXT NOT NULL,      -- <plugin>:<name>，不含 peer
  peer                 TEXT NOT NULL DEFAULT '',
  plugin               TEXT NOT NULL,
  spec                 TEXT NOT NULL,      -- cron
  timezone             TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  enabled              INTEGER NOT NULL DEFAULT 1,
  next_run_at          INTEGER NOT NULL,
  last_run_at          INTEGER,
  last_status          TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  payload              TEXT,
  PRIMARY KEY (id, peer)
);
CREATE INDEX idx_jobs_next ON jobs(enabled, next_run_at);

-- peer 让 R2 能清掉「某个群的执行历史」；只靠 job_id join jobs 不够，
-- 因为 jobs 的实例行将来可能被清理，而历史先于它消失才是对的。
CREATE TABLE job_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL,
  peer        TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  status      TEXT,                        -- ok | error | timeout | skipped
  error       TEXT
);
CREATE INDEX idx_job_runs      ON job_runs(job_id, started_at);
CREATE INDEX idx_job_runs_peer ON job_runs(peer, started_at);
```

### 4.2 7 天留存的实现要点

- **分批删**，不要一次 `DELETE`：每 10 分钟执行 `DELETE FROM messages WHERE created_at < ? LIMIT 5000` 循环至影响行数为 0。理由：单条大事务会长时间持有写锁，与 aria2 的磁盘 IO 叠加时造成 `database is locked`。
- **保留期是配置项**，默认 `168h`；`processed_events` 默认 `72h`（只需覆盖 resume 窗口）。
- `job_runs` 同样纳入 GC（默认 7 天），否则它是最容易无声膨胀的表。
- 删除后用 `PRAGMA incremental_vacuum`（配合 `auto_vacuum=INCREMENTAL`）回收，避免每月一次全库 `VACUUM` 造成分钟级卡顿。

### 4.3 媒体策略（R3 / R4 / R10）

**入站一律不做，出站只发图片**（2026-10-18 定案）。机器人只处理文本：
媒体不下载、不入库、不占磁盘；需要发图时走富媒体上传，文件存在平台侧。

| 方向 | 行为 |
|---|---|
| 入站图片 | 只记 `has_media=1`，**不下载**；事件里的 URL 不入库（带签名，留存无意义且占空间） |
| 入站音视频 | 入口按 `msg_type` 直接忽略，不建行、不下载 |
| `media` 表与 `[media]` 配置 | **保留但未启用**：入站不下载，所以表里永远是空的；`enabled=true` 当前不产生任何效果 |
| 出站图片 | `POST /v2/{groups\|users}/{openid}/files` 换 `file_info` → 以 `msg_type=7` + `media.file_info` 发送 |
| 出站来源 | 本地文件 / 内存字节 / 公网 URL：前两者 base64 直传（≤ 5 MB），URL 由平台自己下载转存 |
| 本地占用 | 0 —— 出站图片不落盘、不入库 |

为什么不删掉这张死表：`DROP TABLE` 要写一次迁移，将来若真接回下载还得再建一次；
留着只多一次空表 GC（§4.2）。代价可以接受，前提是**注释与文档都写明"不写入"**——
一个看起来能开、开了什么都不发生的开关，比一张空表更容易误导人。

发图的三个平台约束直接决定了实现形态，见 §3.3 的 `Sender.Reply` 与 README「发图片」：

1. **两段式**：上传换 `file_info`，再发消息。上传失败不该消耗被动回复配额，
   所以核心是「先上传、后占位」。
2. **一条消息一种 `msg_type`**：图文必须拆成两条（先文本、后图片），
   代价是消耗两格被动配额；第二条失败时错误里会说明"前 N 条已发出"。
3. **直传 ≤ 5 MB**：超过要分片上传（`upload_prepare` → 逐片 PUT →
   `upload_part_finish` → 合并），当前未实现，本地直接拒绝而不是让平台打回。

### 4.4 体积估算

| 消息量 | 7 天行数 | 库体积（含索引） |
|---|---|---|
| 1 000 条/天 | 7 000 | 3–6 MB |
| 5 000 条/天 | 35 000 | 15–25 MB |
| 10 000 条/天 | 70 000 | 30–45 MB |

结论：**消息留存不是磁盘风险，日志和媒体才是**。这也是为什么把 journald 限额与媒体配额写进设计而不是留给运维临场处理。

### 4.5 SQLite 参数

```sql
PRAGMA journal_mode = WAL;          -- 读写不互斥
PRAGMA synchronous  = NORMAL;       -- WAL 下足够，掉电最多丢最后一个事务
PRAGMA busy_timeout = 5000;         -- 同机 aria2 抢 IO 时的必要缓冲
PRAGMA foreign_keys = ON;
PRAGMA auto_vacuum  = INCREMENTAL;
PRAGMA wal_autocheckpoint = 1000;   -- 约 4 MB，WAL 不会无限长
```

- **单写连接**（`MaxOpenConns=1` 或写入串行化），读连接可多个。SQLite 的并发模型决定了这一点不能靠调参绕过。
- 数据库与 WAL 放本地盘，**禁止放 tmpfs 或网络盘**——WAL 在网络盘上会损坏。

---

## 5. 定时任务（R5）

### 5.1 调度模型

- **库表驱动 + 周期扫描**：每 30 s 扫 `jobs WHERE enabled=1 AND next_run_at <= now`（per-peer 行另要求目标群 `left_at=0`，见下）。不采用“每任务一个 timer”，原因是设备休眠/时钟跳变时 timer 会漂移，而扫描式每轮都按绝对时间重算，天然自愈。
- 单实例假设：由 systemd 保证只有一个进程。**不要在同一台机上起两个实例**，否则任务会双跑（若将来要多实例，需引入 `jobs.locked_by/locked_until` 行锁，当前不实现）。
- 任务注册来自插件的 `Scheduler.Register`，启动时 upsert 到 `jobs`；插件删除后对应行置 `enabled=0`，不物理删除（保留执行历史）。

**会话维度**（`JobSpec.PerPeer`）。任务有两种形态：

| 形态 | 行 | 执行 | 适用 |
|---|---|---|---|
| 全局（默认） | `peer=''`，一行 | 只跑一次，插件自己决定推给谁 | 汇总、巡检、只给管理员发的东西 |
| per-peer | 每个在群一行 | 每个群各跑一次，`JobSpec.Peer` 填目标群 | “每个群单独推送”的播报、签到提醒 |

per-peer 的行是谁建的？插件拿不到群列表（也不该拿），所以**由核心展开**：

- `Register` 时遍历 `qq_groups WHERE left_at=0` 建行；
- 入群事件（`GROUP_ADD_ROBOT`）时补建；
- 首次见到某个群的消息、或它从“已退出”恢复时补建（事件丢失的自愈路径）。

补建用 `InsertJobIfMissing`（`ON CONFLICT DO NOTHING`），不能用 `UpsertJob`：后者会把 `next_run_at` 重算成“下一个 cron 时刻”，群消息越密集，任务越永远轮不到。

**退群不需要写任何 `jobs` 行**。`DueJobs` 对 per-peer 行加一个 `EXISTS (qq_groups.left_at = 0)`，所以停用与恢复完全由群登记表这一个事实源决定。这是刻意的：如果退群也写 `enabled=0`，入群时就分不清该行是“因为退群停的”还是“因为连续失败熔断停的”，恢复动作会顺手把熔断状态清掉。

### 5.2 错过执行与重启

| 场景 | 行为 |
|---|---|
| 停机错过 | `now - next_run_at > 300 s` → 标记 `skipped` 并推进到下一个时间点，**不补跑**。补跑在低配机上极易形成“重启后同时触发 10 个任务”的雪崩 |
| 执行超时 | 60 s 强制取消，记 `timeout`，不影响下一轮 |
| 连续失败 | 指数退避（1/2/4/8/16 min）修改 `next_run_at`，5 次后 `enabled=0` 并告警 |
| 时钟 | 固定 `Asia/Shanghai`，显式存时区，不依赖容器/系统 `TZ` |

### 5.3 与外部 API 的关系

定时任务天然是“主动出站”的高发点。约束：**所有 Job 的出站请求必须走统一 `httpx`**，不允许插件自带 client，否则限流不可观测。

---

## 6. 出站访问层（R1：Torn API）

| 设计点 | 方案 |
|---|---|
| 统一客户端 | 单一 `httpx`：连接复用、5 s 超时、最多 2 次重试（仅对 5xx/超时）、全抖动指数退避 |
| 域名白名单 | 插件在 Manifest 声明（如 `api.torn.com`），未声明即拒绝，防止插件被滥用为代理 |
| 凭证 | Torn API key 走 `EnvironmentFile`（600 权限），**不入库、不入日志、不进 git** |
| 限流 | 按 key 维度的令牌桶，默认 100 req/min 起步（以 Torn 官方限额为准，做成配置项）；收到限流错误码时整桶退避 |
| 缓存 | 落 `plugin_kv`，key = `sha256(endpoint + sorted(params))`，TTL 分层：静态数据 24 h、玩家资料 5–10 min、余额/库存类 2–5 min |
| 降级 | 上游不可用时返回缓存副本并标注“数据可能过期”，而不是直接报错——这是对话类机器人体验的关键 |

Torn 的响应是 `{"error": {"code": .., "error": ".."}}` 形式的业务错误与 HTTP 200 并存，**不能只看状态码**，解析层要显式区分。

---

## 7. 消息发送与频控

- **被动回复**：群 5 min / 最多 5 次；单聊 60 min / 最多 4 次。核心为每个 `msg_id` 维护已用次数，插件拿到的 `Reply` 在第 6 次直接失败（不消耗 HTTP 调用）。
- **幂等**：`msg_id + msg_seq` 全局唯一，重试不得复用同一 `msg_seq`；这也是“每 peer 串行”的原因。
- **令牌桶**：Bot 维度（群 30/60 qpm，单聊 5 qps+30 qpm）+ 单关系维度（20 qpm、1000 条/天）。两层都要过，缺一即 429。
- **队列优先级**：被动回复插队到主动/批量之前，避免批量推送把对话消息饿死。
- **主动消息默认关闭**（R 全部为查询类需求，本不需要）。若将来做推送，按 (插件 × 场景) 白名单逐项开启，并受每日 1000 条上限约束。

---

## 8. 管理员重置与状态管理（R7）

### 8.1 平台给了什么、没给什么

**给了群内身份**：群消息事件（`GROUP_AT_MESSAGE_CREATE` / `GROUP_MESSAGE_CREATE`）的
`author.member_role` 直接是 `member / admin / owner`，与 `author.id` 在同一条载荷里到达，
零额外请求、零配额消耗。另外 `GET /v2/groups/{group_openid}/members/{member_openid}`
也返回 `member_role`，但该接口处于内邀阶段（错误码 `11253`，仅白名单机器人可用），
不能作为可靠依赖——**所以本项目不调用它**，群内角色只从事件载荷取。

**没给 Bot 运维身份**：平台不知道“谁是这台机器人的管理员”。群主是群主，不是 owner。

**没给 QQ 号**：但 `author.id` 是平台声明的用户唯一标识，群聊的 `member_openid` 与
单聊的 `user_openid` 对同一个人取同一个值（实测与文档示例一致）。因此 `admins`
表只用 openid 做主键，**角色全局有效**；`union_openid` 退化为兜底（可能为空）。

这三条决定了两件事：

- “谁有权重置 / 重启 / 看指标”**必须由本系统自己认定** → `admins` 表 + 认领流程（§8.2）；
- 平台群角色只用于“群管级”权限点（§8.7）：**群主不会自动获得 Bot 管理权**。

还有一条路径完全不经过鉴权：群管理员在 QQ 客户端直接把机器人移出群，这以
`GROUP_DEL_ROBOT` / `GROUP_ADD_ROBOT` 异步到达，只能靠事件做清理（§8.5），
不能假设所有变更都由对话指令发起。

### 8.2 管理员认定：一次性认领令牌

openid 是相对本机器人加密的，注册前无法预知，因此**不能在配置文件里硬编码首个管理员**。采用认领机制：

1. 启动时若 `admins` 为空，生成 32 字节随机令牌，打印到 journald（`journalctl -u qqbot | grep CLAIM`）；
2. 用户在目标会话发送 `/claim <token>`，核验通过即写入 `admins`，`role=owner`；
3. 令牌**一次性、10 分钟过期**，认领成功后从内存清除，并不再打印；
4. 之后由 owner 用 `/admin add` / `/admin rm` 管理。admin 与 owner 都是**全局角色**：
   无论通过群内还是私聊添加，效果一致（openid 跨场景同值，`admins` 主键只有 openid）。

代价是部署时必须看一眼日志；收益是**不存在“默认口令”这个永久攻击面**。

### 8.3 重置分级

| 级别 | 语义 | 数据影响 | 所需身份 | 冷却 |
|---|---|---|---|---|
| R0 重连 `/reset reconnect` | 断开当前 WSS 后**立即**重连（保留 session，走 Resume） | 无 | owner | 60 s |
| R1 会话 `/reset session` | 清该 peer 的 `plugin_kv`（`peer` 非空的行） | 该群/该用户需重新开始对话 | Bot 管理员 | 10 min |
| R2 数据 `/reset data` | 清该 peer 的 `messages` + `media`（先删文件后删行）+ `job_runs` | 该群历史不可恢复 | Bot 管理员 + 二次确认 | 1 h |
| R3 全量 `/reset all` | 清 `messages` / `media` / `processed_events` / `plugin_kv` / `job_runs` / `identities` | 全库业务数据不可恢复 | 仅 owner + 二次确认 + 短语 | 24 h |

**四条硬约定**（每条都对应一类"重置完机器人就废了"的故障）：

- **任何级别都不重置 `bot_state`**（token / session / seq）。凭证失效是运维问题，一旦与业务重置混在一起，会出现"重置一下把机器人搞成整天连不上"的连锁故障。
- **任何级别都不删 `admins`**，否则会把自己永久锁在门外，而且它是自举的——没有它连重新授权的入口都没有。
- **任何级别都不删 `qq_groups`**。群名单是访问控制配置而不是业务数据；清掉它，白名单一开就等于对所有群静默拉黑，与删 `admins` 是同一类事故。
- R2/R3 必须**先删媒体文件、再删行**。反过来会留下无引用的孤儿文件，只能靠扫盘才能发现，而扫盘在低配机上很贵。

**冷却按 `(level, peer)` 计**。R1/R2 换个群不受上一个群的影响——连清两个群是正常运维动作；R0/R3 是全局操作，`peer` 记为 `*`，因此全局冷却。冷却起点取 `reset_audit.finished_at`，只认 `status=done` 的行。

**R2 清 `job_runs` 但保留 `jobs`**：执行历史是该群的业务数据，任务定义（含 per-peer 实例行）是配置。这与“R2 不动 `qq_groups`”是同一条边界的两个方向。

### 8.4 二次确认

不可逆操作走两段式，而不是"回复 Y 确认"——`Y` 太容易误触发，也容易被群内他人抢答：

1. `/reset data` → **不执行**，只回"将清空消息 128 条、媒体 6 个"，生成 6 位一次性确认码，`reset_audit` 记 `pending`，回复"60 秒内回复 `/reset data <code>`"；
2. 用户回带码指令 → 校验 `(actor, peer, level)` 绑定 + 过期 + 冷却 → 执行，audit 置 `done` 并写入执行摘要。

确认码**绑定发起者、目标 peer 与级别**：换个会话用不了，换个人代答不了，换个级别也不行。

R3 在确认码之外还要**短语**：`/reset all <code> 确认`。确认码防误触，短语防"看都没看就按了两次"。

预览阶段的数字取自库（`PeerStats` / `DataStats`），不是估算——回复里说"消息 128 条"，删完就应该是 128 条。

重复发起同一目标的 `/reset data` 会把上一条 `pending` 置为 `cancelled` 再发新码，而不是拒绝：用户很可能只是没收到上一条消息，拒绝只会让他在同一个死胡同里反复试。

**过期由 GC 收尾**：超过 60 秒未确认的 `pending` 在下一轮 GC 里标成 `expired`。读路径不做这件事，否则"尝试过但没确认"在审计里会连痕迹都不剩。

**命名**：级别同时接受语义名与 R 编号（`/reset r2` 等价于 `/reset data`）。两种写法在文档里本来就并存，硬性二选一只会让另一半文档变成错的。

### 8.5 平台侧移除的处理

| 事件 | 处理 |
|---|---|
| `GROUP_DEL_ROBOT` | 打 `left_at`；按 **R1 语义**清该群的 `plugin_kv`（**不删消息**——可能是管理员误操作或临时调整，7 天到期自然清理）；私聊提醒 |
| `GROUP_ADD_ROBOT` | 清 `left_at`、重新取群名、补建 per-peer 任务实例、私聊提醒 |

**该群的定时任务不需要单独处理**：`DueJobs` 对 per-peer 行要求 `qq_groups.left_at = 0`，
所以打标记即停、清标记即恢复。这比在退群时写 `jobs.enabled=0` 更可靠——
后者会让“因退群停用”与“因连续失败熔断停用”共用一个开关，
重新入群时恢复任务就会顺手把熔断状态清掉（§5.1）。

**没有入群欢迎语**：`GROUP_ADD_ROBOT` 的载荷只有 `group_openid` / `op_member_openid` / `timestamp`，
没有 `msg_id`，无法走被动回复；要发只能走主动消息，会消耗日预算。
欢迎语不是必要功能，当前不做——所有者收到的私聊提醒已经覆盖了“机器人进了哪个群”。

> 兜底：若平台未下发 `GROUP_DEL_ROBOT`（或机器人被静默移除），由两条机制补偿——
> 该群 per-peer 任务随熔断自动停用（连续失败 5 次），消息在 7 天后按期清空。
> 反向也有一条：退群事件丢失后机器人重新说话，`ObserveGroup` 会清掉 `left_at`
> 并补建任务实例，状态自愈。**不要**假设事件一定到达，这是所有基于推送的清理逻辑的共同前提。

**当前实现状态**（2026-10-18）：表格中的行为已全部接上。唯一没做的是欢迎语，理由见上。

### 8.6 状态机

```
              /claim <token>                    /reset data|all
  (none) ───────────────────► (owner) ──────────────────────► pending
                                │                               │
                    /admin add  │                   确认码+短语 │ 60 s 未确认
                                ▼                               ▼
                             (admin) ──/admin rm──► (none)   done / expired
```

权限是**叠加**而非层级替换：owner 隐含全部；admin 除增删管理员外与 owner 相同；
R0 与 R3 恒为 owner 专属。冷却对四级一视同仁：R0/R1 在执行前检查，
R1/R2 在发码时检查一次、真正执行时再检查一次（中间可能已经过了别人的一次重置）。

### 8.7 权限模型：三级角色 + 会话内身份

参考 nonebot 的 `Permission` 思路（`SUPERUSER | GROUP_ADMIN` 表示任一满足），但把
判定收进核心：插件只声明"要什么"，核心在路由阶段决定"给不给"。插件里没有判权代码，
漏写检查也不会越权。

**两个正交维度**

| 维度 | 取值 | 来源 | 用途 |
|---|---|---|---|
| Bot 角色 `Role` | `user` < `admin` < `owner` | `admins` 表 | 能否管理机器人 |
| 群内角色 `GroupRole` | `unknown` < `member` < `admin` < `owner` | 事件载荷 `author.member_role` | 能否执行群管级操作 |

- **Bot 角色是全局的**：openid 跨场景同值，群内添加与私聊添加效果一致，没有"只在某个群生效"的委托；
- **私聊的群内角色默认等价于 `member`**：没有群，但调用者仍是普通成员，"要求成员及以上"的权限点在私聊里照常成立；
- 叠加规则：Bot 管理员在任何群都按群管级处理；平台群主**不会**因此成为 Bot 管理员；
- 未知群内角色（通知事件、字段缺失）按"低于成员"处理，依赖群角色的权限点失败（fail-closed）。

**权限点**（`host.Perm`，按位或组合，语义为"任一满足"）

| 权限点 | 成立条件 |
|---|---|
| `PermUser` | 任何人（含私聊、通知事件） |
| `PermGroupMember` | 群聊，或私聊里的成员（默认基线） |
| `PermGroupAdmin` | 平台群管 / 群主，或 Bot 管理员 |
| `PermGroupOwner` | 平台群主，或 Bot 管理员 |
| `PermBotAdmin` | Bot 管理员或所有者 |
| `PermBotOwner` | 仅 Bot 所有者 |

**声明方式**：`Manifest.Permission` 是插件级默认，`Command.Permission` 覆盖它
（零值 = 未声明 = 继承；要显式公开写 `PermUser`）。`DenyReply` 提供拒绝文案，
是否真的发出由 `[permission].deny_reply` 统一控制，默认关闭（静默）。

**判定时机**：`Core.Handle` 在去重之后、路由之前填 `Event.Actor`（Bot 角色查库，
群内角色已由网关填好），`Dispatcher` 在调用插件前过滤。权限先于优先级：高优先级但
无权限的插件不会挡住后面能处理该消息的插件。

**为什么不在插件里判权**：插件一旦能接管 `/admin` 就能给自己提权；同理，判权逻辑
分散在每个插件里时，任何一处漏写就是一个越权口子。核心统一判定 + 装配期校验
（未定义的权限位直接拒绝加载）把这类错误前移到启动阶段。

### 8.8 群准入：白名单与黑名单

**为什么放在核心**：群准入决定"这个群能不能跟机器人说话"，属于接入层策略，
和管理员权限一样不能让插件接管——否则一个插件就能把机器人拉回被拉黑的群。

**存储**：`qq_groups` 一行一个群，`mode` 三态（`''` / `allow` / `deny`），
`left_at` 标记机器人在不在群里（`0` = 在群）。用三态而不是两个布尔列，
是为了让"同时在黑白名单"这种自相矛盾的状态不可能出现。

**判定顺序**（`Core.Handle`，在去重、落库、路由之前）：

1. 群消息先登记 `group_openid`（顺手清 `left_at`：能收到消息就说明在群里）；
   首次见到时异步尝试取一次群名（失败留空）；
2. 黑名单开启且 `mode=deny` → 丢弃；
3. 白名单开启且 `mode≠allow` → 丢弃；
4. 其余放行。两个开关都关 = 不设准入。

被丢弃的消息**不落 `messages`、不进 `processed_events`、不触发任何插件**——
"不进入 bot 系统"是字面意思，调试轨迹里只留一行 `群未放行，已丢弃（原因）`。
登记发生在判定之前，所以被丢弃的群仍会出现在 `/group list` 里，管理员能按 id 补救。

**例外一**：Bot 管理员的 `/group` 指令在"未加入白名单"的群里可以穿过闸门，
否则新群无法自助接入（白名单默认开启且为空时，所有群都会被拦）。黑名单是硬阻断，
群内任何人（包括 owner）都不能自助解除，只能从私聊操作。

**例外二**：群生命周期通知（入群/退群）不走闸门，见下一节。

#### 群生命周期：入群与退群

平台推 `GROUP_ADD_ROBOT` / `GROUP_DEL_ROBOT`，载荷只有三个字段：
`group_openid`、`op_member_openid`（操作人）、`timestamp`。网关把它们解码成
`host.Notice` 语义标签（`bot.joined_group` / `bot.left_group`），核心只认标签——
让核心去比较平台事件名，会破坏"平台事件名只出现在 gateway 包"这条边界。

| 事件 | 处理 |
|---|---|
| 入群 | 立即建档并清 `left_at` → 后台取群名写库 → 私聊通知所有者 |
| 退群 | 置 `left_at` → 私聊通知所有者；保留名单与名称，重新拉回群时不用再授权 |

**为什么不走闸门**：新群此刻必然未放行，走闸门这条通知会被直接丢掉，
结果是"被谁拉进了哪个群"永远没人知道。这两个事件描述的是机器人自身的状态变化，
不是群里的消息，准入判定对它们没有意义。

**为什么在去重之后**：平台重投同一条入群事件时，不能给所有者发两遍私聊，
也不能把 `left_at` 反复改。实际顺序是 `闸门（仅群消息）→ 去重 → 生命周期 → 权限 → 路由`。

**通知发给谁**：默认只发所有者（`[group].notify = "owner"`）。新群是否放行是
所有者一个人的决定，抄送给所有管理员只会制造噪音；`admin` 抄送管理员，`off` 完全不发。
通知走主动私聊，因此受 `[active].enabled` 与每 peer 日预算约束，但**不**走
`[active].plugins` 白名单——这是基础设施级通知，不该要求用户把一个叫 "core"
的插件名写进配置才能收到。取名字与发通知都在后台协程，`Handle` 不陪着等 HTTP。

**已退出状态**：`left_at` 非零的行保留，只是沉到 `/group list` 末尾并标出退出时间。
它不再计入"在群"、也不再被 `/group refresh` 刷新。
**自愈**：收到该群任何消息都会清掉 `left_at`——断线期间丢掉的 `GROUP_ADD_ROBOT`
靠这一条兜住，否则该群会被永久当成"已退出"，表现为 refresh 永远跳过它。

#### 群名与批量刷新

`GET /v2/groups/{group_openid}/info` 返回 `group_name` 等字段，但该接口标注为
"仅白名单机器人可用"（`11253`）。因此：

- 首次见到新群、或收到入群事件时只尝试一次；成功写 `name_source=api`，失败留空；
- `/group name` 手工设置后 `name_source=manual`，任何自动抓取都不得覆盖它；
- `/group refresh` 批量重刷所有**在群**群的名称：串行调用（接口限额 30 QPM，
  并发只会更快打满配额）、每群 8s 超时、整批 10 分钟封顶、同一时刻只允许一个任务。
  因为它要跑 HTTP，指令**立即回执、后台执行、完成后私聊汇报**——`Handle` 就是
  网关的读循环，同步跑 N 个请求会把整条消息链路卡住。

**已知短板**：平台没有"列出机器人加入的群"的接口，`qq_groups` 只能靠事件积累。
入群事件补上了最大的缺口（被拉进群即刻登记，不必等有人说话），但仍有漏网：
机器人早就在群里、而本次部署之后该群再没有任何消息与事件。

**配置**（两套开关独立，默认都开）：

```toml
[group]
whitelist  = true    # 只允许白名单群
blacklist  = true    # 丢弃黑名单群（优先于白名单）
fetch_name = true    # 见群/入群时尝试调用群信息接口取名称
notify     = "owner" # 入群/退群私聊通知：owner｜admin｜off
```

---

## 9. 部署与运维

### 9.1 systemd

```ini
[Unit]
Description=QQ Bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=qqbot
WorkingDirectory=/opt/qqbot
ExecStart=/opt/qqbot/bin/qqbot -c /etc/qqbot/config.toml
EnvironmentFile=/etc/qqbot/env
Restart=on-failure
RestartSec=10
StartLimitIntervalSec=300
StartLimitBurst=5
MemoryMax=256M
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/qqbot

[Install]
WantedBy=multi-user.target
```

`StartLimitBurst=5 / 300 s` 是刻意配的：**防止 session 配额（1000/天）被重启循环吃掉**。配置错误时应停在 failed 状态等人看，而不是无声重试一整夜。

### 9.2 升级流程

1. 进程收到 `SIGTERM` → 停止收新事件 → 落 `session_id`/`seq` → 关闭 WSS（不主动 Identify 关闭）。
2. 替换二进制 → `systemctl restart`。
3. 启动后优先 **Resume**（带 `session_id` + `seq`），失败（4006/4009）才 Identify。这一步能避免每次发布都消耗一次 Identify 配额并丢失断线期间的事件。

### 9.3 日志与磁盘

- 结构化 JSON 日志到 stdout，由 journald 收集，`SystemMaxUse=200M`。
- **禁止**记录 `access_token`、`AppSecret`、Torn API key、完整媒体 URL（带签名）。
- 磁盘水位：< 500 MB 时告警。低配机上 SQLite 写失败的症状是“消息时好时坏”，极难排查，必须提前报警。

### 9.4 测试环境切换

| 项 | 本机（Windows） | 生产（Debian） |
|---|---|---|
| 配置 | `config.local.toml`（不入 git） | `/etc/qqbot/config.toml` |
| 环境 | `sandbox = true` → `sandbox.api.sgroup.qq.com` | `sandbox = false` |
| IP 白名单 | **不需要** | 必须填服务器公网 IP |
| 数据目录 | `./.temp/data` | `/var/lib/qqbot` |

同一份代码、同一套 schema，仅配置差异。**不要为沙箱维护第二套逻辑**——沙箱与正式的最大差异只有域名和事件来源范围。

---

## 10. 风险清单

> 已消解：原“主体类型未定”一项（D3 确认个人主体可开发群与消息列表单聊）。

| 等级 | 风险 | 缓解 |
|---|---|---|
| 高 | session 配额 1000/天被重启循环耗尽 | `StartLimitBurst` + 退避 + 持久化 session（4.1 / 9.2） |
| 高 | **两个进程共用数据目录**：网关 session 互顶（`4902 reset by resume`），且各回一遍消息、各记一遍幂等表 | 启动时按 `[store].path` 取独占文件锁，拿不到直接退出并报出持有者 pid；锁随进程退出释放，无残留（§2.1） |
| 高 | **重置误操作**：R2/R3 不可逆，群内误触发即丢历史 | 分级 + 二次确认码 + 冷却 + `reset_audit`（第 8 章） |
| 中 | **管理员鉴权来源单一**：平台给群内身份，不给 Bot 运维身份，只能靠本地 allowlist | 认领令牌 + owner/admin 分离 + 全局角色；官方若开放 Bot 级身份则升级为双因子 |
| 中 | **空白名单自锁**：白名单默认开启且初始为空 → 所有群消息被丢弃 | 启动时 warn；未入名单的群里放行 Bot 管理员的 `/group allow` 自助接入；也可私聊 `/group list` 按 id 添加 |
| 中 | 认领令牌泄露 → 他人抢先成为 owner | 一次性 + 10 min 过期 + 仅打印到本机 journald；认领后不再可用 |
| 中 | P1 同进程插件无隔离，插件 panic/内存泄漏波及核心 | `recover` + 超时 + 连续失败熔断 + `MemoryMax` |
| 中 | SQLite 与 aria2 同机争 IO，出现 `database is locked` | `busy_timeout=5000` + 单写连接 + 分批 GC |
| 中 | botgo 维护频率低于 botpy | 把 botgo 当协议参考而非黑盒；opcode 状态机保留独立可替换层 |
| 中 | 沙箱“免频控”仅频道场景有明文，群场景未明确 | 群场景按受限设计，压缩批量回复 |
| 低 | 出站图片超 5 MB 直传上限 | 本地预检直接报错，并提示"更大的图需分片上传（未实现）" |
| 低 | 插件用 `ImageFromURL` 让平台抓任意地址 | 只接受 http(s)；下载发生在平台侧，本机不出站，无 SSRF 面；发什么图由插件决定，核心不做内容审核 |
| 低 | Torn API key 泄露 | env 文件 600 + 不入库不入日志 |

---

## 11. 待决事项

> 2026-10-18 收敛：分片上传与 `plugins/torn` **暂时搁置**；REST 接口**不做**。
> 已定的条目保留原文并标注结论，不再反复讨论。

1. **服务器规格**：`free -m` 实测余量，用于确认 `MemoryMax=256M` 是否与 TS3/WebDAV/aria2 共存无冲突。
2. **要不要做分片上传** —— **暂时搁置（2026-10-18）**。当前只支持 ≤ 5 MB 的 base64 直传，更大的图本地直接拒绝。
   若将来要发大图或视频，需要实现 `upload_prepare` → 逐片 PUT → `upload_part_finish`
   的完整流程（官方推荐路径，约 4 个接口 + 预签名 URL 的 PUT）。
   搁置的理由：现有 `ImageFromURL` 已能覆盖大图场景（下载发生在平台侧，本机不出站），
   分片上传的复杂度（4 个接口 + 预签名 PUT + 断点/重试语义）与收益不成比例。
3. **重置指令的命名与入口**：~~`/reset` 是否与其他插件的命令前缀冲突~~ —— 已定：`/reset` 进核心保留前缀（插件声明同前缀会被拒绝加载），级别同时接受语义名与 `R0–R3` 编号。
4. ~~是否需要**对外提供 REST 接口**~~ —— **不做（2026-10-18 定案）**。全部为出站调用，
   进程不监听任何入站端口；机器人只通过 QQ 平台事件与主动消息交互。若将来需要外部触发，
   优先做成插件里的定时任务或平台侧指令，而不是新开一个需要鉴权与防火墙放行的入站面。
