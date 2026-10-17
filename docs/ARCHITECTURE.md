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
| R3 | 图片**默认不接收**，可开启并设**最大占用空间** | 两段式下载 + 配额 + LRU 淘汰 |
| R4 | 音视频**不接收** | 按 `msg_type` 在入口直接过滤，不落库 |
| R5 | 可能有**定时任务** | 库表驱动的调度器，不依赖外部 cron |
| R6 | 每个部件**插件化**，主体与功能插件分离 | 核心层只做接入/存储/调度，业务全在插件 |
| R7 | **管理员可主动重置机器人** | 需管理员认定 + 分级重置语义 + 二次确认 + 审计（见第 8 章） |
| R8 | **群准入可控**：未授权群不得进入机器人 | 群注册表 + 白/黑名单两套独立开关 + `/group` 管理指令（§8.8） |

### 1.2 平台硬约束（已核实官方文档）

| 约束 | 事实 | 后果 |
|---|---|---|
| 鉴权 | `POST https://bots.qq.com/app/getAppAccessToken`，`{appId, clientSecret}` → `access_token` / `expires_in: 7200`；请求头 `Authorization: QQBot {token}` | 必须缓存，到期前 60 s 刷新；旧 `Bot appid.token` 已废弃 |
| 接入 | v2 仅 WebSocket 长连接：`/gateway/bot` → op10 → op2 Identify → op1 心跳 → op6 Resume | 纯出站；服务器零入站端口 |
| 环境 | 沙箱 `https://sandbox.api.sgroup.qq.com`；**IP 白名单仅作用于正式环境** | 本机可开发，动态 IP 无影响；上线需服务器固定 IP |
| 会话配额 | `session_start_limit: total 1000 / reset 86400000 / max_concurrency 1` | 重启风暴会耗尽日配额 → 必须退避重启 + 持久化 session |
| 被动回复 | 群：5 分钟内、最多 5 次；单聊：60 分钟内、最多 4 次 | 回复必须快；超时即失败，不能重试硬撑 |
| 主动消息 | 群：未认证 30 qpm / 认证 60 qpm；单聊：未认证 5 qps+30 qpm；单关系 20 qpm、每群/每用户每天 1000 条 | **默认全关**，仅白名单场景开启 |

### 1.3 资源预算

| 项 | 预算 |
|---|---|
| 内存（核心） | 15–40 MB |
| 内存（每插件，同进程） | +2–8 MB |
| 二进制磁盘 | 25–45 MB |
| SQLite（1000–10000 条/天，留 7 天） | 5–40 MB |
| 媒体配额 | 默认 256 MB（可配） |
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
| `Sender.Reply` | 必须带原事件 ID，核心自动分配 `msg_seq` | 走频控；超出被动窗口直接失败并告警 |
| `Sender.SendActive` | 主动消息 | **默认禁用**，需在配置中按 (插件, 场景) 白名单开启 |
| `Store.KV` | 按插件命名空间隔离的 KV，支持 TTL | 只能读写自己的命名空间 |
| `Store.Query` | 只读、参数化、行数上限 1000 | 不允许 DDL/写操作 |
| `HTTP.GetJSON` / `PostJSON` | 域名白名单 + 超时 + 重试 + 自动降级 | 未声明域名一律拒绝 |
| `Scheduler.Register` | 注册 Job（cron 表达式 + 超时） | 仅启动期可注册 |
| `Log` | 结构化日志，自动带 plugin 字段 | 敏感字段脱敏 |

**插件不直接持有 SQLite 连接、不直接持有 WS 连接、不读环境变量取密钥。** 这条边界决定了：插件可单测（Host 用 fake）、可复用（换核心只改 Host 适配层）、可审计（能力清单即权限清单）。

### 3.4 事件路由

```
事件 → 去重（event_id）→ 落库 → 按 intents 过滤 → 候选插件按 priority 排序
     → 依次匹配 commands → 首个命中且 broadcast=false 的处理并返回
     → 全未命中 → 交核心 fallback（默认静默，可配置默认提示）
```

- 路由在**插件的 matcher 表**上做，核心不解析业务语义。
- 单条消息只允许一个插件回复（除显式 `broadcast: true`），从根本上避免“两个插件同时回、消耗 5 次被动额度”的问题。
- **核心保留前缀**：`/claim`、`/reset`、`/grant`、`/revoke` 由核心以最高优先级独占处理，插件在启动期若声明了相同前缀，**直接拒绝加载并报错**。否则一个第三方插件就能劫持重置指令，等于把第 8 章的鉴权整个绕过去。

### 3.5 插件目录与复用方式

```
qqbot/
  cmd/bot/main.go              入口，集中注册插件（P1 的唯一“装配点”）
  internal/gateway/            WSS 状态机、心跳、resume、分片
  internal/core/               路由、幂等、频控、调度、媒体、GC
  internal/host/               Host 接口实现（插件唯一可见的 API）
  internal/store/              schema、迁移、GC
  internal/httpx/              统一出站客户端
  plugins/torn/                插件：Torn 查询
  plugins/echo/                插件：最小示例，兼作契约回归用例
  config/config.toml           全局配置，插件配置在 [plugins.<name>]
  deploy/qqbot.service
  docs/ARCHITECTURE.md
```

复用方式：`plugins/*` 与 `internal/host` 可拆为独立 Go module，供其他机器人项目引用；业务插件不 import `internal/gateway`，所以换接入层（例如将来支持别的 IM）时插件零改动。

### 3.6 隔离与超时

| 措施 | 参数 |
|---|---|
| panic 捕获 | 每个 `Handle`/`Job` 包 `recover`，记堆栈，回复降级文案 |
| 超时 | `Handle` 默认 3 s（硬上限 8 s）；`Job` 默认 60 s |
| 连续失败熔断 | 同一插件连续失败 5 次 → 自动禁用该插件并告警，**不影响其它插件** |
| 内存 | 进程级 `MemoryMax=256M`；插件级无法单独限制（P1 的固有短板） |

### 3.7 依赖清单（Go，D1/D2 定稿后）

设计约束：**第三方依赖不超过 5 个**。Go 项目的长期维护成本几乎全部来自依赖链，而不是自己的代码。

| 用途 | 选型 | 理由 |
|---|---|---|
| WSS 客户端 | `coder/websocket` | 原生支持 `context` 取消，重连/超时逻辑可统一；`gorilla/websocket` 生态更大但需自己包一层取消机制 |
| SQLite | `modernc.org/sqlite` | 纯 Go、免 cgo，Windows 交叉编译 Linux 产物一条命令搞定；`mattn/go-sqlite3` 的 cgo 交叉编译是公认的坑 |
| cron 解析 | `robfig/cron/v3` | 仅用于解析表达式算 `next_run_at`，调度仍由自研 30 s 扫描驱动 |
| 配置 | `BurntSushi/toml` | 轻量。**不引 viper**——它会把依赖树放大一个数量级，与本项目的低配目标冲突 |
| 日志 | 标准库 `log/slog` | 零依赖，自带 JSON handler，直接对接 journald |

不上依赖的部分：HTTP 用标准库 `net/http`（连接复用、超时、退避自己写，约 100 行）；测试用标准 `testing` + fake Host，不引 testify。目标 Go 版本 1.22+（`slog` 与循环变量语义修复）。

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

-- 媒体（默认不写；开启后受配额约束）
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
CREATE TABLE plugin_kv (
  plugin     TEXT NOT NULL,
  k          TEXT NOT NULL,
  v          BLOB NOT NULL,
  expires_at INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (plugin, k)
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
  first_seen_at INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_qq_groups_mode ON qq_groups(mode, last_seen_at);

-- 重置审计（不可省略：重置是不可逆操作）
CREATE TABLE reset_audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  actor      TEXT NOT NULL,
  peer       TEXT,
  level      TEXT NOT NULL,                -- R0 | R1 | R2 | R3
  detail     TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_reset_audit_ts ON reset_audit(created_at);

-- 定时任务（R5）
CREATE TABLE jobs (
  id                   TEXT PRIMARY KEY,   -- <plugin>:<name>
  plugin               TEXT NOT NULL,
  spec                 TEXT NOT NULL,      -- cron
  timezone             TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  enabled              INTEGER NOT NULL DEFAULT 1,
  next_run_at          INTEGER NOT NULL,
  last_run_at          INTEGER,
  last_status          TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  payload              TEXT
);
CREATE INDEX idx_jobs_next ON jobs(enabled, next_run_at);

CREATE TABLE job_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  status      TEXT,                        -- ok | error | timeout | skipped
  error       TEXT
);
CREATE INDEX idx_job_runs ON job_runs(job_id, started_at);
```

### 4.2 7 天留存的实现要点

- **分批删**，不要一次 `DELETE`：每 10 分钟执行 `DELETE FROM messages WHERE created_at < ? LIMIT 5000` 循环至影响行数为 0。理由：单条大事务会长时间持有写锁，与 aria2 的磁盘 IO 叠加时造成 `database is locked`。
- **保留期是配置项**，默认 `168h`；`processed_events` 默认 `72h`（只需覆盖 resume 窗口）。
- `job_runs` 同样纳入 GC（默认 7 天），否则它是最容易无声膨胀的表。
- 删除后用 `PRAGMA incremental_vacuum`（配合 `auto_vacuum=INCREMENTAL`）回收，避免每月一次全库 `VACUUM` 造成分钟级卡顿。

### 4.3 媒体策略（R3 / R4）

| 阶段 | 行为 |
|---|---|
| 默认（`media.enabled=false`） | 只记 `has_media=1`，**不下载**，事件里的 URL 不入库（URL 带签名，留存无意义且占空间） |
| 开启后 | 先 `HEAD` 取 `Content-Length` → 超 `max_file_bytes`（默认 8 MB）直接放弃 → 下载到 `tmp/` → 校验 sha256 → 原子 `rename` 到 `media/YYYY/MM/DD/<sha256>.<ext>` → 入库 |
| 去重 | 以 sha256 唯一索引去重，同一图片重复发送不占第二份空间 |
| 配额 | `quota_bytes`（默认 256 MB）。写入前预检 `SUM(bytes) + new > quota` → 按 `last_access_at` 最旧优先淘汰，**淘汰同时删文件** |
| 音视频 | 入口按 `msg_type` 直接丢弃，不建行、不下载；必要时回固定文案“暂不支持音视频” |
| 过期 | 随 `messages` 一起按 7 天删除；实际生效取“留存期”与“配额”中更严的一个 |

> 防御点：媒体 URL 是外部可控输入，下载必须限定 scheme=https、禁止重定向到内网地址、限制单次读取字节数（防慢速大文件打满内存）。

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

- **库表驱动 + 周期扫描**：每 30 s 扫 `jobs WHERE enabled=1 AND next_run_at <= now`。不采用“每任务一个 timer”，原因是设备休眠/时钟跳变时 timer 会漂移，而扫描式每轮都按绝对时间重算，天然自愈。
- 单实例假设：由 systemd 保证只有一个进程。**不要在同一台机上起两个实例**，否则任务会双跑（若将来要多实例，需引入 `jobs.locked_by/locked_until` 行锁，当前不实现）。
- 任务注册来自插件的 `Scheduler.Register`，启动时 upsert 到 `jobs`；插件删除后对应行置 `enabled=0`，不物理删除（保留执行历史）。

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
| R0 重连 | 主动断开 WSS 后重连（先 Resume，失败再 Identify） | 无 | owner | 60 s |
| R1 会话 | 清该 peer 的 `plugin_kv` 与各插件会话上下文 | 该群/该用户需重新开始对话 | Bot 管理员 | 10 min |
| R2 数据 | 清该 peer 的 `messages` + `media`（先删文件后删行）+ `job_runs` | 该群历史不可恢复 | Bot 管理员 + 二次确认 | 1 h |
| R3 全量 | 清空全部业务表（**保留 `admins` 与 `bot_state`**） | 全库不可恢复 | 仅 owner + 二次确认 + 短语输入 | 24 h |

三条硬约定：

- **任何级别都不重置 `bot_state`**（token / session / seq）。凭证失效是运维问题，一旦与业务重置混在一起，会出现“重置一下把机器人搞成整天连不上”的连锁故障。
- **任何级别都不删 `admins`**，否则会把自己永久锁在门外。
- R2/R3 必须**先删媒体文件、再删行**。反过来会留下无引用的孤儿文件，只能靠扫盘才能发现，而扫盘在低配机上很贵。

### 8.4 二次确认

不可逆操作走两段式，而不是“回复 Y 确认”——`Y` 太容易误触发，也容易被群内他人抢答：

1. `/reset data` → 不执行，生成 6 位一次性确认码，`reset_audit` 记 `pending`，回复“60 秒内回复 `/reset data <code>`”；
2. 用户回带码指令 → 校验 `(actor, peer, level)` 绑定 + 冷却 → 执行，audit 置 `done`。

确认码**绑定发起者与目标 peer**，不能在别的会话复用，也不能被他人代答。

### 8.5 平台侧移除的处理

| 事件 | 处理 |
|---|---|
| `GROUP_DEL_ROBOT` | 按 **R1 语义**清会话，**不删消息**（可能是管理员误操作或临时调整，7 天到期自然清理）；同时把该群 `jobs` 置 `enabled=0`，避免向已不在的群推送 |
| `GROUP_ADD_ROBOT` | 恢复该群 `jobs`，并回一条欢迎语（带 `event_id`，属被动回复，不消耗主动额度） |

> 兜底：若平台未下发 `GROUP_DEL_ROBOT`（或机器人被静默移除），由两条机制补偿——该群 `jobs` 连续失败触发熔断后自动禁用；消息在 7 天后按期清空。**不要**假设事件一定到达，这是所有基于推送的清理逻辑的共同前提。

### 8.6 状态机

```
              /claim <token>                    /reset <level>
  (none) ───────────────────► (owner) ──────────────────────► pending
                                │                               │
                    /admin add  │                     code 校验 │ 60 s 超时
                                ▼                               ▼
                             (admin) ──/admin rm──► (none)  executed / discard
```

权限是**叠加**而非层级替换：owner 隐含全部；admin 除增删管理员外与 owner 相同；
R0 与 R3 恒为 owner 专属。

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

**存储**：`qq_groups` 一行一个群，`mode` 三态（`''` / `allow` / `deny`）。
用三态而不是两个布尔列，是为了让"同时在黑白名单"这种自相矛盾的状态不可能出现。

**判定顺序**（`Core.Handle`，在去重、落库、路由之前）：

1. 群事件先登记 `group_openid`；首次见到时异步尝试取一次群名（失败留空）；
2. 黑名单开启且 `mode=deny` → 丢弃；
3. 白名单开启且 `mode≠allow` → 丢弃；
4. 其余放行。两个开关都关 = 不设准入。

被丢弃的消息**不落 `messages`、不进 `processed_events`、不触发任何插件**——
"不进入 bot 系统"是字面意思，调试轨迹里只留一行 `群未放行，已丢弃（原因）`。
登记发生在判定之前，所以被丢弃的群仍会出现在 `/group list` 里，管理员能按 id 补救。

**唯一例外**：Bot 管理员的 `/group` 指令在"未加入白名单"的群里可以穿过闸门，
否则新群无法自助接入（白名单默认开启且为空时，所有群都会被拦）。黑名单是硬阻断，
群内任何人（包括 owner）都不能自助解除，只能从私聊操作。

**群名**：`GET /v2/groups/{group_openid}/info` 返回 `group_name` 等字段，但该接口
标注为"仅白名单机器人可用"（`11253`）。因此每个新群只尝试一次：成功写入
`name_source=api`，失败留空；`/group name` 手工设置后 `name_source=manual`，
自动抓取不得覆盖它。

**已知短板**：平台没有"列出机器人加入的群"的接口，`/group list` 只能列出
"收到过事件的群"。刚被拉进群、还没人说话时不会出现在列表里。

**配置**（两套开关独立，默认都开）：

```toml
[group]
whitelist  = true   # 只允许白名单群
blacklist  = true   # 丢弃黑名单群（优先于白名单）
fetch_name = true   # 首次见群时尝试调用群信息接口取名称
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
| 高 | **重置误操作**：R2/R3 不可逆，群内误触发即丢历史 | 分级 + 二次确认码 + 冷却 + `reset_audit`（第 8 章） |
| 中 | **管理员鉴权来源单一**：平台给群内身份，不给 Bot 运维身份，只能靠本地 allowlist | 认领令牌 + owner/admin 分离 + 全局角色；官方若开放 Bot 级身份则升级为双因子 |
| 中 | **空白名单自锁**：白名单默认开启且初始为空 → 所有群消息被丢弃 | 启动时 warn；未入名单的群里放行 Bot 管理员的 `/group allow` 自助接入；也可私聊 `/group list` 按 id 添加 |
| 中 | 认领令牌泄露 → 他人抢先成为 owner | 一次性 + 10 min 过期 + 仅打印到本机 journald；认领后不再可用 |
| 中 | P1 同进程插件无隔离，插件 panic/内存泄漏波及核心 | `recover` + 超时 + 连续失败熔断 + `MemoryMax` |
| 中 | SQLite 与 aria2 同机争 IO，出现 `database is locked` | `busy_timeout=5000` + 单写连接 + 分批 GC |
| 中 | botgo 维护频率低于 botpy | 把 botgo 当协议参考而非黑盒；opcode 状态机保留独立可替换层 |
| 中 | 沙箱“免频控”仅频道场景有明文，群场景未明确 | 群场景按受限设计，压缩批量回复 |
| 低 | 媒体下载被构造为慢速大文件 | 单次读取上限 + Content-Length 预检 + scheme/域名校验 |
| 低 | Torn API key 泄露 | env 文件 600 + 不入库不入日志 |

---

## 11. 待决事项

1. **服务器规格**：`free -m` 实测余量，用于确认 `MemoryMax=256M` 是否与 TS3/WebDAV/aria2 共存无冲突。
2. **媒体配额具体值**：默认 256 MB 是否合适（按实际群规模与图片开启意愿调整）。
3. **重置指令的命名与入口**：`/reset` 是否与其他插件的命令前缀冲突（路由在启动期静态校验，命名需一次定好）。
4. 是否需要**对外提供 REST 接口**（当前理解：不需要，全部为出站调用）。若需要，需增加 Nginx/Caddy + 鉴权，并在防火墙开一个入站端口。
