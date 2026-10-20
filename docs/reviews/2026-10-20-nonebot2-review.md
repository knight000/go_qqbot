# 对照 nonebot2 的代码评审

- 评审对象：`d248055`（工作区干净，Go 1.26.1）
- 参照物：nonebot2 `master`（`nonebot/message.py`、`nonebot/internal/matcher/matcher.py`）与 2.4.x 文档
- 文档性质：**评审记录，不是事实源**。结论落地时应转为 issue 或 ADR，并同步 `ARCHITECTURE.md`。
- 比较范围：只比较语义与契约，不比较语言特性。凡涉及取舍的地方都给出代价，不做单边推荐。

---

## 0. 结论摘要

一句话：**go_qqbot 在"强一致、可静态校验、单机低配"这条路上已经比 nonebot2 更严谨；真正缺的不是架构，而是一层"会话（多轮交互）原语"。**

按收益排序：

| # | 事项 | 性质 | 建议 |
|---|---|---|---|
| 1 | 会话原语缺失（无 `pause/got` 的等价物） | 能力空洞 | 借鉴 nonebot 的 temp matcher，但**必须补上 peer 维度**（见 §2.1） |
| 2 | 命令解析在核心与插件各做一遍 | 契约缺陷 | `Event` 增加 `Command/Args`，路由结果回传插件（见 §2.2） |
| 3 | `Broadcast` 挂在中标插件上，受名字排序影响 | 语义缺陷 | 改为"层级属性"或 per-plugin `Block`（见 §2.3） |
| 4 | `Capabilities` / `ConfigSchema` 是死字段 | 文档与实现脱节 | 实现校验，或显式标注为保留字段（见 §4.1） |
| 5 | `@提及` 在解码时被丢弃，路由无法表达 | 能力缺口 | 加语义标记 `Event.Mentioned`（见 §4.2） |
| 6 | 主动消息日预算只在内存 | 与注释承诺相反 | 落 SQLite（见 §4.4） |
| 7 | 调度器全局串行 | 扩展性上限 | 任务间并行、同类串行（见 §4.5） |

明确**不建议**借鉴的四项见 §3，避免以后重复讨论。

---

## 1. 心智模型对照

| 维度 | nonebot2 | go_qqbot | 判断 |
|---|---|---|---|
| 路由单位 | `Matcher`（可挂多个 handler、可运行期增删） | `Plugin`（单 `Handle` 返回单 `Reply`） | 前者灵活，后者可在启动期静态校验；本场景后者更合适 |
| 事件并发 | 单事件循环；**同优先级并行**，每个 matcher 拿 `state.copy()` | 按 peer 串行 + 全局 16 令牌；**只跑中标者** | go 版对"同一会话串行"是硬保证；nonebot 的并行同优先级会让两个插件同时回一条消息 |
| 会话 | 临时 matcher（priority 0 + `expire_time`）+ 异常控制流 | 无。只能 `PeerKV` 手工状态机 | **最大差距** |
| 会话状态 | 内存 dict，重启即失 | SQLite + TTL | go 版更强，且 R1 能定向清理 |
| 权限 | `Rule`/`Permission` 组合子，插件自定义 | 声明式权限位，核心在路由阶段拦截 | go 版更难写错；代价是不可组合（无"且/或/非"） |
| 配置 | 全局扁平 + 前缀 + pydantic 运行时校验 | 分段 TOML + 弱类型读取 | 隔离更好，**校验更弱** |
| 跨插件复用 | `require()` + 加载顺序解析 | Go import + `internal/host` | Go 版更简单，无顺序问题 |
| 平台扩展 | Driver / Adapter / Bot 三层，可多平台 | 单平台硬编码在 `internal/gateway` | 有意的取舍；seam 的位置是 `host.Event` + `api.Client` |
| 排序哲学 | priority 越小越先 | priority 越大越先 | 移植插件时要换算，容易踩 |

补充一个容易被误判的对照：nonebot 的 `Message` 是消息段数组，可以一条消息里混排文本、图片、@；go_qqbot 的 `Reply{Content, MsgType, Image}` 只能二选一，图文拆两条。**这是 QQ 开放平台的 `msg_type` 约束所致，不是设计缺陷**，不应为了对齐 nonebot 而改。

---

## 2. 值得借鉴的三项

### 2.1 会话原语：把 nonebot 的 temp matcher 做成 peer 级

#### nonebot 的做法

`pause()` / `reject()` / `finish()` 都是抛异常终止 handler 链；`run()` 顶层捕获后，用 `Matcher.new()` 重新注册一个：

- `temp=True`：触发一次即销毁；
- `priority=0`：最高优先级，下一次事件必然先命中它；
- `block=True`：处理完不再向低优先级传播；
- `default_state=self.state`：整个状态字典带过去，实现"断点续跑"；
- `expire_time=bot.config.session_expire_timeout`：超时自动失效。

`reject` 与 `pause` 的唯一区别是 `resolve_reject()` 把当前 handler 插回队首（重放当前步），`pause` 从下一步继续。这是"循环式对话"与"线性对话"的分界。

**关键缺陷（不要照抄）**：重建时 `rule=Rule()`（空规则）、`type="message"`，不比对 `session_id`。也就是说，**pause 之后的任意用户的下一条消息都会推进这个会话**。源码里没有任何 `get_session_id` 过滤，会话隔离要靠插件的 `type_updater` / `permission_updater` 或 `set_target` 自行维护。这一点在多人活跃群里是实际会串的。

#### 本项目的现状与后果

`Plugin.Handle` 返回 `(*Reply, error)`，事件生命周期就此结束。要做多轮交互只能：

1. 用 `Host.PeerKV(peer)` 自己存状态；
2. 用一个不带 `Commands` 的插件（`matchCommands` 返回 `nil, true`）接住该 intent 的所有消息；
3. 在 `Handle` 里先查 KV 判断"是不是在等我"。

三个具体后果：

- **等待期间无法独占输入**。路由是静态的：如果等待中的插件 priority 低于某个命令插件，用户想输入的答案会被命令插件抢走；如果为了不被抢而把 priority 提到最高，就会吞掉所有人的所有消息。
- **每个需要交互的插件都要重写一遍状态机**：键名、TTL、并发、清理、`/reset` 后的残留，全靠自觉。`PeerKV` 只解决了存储隔离，没解决"谁来接这条消息"。
- **无法表达"这一步只接受这个人的回复"**：`Handle` 拿到的 `Event` 已经是全集，判断条件散落在插件里。

#### 建议的实现形态

核心持表、peer 级、一次性、可持久化：

```sql
CREATE TABLE sessions (
  peer       TEXT PRIMARY KEY,   -- 同一会话同时只允许一个插件持有
  plugin     TEXT NOT NULL,
  state      BLOB NOT NULL,      -- 插件自定义，建议 JSON
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
```

路由顺序（`Core.Handle` 内，`handleReserved` 之后、`dispatch.Dispatch` 之前）：

1. 仅对 `KindMessage` 查一次 `sessions[peer]`（主键点查；可用内存集合做"有无待处理会话"的快速判断，避免每条消息一次 SQL）；
2. 命中且插件仍在加载 → 直接投递给该插件，`Event.Session` 非空，跳过静态匹配；
3. 命中但插件已卸载 → 删行、warn、落回正常路由；
4. 投递完成即删行（一次性语义，等价于 `temp=True`）。

插件侧契约（`Host` 新增，APIVersion 7）：

```go
type Session interface {
    // Await 登记"该 peer 的下一条消息交给本插件"，并附带 state。
    // 同 peer 已有其他插件持有时返回 ErrSessionBusy，由插件决定提示还是放弃。
    Await(ctx context.Context, ev *Event, state []byte, ttl time.Duration) error
}

type Event struct {
    ...
    // Session 非空表示这条消息是对本插件 Await 的回应。
    Session *SessionContext // { Plugin string; State []byte }
}
```

与现有设计的一致性：

- 与 `Notice` 的做法同构：平台事实在 gateway 归一化成**语义标记**，核心不比较平台事件名；
- 与 `PeerKV` 同构：会话状态按 peer 存，`/reset session`（R1）顺手清 `sessions`，否则重置后会留下一个永远等不到输入的悬挂会话；
- 与 `Dispatcher` 的"同 peer 串行"天然契合：同一 peer 的会话推进不会被并发打乱。

#### 代价与风险

- 每条消息多一次主键点查（SQLite 上是微秒级；用内存集合可降到零）；
- 新增一个用户可见概念（"会话接管"），文档与 Trace 都要覆盖（建议在轨迹里打印 `session=plugin`）；
- `ErrSessionBusy` 的语义要定清楚：**覆盖**会让先来的插件被静默饿死，**拒绝**会让后到的插件报错。推荐拒绝 + `Info` 日志，把选择权留给插件；
- 会话是"隐式路由"，可观测性会变差。必须能在 `/group status` 或 Trace 里看到"当前有哪些 peer 处于会话中"。

**如果不做**：多轮交互会继续以"每个插件自己发明一套"的方式扩散。以 Torn 插件的复杂度，这一天不会太远。

---

### 2.2 命令解析归位：把命中结果交回插件

#### 现状

`Router` 已经做了前缀/正则匹配，并把命中的 label 放进 `Match.Command`（`router.go:141`），但 `Dispatch` 调用 `Plugin.Handle(ctx, ev)` 时**没有把这个信息传下去**。于是插件必须再解析一遍：

- `plugins/md/md.go` 的 `parse()` 自己 `CutPrefix("/md")`，还要额外判"指令词是否独立成词"，注释里写明"Manifest 的前缀匹配是裸的 HasPrefix，`/md5 abc` 也会命中"；
- `plugins/echo/echo.go` 在一个 `Handle` 里用 `if strings.HasPrefix` 链分派两个命令。

后果有三个，且都是结构性的：

1. **声明与实现可以不一致而不报错**：Manifest 写 `/md`，`Handle` 判 `!md`，编译通过、启动通过，只是永远不命中。
2. **命令级 `Permission` / `DenyReply` 由核心解析，但插件不知道用户走的是哪条命令**，做二次判断时只能重算。
3. **参数提取各写各的**。`strings.Fields` 的 cap 估算坑已经在别处踩过（`argsAfter` 才修正），说明这不是假想问题。

#### 建议

最小改动：`Event` 增加两个字段，由 `Router.Match` 在命中时填入。

```go
type Event struct {
    ...
    Command string // 命中的命令 label（前缀原文或正则源码）；未命中为空
    Args    string // 命令之后、已 TrimSpace 的剩余原文
}
```

`Handle` 里按 `ev.Command` 分派，`md` 的 `parse()` 可以整体删掉，`/md5` 的边界问题由路由层统一处理。

同时建议给 `Command` 增加**词边界**语义（对应 nonebot 的 `force_whitespace`）：

- 现状：`strings.HasPrefix(content, "/md")` → `/md5` 命中；
- 建议：前缀后必须是空白或结尾（或 `Command.Loose=true` 显式放宽）。

这是行为变更，需要和 §2.1 一起放进 APIVersion 7，或单独作为一个语义修正票。

**不建议**引入 nonebot 那种"多 handler 列表"。Go 的 `Manifest` 是纯数据、可在启动期校验；把函数塞进去会破坏这个性质，且本项目的插件数量不需要那么多分层。单 `Handle` + `ev.Command` 能覆盖 80% 的重复。

---

### 2.3 `Broadcast` 应是层级属性，不是中标者的属性

#### 现状

`dispatch.go:140-153`：

```go
top := matches[first]
chosen := matches[first : first+1]
if top.Manifest.Broadcast {
    for _, m := range matches[first+1:] {
        if m.Priority != top.Priority { break }
        ...
        chosen = append(chosen, m)
    }
}
```

`top` 由 `router.go:146-151` 的排序决定：priority 降序，**同优先级按插件名升序**。于是：

- 插件 `aaa`（priority 100，`Broadcast=false`）与 `zzz`（priority 100，`Broadcast=true`）同时命中；
- `aaa` 排在前面成为 `top`，`zzz` 永远不执行；
- 没有任何日志或错误提示这一点。

也就是说，"这一层要不要扇出"是**层级属性**，却被实现成"排序第一名的个人属性"，并且结果依赖名字字典序。

#### 两个修法

| 方案 | 改动 | 语义 | 代价 |
|---|---|---|---|
| A. 层级任一为真 | `broadcast := slices.ContainsFunc(level, ...)` | 与 nonebot 的"同级并行 + block 停在下层"最接近 | 一行的改动，但仍是"别人替我决定" |
| B. per-plugin `Block` | `Command.Block`/`Manifest.Block`，默认 `true`；按序执行直到遇到 `Block=true` | 每个插件自己决定是否让位 | 语义更清晰，改动面更大，需要改文档与测试 |

推荐 **B**，因为 A 只是把耦合从"一个人"变成"一群人"：只要有一个插件声明 `Broadcast`，其他插件的 `Block=false` 意图仍然无法表达。B 的默认值保持现状（默认阻断），迁移成本接近零。

`ARCHITECTURE.md:216` 的"单条消息只允许一个插件回复（除显式 `broadcast: true`）"要同步改成"由每个匹配者自己声明是否让位"。

---

## 3. 明确不建议借鉴的四项

### 3.1 动态插件加载 / `entry_points`

P1（编译期注册）是已定稿决策，理由在 `ARCHITECTURE.md:150-166`：无 IPC、无版本错配、故障面最小。nonebot 的动态加载换来的是"免重编译"，代价是 Python 基线内存与加载顺序、循环依赖、热重载状态丢失。本项目的 `/reload` + `tools/devrun` 已经把"改插件要重编译"这件事的成本压到可接受范围，没有必要为它引入 P2。

### 3.2 依赖注入框架

nonebot 的 `Depends`/`Dependent` 是为了让 handler 签名与框架解耦（`ArgPlainText`、`RegexGroup`、`T_State` 等按需注入）。Go 没有运行期类型反射的等价物，实现一套只能靠代码生成或 `any` 断言，收益不抵复杂度。**显式参数（`ev`、`h`）在 Go 里更可读、更好测**，保持现状。

### 3.3 Driver / Adapter / Bot 三层

单平台不需要抽象。值得记住的是 seam 在哪：新增平台时，改动应止于 `internal/gateway`（解码 + 发送）与 `internal/api`（HTTP），核心与插件只认 `host.Event` / `host.Reply`。这条边界目前守得住，不需要提前抽象。

### 3.4 运行期增删 matcher

nonebot 的 matcher 表是全局可变的，`destroy()` 直接改列表。换来的灵活性在 Go 里会带来三倍成本：并发安全、可观测性、以及"重启后 matcher 表消失"。会话原语（§2.1）用**持久化的待处理会话行**替代临时 matcher，正好绕开这三点。

---

## 4. 不依赖 nonebot 的自身问题

### 4.1 契约字段是死代码：`Capabilities` 与 `ConfigSchema`

事实（grep 全仓）：

- `Manifest.Capabilities` 只在 4 个插件里被**写**，核心从未读取；`NewService` 的 `Options` 里根本没有这个字段（`host/service.go:72-85`）；
- `Manifest.ConfigSchema` 同样只被写，从未校验；
- 但 `host.go:5` 的包注释写着"**能力清单即权限清单**"，`ARCHITECTURE.md:179` 写着"配置项声明，用于启动时校验"。

这是**文档承诺大于实现**。按项目"文档是事实源"的规矩，两者必须对齐，二选一：

- **实现**：`ConfigSchema` 只有 `string/bool/int/duration` 四种，启动期校验存在性与类型，约 20 行；`Capabilities` 用于装配期判定——声明了 `sender.active` 才注入可发主动消息的 `Sender`，没声明 `http` 却声明了 `AllowedDomains` 直接报错。后者能把"插件悄悄多要一个能力"变成装配失败。
- **删除**：字段删掉，注释改成"保留字段，当前仅作文档用途"。

倾向实现 `ConfigSchema`（真能防"配置写错但启动成功"），`Capabilities` 暂时降级并显式标注，等有第二个真实消费方再启用。

### 4.2 `@提及` 被丢弃，路由无法表达"只响应 @"

- `gateway/event.go:120` 解出了 `Mentions`，但 `decodeMessage`（`:165-199`）没有映射到 `host.Event`；
- `ARCHITECTURE.md:78` 明确把 `GROUP_AT_MESSAGE_CREATE` 与 `GROUP_MESSAGE_CREATE` 归一化为同一类，下游不区分；
- 但 `ARCHITECTURE.md:177` 又写着 `commands` 支持"前缀 / 正则 / **@提及**"，而 `host.Command`（`host.go:279-292`）只有 `Prefix` 与 `Pattern`。

三处互相矛盾。实际后果：全量模式开启的群里，机器人无法便宜地判断"这条消息有没有 @ 我"，只能对每条消息跑路由；插件也拿不到这个信号（`Name` 里有，但核心不许比较平台事件名）。

建议按 `Notice` 的既有做法补一个语义标记，在 gateway 填：

```go
// Mentioned 表示平台明确标记这条消息 @ 了机器人。
// 与 Notice 同构：平台事件名只在 gateway 出现，核心只认语义。
Mentioned bool
```

成本一行；收益是插件可以写 `if !ev.Mentioned && ev.Scene == SceneGroup { return nil, nil }`，而不必给机器人开全量模式后再用正则过滤。

### 4.3 已知事件不保留原始载荷

`Event.Payload` 的注释写明"仅未知事件保留"（`host.go:85`）。已知事件解析后原文丢弃，意味着：**平台新增任何字段，都需要改核心 + 升 APIVersion，插件无法自救**。这与 nonebot 相反（事件是完整的 pydantic 模型，插件可读任意字段）。

建议：`Event.Raw json.RawMessage`，对所有事件保留并截断到 32 KiB，**不落库**，仅在 dispatch 生命周期内有效。内存代价约等于消息体积（通常 < 2 KiB）。这条与 §4.2 是同一类问题：**窄接口的代价是平台演进时核心必须先动**。

### 4.4 主动消息日预算只在内存

`sender.go:56-59` 的 `activeUse/activeDay` 是进程内 map；`sendActiveOne` 的注释（`:266-267`）说"先扣预算再上传"。但**进程重启即清零**，与 `sender.go:19-21` 声称的"避免重启循环之类的异常把额度烧掉"正好相反。

现有缓解是 `bootnotice` 自己在 KV 里记 `min_interval`，属于插件级补丁，不覆盖其他主动消息发送者。

建议：把日预算落到 `plugin_kv` 或独立表 `active_usage(day, scene, peer, used)`，启动时读回。成本很小，收益是注释与行为一致。

### 4.5 调度器全局串行

`scheduler.go:200-213` 的 `runDue` 是 `for` 循环顺序执行；`DueJobs` 单轮上限 20（`store/jobs.go:76-85`）。后果可以量化：

- per-peer 任务在 N 个群上展开后有 N 行；若每个任务耗时 3 s，20 个群就是 60 s；
- 而 tick 间隔是 30 s（`scheduler.go:31`），一轮扫不完，下一轮必然迟到；
- 单个任务超时上限 60 s，最坏情况会阻塞同轮**所有**其他插件的任务。

建议：按 `(plugin)` 分桶串行（保证同一任务不重叠），桶间并行，信号量 4；`DueJobs` 的 limit 提到 100。这样既保留"同一任务不重入"的必要约束，也不会让一个慢任务拖住全局。

### 4.6 `msg_seq` 在窗口过期后从 1 重来（低风险，需核实）

`sender.go:173-198`：`seq` 就是窗口内的 `used` 计数；窗口过期后 `used=0`，同一 `msg_id` 的 `seq` 会重新从 1 开始。平台约束是"`msg_id + msg_seq` 全局唯一"。若 `msg_id` 在窗口过期后已不可用，则无害；若平台对唯一性的判定跨越更长周期，这里会撞。

保守做法：`seq` 单调递增、不随窗口重置（窗口只重置"剩余次数"）。改动 3 行，无副作用。

### 4.7 文档与实现的三处漂移（汇总）

| 位置 | 文档说法 | 实现 |
|---|---|---|
| `ARCHITECTURE.md:185` | 两条同优先级正则同时匹配，**启动即报错** | 不检查；按插件名字典序取第一个（§2.3） |
| `ARCHITECTURE.md:266` | 同一**插件**连续失败 5 次 → 自动禁用该插件 | 熔断只存在于 `scheduler.go` 的**任务**维度；`Handle` 失败只打日志（`dispatch.go:193-198`） |
| `ARCHITECTURE.md:179` | `config_schema` 用于启动时校验 | 从未读取（§4.1） |

按项目铁律"改架构先改文档再改代码"，这三处应当先修文档（承认现状）或先补实现，不能继续两套说法并存。

---

## 5. 建议的落地顺序

1. **文档对齐**（§4.7）——半天。消除"文档骗人"，这是后续所有决策的地基。
2. **`Broadcast` 修正**（§2.3，方案 B）——改动小、含测试，能立刻消除"名字排序决定行为"的隐式依赖。
3. **`Event.Command/Args` + 词边界**（§2.2）——不动 `Handle` 签名，插件可逐个迁移；与 2 一起进 APIVersion 7。
4. **`Mentioned` + `Raw`**（§4.2 / §4.3）——各一行到几十行，先解决"平台字段演进必须改核心"的结构性问题。
5. **日预算落库 + `seq` 单调**（§4.4 / §4.6）——小修，风险低。
6. **会话原语**（§2.1）——单独立 ADR（编号 0004），因为它引入新的用户可见概念与一张新表，值得完整记录取舍。
7. **调度并发**（§4.5）——等 per-peer 任务真的铺开再做，当前任务数下收益有限。

前 5 项可以并行，6 需要单独设计评审。

---

## 6. 结论

nonebot2 的价值不在于它的实现（Python 生态、全局可变注册表、异常控制流、会话不隔离，这些都不适合直接搬到 Go），而在于它把"事件 → 响应器 → 状态 → 会话"这条链路的**职责划分**想得很清楚：

- 匹配是**数据**（rule/priority/block），不是代码；
- 交互是**可中断的流程**，不是一次函数调用；
- 状态跟着**会话**走，不是跟着插件走。

go_qqbot 已经在前两点上做到了"用数据描述匹配"，缺的是第三点的运行时支撑。补上会话原语之后，这套架构在能力上就基本对齐了，而在一致性、可观测性与资源占用上仍明显优于参照物。
