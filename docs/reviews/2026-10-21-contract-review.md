# 全量代码审查：契约与运行时一致性（2026-10-21）

- 范围：`internal/{core,store,gateway,api,httpx,host,config,render,instance,logx,termcolor,sysinfo}`、`cmd/bot`、`tools/*`、`plugins/*` 的全部非测试代码（15,139 行）。
- 方法：先建跨边界契约地图（Manifest ↔ 路由、配置字段 ↔ 消费点、SQL ↔ Go 结构、平台文档 ↔ 实现），再逐模块通读；每条结论都做过一次证伪。
- 基线：`go test ./...` 全绿（19 个包，本机 2026-10-21 实测）；`go vet` 无告警。
- 结论：**2 个 P1、11 个 P2、26 个 P3**。P1 都是"进程还活着、但机器人永久不工作"这一类；P2 集中在平台契约（关闭码、err_code）、关停时序与配额口径。没有发现数据被静默清空的持久化缺陷。

---

## 0. 契约地图（先看差集）

| 边界 | 一侧 | 另一侧 | 核对结果 |
| --- | --- | --- | --- |
| 插件清单 | `Manifest` 11 个字段 | `Router` / `Dispatch` / `Core.Start` / `host.Service` | 除 `Capabilities`（保留字段，文档已声明不读）外全部有消费方 |
| 保留指令 | `reservedPrefixes` 6 项 | `handleReserved` 的 5 个 case（`/reload`+`/restart` 合并） | 严格对应，有测试锁 |
| 全局配置 | 12 个配置段 | `applyDefaults` / 各模块 | `Media.Enabled/MaxFileBytes/QuotaBytes` 是文档化的"保留但未启用"；其余均有读者 |
| SQL | 13 张表 | `internal/store` 的 SQL 字面量 | 列序/占位符/Scan 全部一致；三处旧库重建实测通过（见 §4） |
| 平台 API | 官方 `err_code` 模型 | `internal/api` | **缺失**：只有 HTTP 状态判断，没有业务错误模型（P2-1） |
| 平台 WSS | 官方关闭码表 | `gateway.closeReason` | **只处理了 2 个**，4006/4007 不清 session（P1-1） |
| 文档承诺 | README「401 → 刷新 token」 | `api.InvalidateToken` | **不成立**（P1-2） |

---

## 1. P1：会永久性停摆的两条路径

### P1-1 4006 / 4007 不清 session → 永远 Resume 同一个死 session，重启与 `/reset` 都救不回来

**证据**
- `internal/gateway/gateway.go:451-463`：`closeReason` 对 `4006, 4009` 只包了一层错误文本，**没有调用 `clearSession`**；`4007` 连分支都没有。
- `internal/gateway/gateway.go:188-200`：下一轮 `connectOnce` 只看 `hasSession()`，有 session 就发 Resume。
- `internal/gateway/gateway.go:391-398`：`clearSession` 只在 op9 与"发送 Resume 失败"两处被调用。
- `internal/core/reset.go:22-25` 与 `internal/store/reset.go:214`：任何重置级别都不动 `bot_state`，所以 `/reset` 也不清 `session_id`。

**官方口径**（bot.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/websocket.html）：

| 关闭码 | 含义 | 必须做什么 |
| --- | --- | --- |
| 4006 | 无效的 session id，无法继续 resume | **Identify** |
| 4007 | seq 错误 | **Identify** |
| 4009 | 连接过期 | Resume |

**后果链**：停机超过平台 session 有效期（或升级窗口）→ 启动时 `loadState` 读回 `session_id` → `sendResume` → 平台 close 4006 → 不清 session → 退避 1s…60s → 再次 `sendResume`（同一个死 sid）→ 再次 4006。**永远不发 Identify、永远收不到事件**。日志每 60 秒一条 `session 不可用(4006)`，只能人工改库。

**修法**：在 `connectOnce` 的读错误出口按关闭码做状态迁移，而不是在 `closeReason` 里只改文案：

```go
switch websocket.CloseStatus(err) {
case 4006, 4007, 4900, 4901, 4902, 4903, 4904, 4905, 4906, 4907, 4908, 4909, 4910, 4911, 4912, 4913:
    c.clearSession(ctx)        // 下一轮走 Identify
case 4009:
    // 保留 session，继续 Resume
case 4004:
    c.o.API.InvalidateToken()  // 见 P2-10
case 4914, 4915:
    return fatal               // 不再重连
}
```

同时补一条**表驱动测试**：给定关闭码 → 断言 `sessionID` 是否被清、下一轮是 Identify 还是 Resume。这类映射只有测试守得住。

---

### P1-2 401 之后 token 自锁：`InvalidateToken` 只清内存，下一次又从 `bot_state` 读回同一个坏 token

**证据**
- `internal/api/api.go:149-154`：`InvalidateToken` 只做 `c.token = ""`，**不删持久化的 `access_token` / `access_token_exp`**。
- `internal/api/api.go:112-122`：`c.token == ""` 时从 `state` 读回 token；只要 `access_token_exp` 仍在未来（提前 60s 判定），直接返回，一个网络请求都不发。
- `internal/api/api.go:137-143`：刷新成功时把 token 与 exp 写入 `bot_state`。
- `README.md:487`：「401 → 刷新 token」——实现不成立。

**后果链**：时钟偏差 / 平台提前吊销 / secret 轮换 → 任意接口 401 → `InvalidateToken` → 下一次 `AccessToken` 从库里读回坏 token → 再 401 → 再 Invalidate……**在 `exp` 到期前（最长 7200s）全站鉴权失败**。`connectOnce` 第一步就是 `AccessToken`，所以连 WSS 都建不起来，机器人整段静默。进程重启也无效：`bot_state` 里还是那个 token。

**修法**：`InvalidateToken` 同时删除两个键（或加一个 `trustCache=false` 标志）；`Get/Post/postOnceWith` 的 401 分支用新 token **重试一次**再返回错误。测试：注入一个"永远 401"的 httpx，断言第二次请求带的是新 token、且持久化键已被清。

---

## 2. P2：明确会出错，但有触发条件

### P2-1 发送结果不解析 `err_code`：200/201/202 + 业务错误被当成成功

**证据**
- `internal/api/api.go:318-326`：`SendMessage` → `postOnce(..., out=nil)`，响应体完全丢弃。
- `internal/httpx/client.go:244-248`：只有 HTTP ≥400 才返回错误；`out == nil` 时直接 `return nil`。
- 官方 API 调用指南：**成功时 `err_code` 为 0，失败时响应体包含 `err_code`**，并明确"建议根据 `err_code` 判断请求是否失败"；状态码 201/202 还可能"成功但返回 error body，需要特殊处理"。（access-token 接口更是明确"失败时 HTTP 仍为 200"。）

**后果链**：平台以 2xx + `err_code` 返回失败（消息过期 40034005、内容被风控 1100101、主动消息超限 304045 等）→ 本地记为成功 → 轨迹打印"已发送"、被动配额已扣、主动日预算已扣，用户什么都没收到。排障时所有日志都指向"发成功了"。

**修法**：在 `api` 层引入统一信封：

```go
type envelope struct {
    ErrCode int    `json:"err_code"`
    Code    int    `json:"code"`     // access-token 用 code
    Message string `json:"message"`
}
```

`postOnceWith` 始终解码 body（即使 `out == nil`），`err_code != 0` 时返回 `*api.BusinessError{Code, Message}`；`errors.As` 可判。至少对 `SendMessage` / `UploadMedia` / `GroupInfo` 生效。**先实测确认**消息接口是否真的会 2xx+err_code（见 §5）。

### P2-2 调度器关停时用已取消的 ctx 写回状态：重启后同一任务会再跑一遍

**证据**
- `internal/core/scheduler.go:420`（`run(ctx, ...)` 的 ctx 就是 `s.runCtx`）、`:235-247`（`Stop()` 取消 `runCtx`）。
- `internal/core/scheduler.go:486-489`：`UpdateJob(ctx, j)` 与 `RecordJobRun(ctx, ...)` 用的都是这个已取消的 ctx。
- `internal/core/core.go:220-225`：关停第一步就是 `sched.Stop()`。

**后果链**：`/reload` 或 SIGTERM 时正在跑的任务收到取消 → 任务返回 error → `UpdateJob`/`RecordJobRun` 因 `context canceled` 失败（只留一条 Error 日志）→ `next_run_at` 没推进、`job_runs` 少了这一行 → 5 分钟内重启时 `DueJobs` 仍认为它到期（`missGrace=300s`）→ 同一个 per-peer 任务再跑一次（日推类任务会给群里发两遍）；超过 5 分钟则被判 `skipped`，静默不补。

**当前触发面**：4 个示例插件都没实现 `JobRunner`，今天不会触发；第一个带定时任务的插件上线即生效。

**修法**：收尾写入改用 `context.WithoutCancel(ctx)` + 2s 超时，只让"领任务/执行"受取消影响：

```go
wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
defer cancel()
_ = s.db.UpdateJob(wctx, j)
_ = s.db.RecordJobRun(wctx, ...)
```

### P2-3 失败的 reset 被记成 `done`：审计谎报 + 冷却按"成功"计时

**证据**
- `internal/core/reset.go:255-266`：失败分支写 `store.ResetStatusDone`，detail 为 `"失败: " + err`；无 pending 的路径走 `AuditResetDone`（恒 done）。
- `internal/store/reset.go:397-411`：`LastResetDoneAt` 只认 `status='done'` 的 `finished_at`。
- `internal/core/reset.go:408-424`：冷却起点就是这条记录。
- `internal/core/reset.go:455`：`/reset status` 把 done 显示成"已执行"。

**后果链**：R3 执行失败（取媒体路径失败、库错误）→ 审计里一条 `done`，`/reset status` 显示"已执行" → 24 小时冷却挡住重试 → 用户看到"重置失败"+"该级别冷却中，还需等待 23 小时"，只能改库。R2 是 1 小时、R1 是 10 分钟。

**修法**：增加 `ResetStatusFailed`（或失败写 `cancelled` 并在文案上区分），`LastResetDoneAt` 只认成功状态；`AuditResetDone` 增加 status 参数。

### P2-4 主动消息日预算按**进程本地时区**切日，cron 按 `Asia/Shanghai`，部署单元没有 `TZ`

**证据**
- `internal/core/sender.go:384-388`：`Day: s.now().Format("2006-01-02")`，`s.now = time.Now`（本地时区）。
- `internal/store/schema.go:102-108` / `internal/store/active.go:12-15`：注释明确写"本地时区的 YYYY-MM-DD"，store 只接收字符串。
- `internal/core/scheduler.go:66-71`：cron 显式 `time.LoadLocation("Asia/Shanghai")`。
- `deploy/qqbot.service`：只有 `Type/User/...`，**没有 `Environment=TZ=Asia/Shanghai`**（Debian 12 默认 UTC）。

**后果链**：TZ=UTC 的机器上，cron 按北京时间跑、预算桶在北京时间 **08:00** 换。平台配额按自然日算，于是同一个平台自然日内可以花掉"前一天 UTC 桶的余额 + 08:00 后的新满额"，最多 **2×limit**，正好击穿 `sender.go:349-352` 那句"宁可少发一条，也不能把平台每天 1000 条的配额当成无限量烧掉"。

**修法**：把 scheduler 的 `*time.Location` 注入 `Sender`（`ActiveUsageKey.Day` 用同一个 loc 格式化）；或在 systemd 里钉 `Environment=TZ=Asia/Shanghai`（前者更彻底，不依赖部署环境）。

### P2-5 没有 READY / RESUMED 超时：连接活着、心跳有 ACK，但永远收不到事件

**证据**
- `internal/gateway/gateway.go:177-182`：只有 hello 有 20s 截止。
- `internal/gateway/gateway.go:207-260`：发完 Identify/Resume 后直接进入 `ws.Read` 循环，**没有任何握手定时器**。
- `internal/gateway/gateway.go:317-339`：心跳只要收到 ACK 就继续，不会因为"从未 READY"而断开。

**后果链**：平台接受 WS、正常回心跳 ACK，但 Identify 未被授权/READY 帧丢失 → 读循环永远阻塞 → `Run` 永不返回、不触发重连。表现为"进程在线、心跳正常、一条消息都不回"。

**修法**：Identify/Resume 后起一个 30s 握手定时器，超时未收到 READY/RESUMED 就关连接；连续 3 次"连上不到 5s 就断"则强制等 60s（官方 SDK 的做法）。

### P2-6 回复用的 5s ctx 压掉了上传的 30s 超时：大图必然超时，且配额已扣

**证据**
- `internal/core/dispatch.go:325`：插件回复用 `context.WithTimeout(ctx, 5*time.Second)`。
- `internal/api/api.go:26-29`、`:308-310`：上传用 `hcSlow`（30s client timeout）。
- `internal/core/sender.go:151-168`：先上传、再占被动配额。

**后果链**：`WithTimeout(30s)` 只能放宽 `http.Client.Timeout`，**放不宽 ctx**。5MB 图 base64 后约 6.7MB，在 10 Mbps 上行需要 5.4s → `DeadlineExceeded` → 图片永远发不出。主动消息的日预算已经先扣掉（`sender.go:344-357`）；被动回复的配额没扣（上传在 `reserve` 之前），但"文字已发出、图没跟上"的错误插件拿不到。用户侧看到"只有文字没有图"或什么都没有。

**修法**：富媒体分支用独立预算（`Reply` 文本 5s，含上传时 30s），或按大小/带宽预估后降级。注意 `host.Sender.Reply` 的错误插件拿不到，必须在核心内完成。

### P2-7 关停没有 dispatch / GC 的汇合点：`db.Close()` 可能与在途写并发

**证据**
- `cmd/bot/main.go:246-255`：只等 `gwDone`（网关读循环），超时 5s 后照常往下走。
- `internal/core/core.go:215-234`：`Shutdown` 只等调度器与插件，**不等 Dispatcher 的 per-peer 协程，也不等 `gcLoop`**。
- `internal/core/core.go:205`：`go c.gcLoop(ctx)` 从不等待。
- `cmd/bot/main.go:134`：`defer db.Close()`。

**后果链**：网关 5s 超时 → `Shutdown`（无任务时微秒级返回）→ `run` 返回 → `db.Close()`，而 peerLoop 里 `Core.Handle` 还在 `MarkEventSeen`/`InsertMessage`、`Sender` 还在写配额账本 → 一串 `sql: database is closed`；同一条消息的回复/落库中断。网关自己的 `saveState` 也可能落在关库之后 → `session_id`/`seq` 丢 → 下次冷 Identify，白烧一次 session 配额。

**修法**：`Dispatcher` 加 `Wait(ctx)`（peerLoop 计数），`Core.Shutdown` 在停调度后调用；`gcLoop` 同样纳入；`db.Close` 前若 `Wait` 超时，退出码改非 0（见 P3-23）。

### P2-8 单实例锁按「库路径」划分，与它自己声明的动机（appID 级 session）不一致

**证据**
- `internal/instance/instance.go:1-11`：动机写的是"v2 网关的 session 按 appID 分配，两个进程会互相顶掉"。
- `cmd/bot/main.go:105-120`：锁键是 `cfg.Store.Path + ".lock"`，注释还把"不同 store.path 可以同时跑"当成特性。

**后果链**：两份配置同一 `app.id`、不同 `[store].path`（测试库与正式库并存、或换了数据目录但旧进程还在）→ 两个进程都拿到锁 → 同一 bot 两条 WS，一条被 4902 reset by resume；两边幂等表各记一份 → **同一条消息回两遍或一遍都不回**。

**修法**：先拿一把 appID 维度的锁（`%TEMP%/qqbot-<sha256(appID)[:8]>.lock`），再拿 DB 锁；或把 appID 写进锁文件并在 Acquire 后比对，不一致时拒绝启动。

### P2-9 出站重定向不复核白名单：一次 302 就能把请求带去内网

**证据**
- `internal/httpx/client.go:192-225`：只在入口 `checkParsedURL` 一次，`c.hc.Do(req)` 默认跟随重定向。
- `internal/httpx/client.go:266-287`：校验逻辑只看初始 URL。
- `internal/httpx/client.go:75-79`：`http.Client` 没有 `CheckRedirect`；`WithTimeout` 克隆时也不会补。

**后果链**：插件只声明了 `api.example.com`；该域被攻陷/存在开放重定向 → `302 Location: http://127.0.0.1:8500/...` 或 `http://169.254.169.254/...` → httpx 直接跟过去，`AllowedDomains` 形同虚设（SSRF）。同机的 WebDAV/aria2 正好在监听。

**修法**：每个 client 设 `CheckRedirect`，对每一跳重跑 `checkParsedURL` 并拒绝 scheme 变化；`WithTimeout`/`WithDomains` 克隆时一起复制。更强隔离需解析后校验 IP 段。

### P2-10 关闭码分类只覆盖了 4 个：4004 / 4008 / 4914 / 4915 / 4013 / 4014 全走通用退避

**证据**
- `internal/gateway/gateway.go:451-463`：只识别 4006/4009/4914/4915，且 4914/4915 被注释成"intents 非法，检查 bot.intents"（官方含义是"机器人已下架/已封禁"）。
- `internal/gateway/gateway.go:101-108`：所有错误统一 1s→60s 退避。
- `internal/gateway/gateway.go:88-90`：只要收到过 READY 就把退避复位成 1s（即使连接只活了 200ms）。

**后果链**
- 4004（token 失效）：继续用缓存 token Identify，在 `exp` 前每次连接都 4004（与 P1-2 叠加）。
- 4008（发送过快）：官方要求等连接返回的频控信息（通常 60s），代码 1s 后就重连，几乎必然再次 4008，还把 `/gateway/bot` 一起打。
- 4914/4915（下架/封禁）、4013/4014（intent 非法/无权限）：永久性错误，却每 60s 重连一次，日志还在指错方向（让人去改 intents）。

**修法**：按官方错误表做三分类——`fatal`（4001/4002/4010–4014/4914/4915，记一条可读错误后返回，交给 systemd `StartLimit`）、`re-identify`（4006/4007/4900–4913）、`resume`（4009）；4004 清 token，4008 固定 60s。

### P2-11 429 不校正本地账本、不冷却，1 秒后重试等于续期惩罚

**证据**
- `internal/httpx/client.go:240-243`：429 与 5xx 一起归入"可重试"，按 1s/2s/4s 退避。
- `internal/httpx/client.go:209`：每次重试仍从桶里取令牌，不扣罚、不读 `Retry-After`。
- `internal/core/sender.go:169-174`、`:363-367`：只把 429 包成 `ErrRateLimited`，没有 per-peer/per-bot 冷却。

**后果链**：平台惩罚窗口通常 ≥60s，本地 1s 后就重试，3 次尝试全撞在惩罚期，还把域名桶的令牌消耗掉，挤掉同域其他请求（如群名刷新），惩罚被反复续期。

**修法**：429 时把该桶令牌清零并至少冷却 `Retry-After`（没有就 60s）；core 侧对 `(scene, peer)` 记短期冷却，冷却期内直接 `ErrRateLimited` 不出网。

---

## 3. P3：小问题与契约瑕疵

### 3.1 渲染（视觉问题，不影响进程）

| # | 位置 | 问题 | 后果 | 修法 |
| --- | --- | --- | --- | --- |
| P3-1 | `internal/render/container.go:87-102`，对照 `renderer.go:362-369` | 列表项内的 `Paragraph` 走 `textBlock`，绕过了 `paragraphLines` 的 `gap(0)` + `bottom(mbPara)` | 松散列表的第二个段落零间距；代码块/嵌套列表之后的段落间距丢失（实测同结构放进列表比顶层少 32px） | `Paragraph` 分支改调 `l.paragraph(v)`；`TextBlock`（紧凑列表）保持原样 |
| P3-2 | `internal/render/table.go:207-216`、`:315-322` | `padH` 硬下限 4、列宽硬下限 1，`ncol×9 > avail` 时总宽必然溢出；没有任何超宽信号 | ≥113 列时表格右侧被画布裁掉，读者不知道后面还有列（QQ 正文长度下几乎不可达） | `total > avail` 时先等比缩列宽（允许 <1），仍不行则整体缩放或返回 `RenderTooLargeError` |
| P3-3 | `internal/render/inline.go:174-195`；对照 `code.go:49-68` | 行内路径不处理 tab/C0 控制字符；`code.go` 明确把 tab 展开成 4 空格，行内没有 | 正文/表格/行内代码里的 tab 画成缺字方框，并误报"字体子集外"warn | `glyphs()` 里 tab 展开、`unicode.IsControl` 直接跳过（不计 adv、不记 missing） |
| P3-4 | `internal/render/inline.go:196-204` | 行内代码药丸的 `padL` 绑在 span 首个字形上，折行后续行首字形没有 `padL` | 续行药丸左缘压在首字上（视觉裁切），宽度不丢 | 折行后给每行首字形补 `padL`（不动 `adv`） |
| P3-5 | `internal/render/inline.go:166-172` | `if l.muted { col = colMuted }` 无条件覆盖 `link` 的强调色 | 引用块里的链接与正文同色，静态图里唯一的链接线索消失 | 改成 `if s.link { … } else if l.muted { … }` |
| P3-6 | `internal/render/inline.go:227` vs `:365-368` | 折行判决用 `Σ GlyphAdvance`（不含 kern），绘制/`MeasureString` 含 kern | 长行提前 2–5px 断行；run 边界留 1–3px 缝 | 推进量里加 `face.Kern(prev, r)/64`，或整行单 Drawer 顺序绘制 |

### 3.2 契约与运行时

| # | 位置 | 问题 | 后果 | 修法 |
| --- | --- | --- | --- | --- |
| P3-7 | `host/config_schema.go:14-16` vs `host/host.go:484-489` | schema 文档说 `key = ""` 是"存在但为空"、对 string 合法；`Params.Str` 却把 `""` 当未设置返回默认值 | 声明为必填的字符串键被配成空串：装配期通过，插件拿到默认值（如 `api_base` 想置空，结果仍打生产地址） | `Str` 改成 `if v, ok := vals[key].(string); ok { return v }`，把"缺失"与"空"分开 |
| P3-8 | `host/service.go:215-226` | `QueryStore` 用"以 select/with 开头"做只读校验：`WITH … DELETE` 可通过；`strings.Contains(lower," limit ")` 对 `LIMIT\n` 误判，会拼出双 LIMIT | 插件可写库（虽然插件本来就能 import store，属软边界）；合法查询被拼坏 | 用 SQLite `PRAGMA query_only=ON` 的只读连接；LIMIT 用解析或直接不拼（由 `QueryRows` 限制行数） |
| P3-9 | `core/router.go:94-99` | 保留前缀用 `HasPrefix(prefix, "/group")`，`/groupinfo`、`/reset2` 也被拒绝 | 插件作者莫名其妙加载失败，注释说的却是"声明相同前缀" | 按词边界比较（`prefix == rp` 或 `strings.HasPrefix(prefix, rp+" ")`） |
| P3-10 | `core/core.go:153-162`；`config/config.go:56` | `Manifest.Intents` 与 `[bot].intents` 位掩码没有装配期交叉校验 | 插件声明 `notice` 但配置没开对应平台 intent：路由表里有、网关收不到，启动日志一切正常 | `Core.Start` 里把 Intent.Kind 映射到平台位，与 `cfg.Bot.Intents` 求交，缺位 fail-fast 或 Warn |
| P3-11 | `gateway/gateway.go:228-230`、`:400-405` | 收到帧就 `setSeq` 并异步落盘，然后才 `Handler`；10s 节流窗口内崩溃可能"已落盘但未处理" | Resume 从已确认 seq 之后开始 → 丢一条事件（去重表只能挡重复，挡不了丢） | 按"处理完成"推进可确认 seq，或至少把落盘放在 `Handle` 返回之后 |
| P3-12 | `store/schema.go:175`、`store/messages.go:68-73` | `idx_job_runs(job_id, started_at)` 从未被任何查询使用；GC 按 `started_at` 删却只能全表扫 | 唯一按"每次执行"增长的表，每 10 分钟最多 20 次全扫 + 维护无用索引 | 换成 `idx_job_runs_started(started_at)`；新索引放在迁移尾部（旧库补列之后） |
| P3-13 | `store/kv.go:66-70`、`schema.go:22-30` | `PurgeExpiredKV` 每 10 分钟全表扫 `plugin_kv`，`expires_at` 无索引 | 插件缓存上万行时持续 IO 抖动 | `CREATE INDEX … ON plugin_kv(expires_at) WHERE expires_at IS NOT NULL` |
| P3-14 | `store/messages.go:82-92` | `purge()` 把 `RowsAffected` 的错误吞成 `(0, nil)` | GC 用 `n < batch` 判断是否继续，计数失败会被读成"删完了"，剩余行等下一轮；错误无痕 | 返回 error，或至少 `log` |
| P3-15 | `store/messages.go:94-102`、`store/jobs.go:141-149` | `DeletePeerMessages`（会绕过 media/sessions/job_runs）与 `CountJobRuns` 零调用，注释还停在"脚手架阶段尚未接指令" | 下一个人做 R2 最可能误用前者，留下孤儿 media 行与文件 | 删除，或转发到 `ClearPeerData` 并改名 |
| P3-16 | `store/reset.go:73-82`、`:171-176`、`:277-281` | R2 的 media 删除/预览都用 `JOIN messages`（INNER），`message_id IS NULL` 的行既不预览也不删 | 一旦 media 恢复写入，R2 会留下行和文件，预览说"媒体 0 个" | 改成 `message_id IS NULL OR message_id IN (…)`，或写入时强制非空 |

### 3.3 工具链与运维

| # | 位置 | 问题 | 后果 | 修法 |
| --- | --- | --- | --- | --- |
| P3-17 | `README.md:318`、`:474`；`cmd/bot/main.go:6`；`.gitignore:38` | 生产构建命令没有 `go generate`，生成物不入库 | 新放的私有插件不进二进制：构建成功、启动成功、`plugins=4` 不变，只能靠"指令没反应"发现 | README 改成 `go generate ./cmd/bot && go build …`；启动日志打印插件名而不只是数量 |
| P3-18 | `tools/devrun/main.go:137-150` | Windows 先 `bin→old` 再 `tmp→bin`；第二步失败无回滚 | 一次无人值守的 `/reload` 可能让二进制消失、进程下线，而 `bot.old` 就在旁边却不被使用 | 第二步失败时把 `old` 改回 `bin`；`run()` 发现 `bin` 缺失但 `.old` 存在时先恢复 |
| P3-19 | `tools/genplugins/main.go:78-107` | 只扫直接子目录；目录有 `package` 就 blank import，不校验是否真的 `host.Register` | 插件代码放在 `plugins/foo/plugin/*.go`，或目录是共享库 → 构建全绿、插件永不注册 | 追加"该目录（含一层子包）必须出现 `host.Register(`"校验，否则 exit 1 |
| P3-20 | `cmd/bot/main.go:113-120`；`internal/instance/instance.go:94-97` | 文档说"第二个实例该用 wait=0"，实现恒传 2s | 双开时白等 2s（仅体验） | `if RestartedByReload() { 15s } else { 0 }` |
| P3-21 | `cmd/bot/main.go:177` | `signal.NotifyContext` 注册在字体解析（约 300ms）、开库、迁移之后 | 这段窗口收到 SIGTERM 走默认处理，会话不落盘、systemd 看到被信号杀死 | 把信号注册提到 `config.Load` 之后 |
| P3-22 | `cmd/bot/main.go:231-237` | `ctx.Done()` 与 `/reload` 同时就绪时 Go 随机选；选中 req 就把 Ctrl-C 变成重启 | 极窄窗口内关停变重启 | select 后先判 `ctx.Err() != nil` |
| P3-23 | `cmd/bot/main.go:249-251`、`:257-266` | 网关/调度器关停超时只 Warn，退出码恒 0 | systemd `Restart=on-failure` 看不到"关停卡死" | `Shutdown` 返回 error/bool，超时 `os.Exit(1)` |
| P3-24 | `plugins/adminping/adminping.go:77-80` | `EqualFold(fields[0], "/ping")` 是死分支：路由层 `CutPrefix` 区分大小写，`/PING` 到不了 Handle | 当前不会错，但是"插件自己再判一次"的假保险 | 删掉 `isPing`，用 `ev.Command`（与 echo/md 一致） |
| P3-25 | `internal/sysinfo/sysinfo_windows.go:73`、`sysinfo.go:122`、`plugins/adminping/adminping.go:123` | 结构体声明了 `PeakWorkingSetSize` 却从不读取；`ProcessRSS==0` 时整段本进程内存从 `/ping` 消失 | `/ping` 永远没有峰值 RSS；采集失败静默隐藏 | 加 `ProcessPeakRSS`（Windows `PeakWorkingSetSize` / Linux `VmHWM`），失败时显式显示"采集失败" |
| P3-26 | `cmd/bot/config_example_test.go:22-31` | 遍历 `host.Registered()`（含私有插件）去校验入库的 `config.example.toml` | 私有插件一旦声明 `ConfigSchema`，核心仓示例配置必须泄露它，否则开发工作区 `go test ./cmd/bot` 恒红，clone 上却全绿 | 只对示例插件做 schema 校验；私有插件缺段时 Skip 并记日志 |

---

## 4. 已核实不是问题（不要重复怀疑）

这些点都被重点怀疑过，逐一证伪：

1. **`dispatch.go:85-90` 的 `if !timer.Stop() { <-timer.C }` 不会死锁。** 本机 Go 1.26.1 实测：timer 到期但值未被接收时 `Stop()` 返回 `true`，drain 分支不会进入（5/5 轮）。这个旧习语在 Go 1.23+ 语义下是安全的。
2. **4009 保留 session 是对的**，不要"顺手统一"成 4006/4007 的清 session——官方明确 4009 应 Resume。
3. **`Media.Enabled/MaxFileBytes/QuotaBytes` 是文档化的保留字段**（`config.go:72-76`、ARCHITECTURE §4），不是死配置；`Media.Dir` 仍被 `removeMediaFiles` 使用。
4. **`Manifest.Capabilities` 不读是设计**（`host.go:8-9`、ARCHITECTURE §3.2/§4.1），不是漏实现。
5. **SQL 列序/占位符全部一致**：13 张表逐表核对；三处易错点多列/字面量混排（`AuditResetDone` 9 列 = 7 占位符 + 2 字面量、`OpenResetPending`、`SetGroupName` 的 CASE 双 `?`）都正确。
6. **旧库迁移实测通过**：用 `git show 2ce990b` 的原始 DDL 造旧库 → `Migrate` → 连续两次幂等；`jobs` 重建为 `(id, peer)`、`job_runs` 补 `peer`、`plugin_kv` 三列主键、`admins` 重建、`idx_jobs_next`/`idx_job_runs_peer` 补回，全部成立。
7. **R1/R2/R3 的表覆盖与 ARCHITECTURE §8.3 一致**；R2 不清 `plugin_kv`、不清 `identities.peer_scope` 是分级设计（非累加），不是漏项。
8. **会话接管的所有权判定正确**：先删行再 `Handle`、过期优先、权限先于接管、持有者卸载时替换悬挂行；`TestSessionChainsAcrossRounds` 覆盖了关键顺序。
9. **权限先于优先级**：`dispatch.handle` 先跳过无权候选再按 priority 执行，不会出现"高优先级无权插件挡住公开插件"。
10. **调度器 `dueJobLimit=100` 不会饿死**：忙插件在 SQL 里 `plugin NOT IN (...)` 排除，被领走的行下一轮不在窗口里。
11. **`argsAfter` 不会越界**：所有 `fields[n:]` 都经过它；`strings.Fields` 的 cap 陷阱已收口。
12. **出站白名单的后缀匹配是锚定的**：`strings.HasSuffix(host, "."+d)`，`torn.com.evil.com` 绕不过；`userinfo` 用 `Hostname()` 取，`allowed.com@evil.com` 得到 `evil.com` 被拒。
13. **`WithTimeout` 共享连接池与限流桶**：只替换 `hc`，`buckets` 指针保留，`host/http_test.go:187-213` 有断言。
14. **`Params.Decode` 走 TOML 往返**，类型无损；空配置段返回零值不报错。
15. **`termcolor` / `logx` 的契约正确**：`NO_COLOR` 空串视为未设置；非 `*os.File`/重定向/`TERM=dumb` 不上色；派生 handler 共享 `*sync.Mutex`，一条记录一次 Write。
16. **`genplugins` 对空 `plugins/` 的产出合法**（只有 `package main`），不影响构建。
17. **`buildinfo` 与 devrun 的字面量当前一致**（`QQBOT_SUPERVISOR`/`QQBOT_RESTARTED`/75），只是重复定义、有漂移风险。
18. **compact list 的 `TextBlock` 走 `textBlock` 是对的**，只有 `Paragraph`（松散列表）丢失间距。

---

## 5. 需要实测确认的平台事实（决定 P2-1 / P3-11 的等级）

| # | 事实 | 影响 | 怎么测 |
| --- | --- | --- | --- |
| 1 | 消息发送失败是否可能以 2xx + `err_code` 返回 | P2-1 是 P2 还是"防御性加固" | 用过期 `msg_id` 发一条，抓 HTTP 状态与 body |
| 2 | 消息事件 `timestamp` 是字符串还是数字 | 若是数字，`messageData.Timestamp string` 会让**整条消息解码失败被丢弃**（`gateway.go:374-377`） | 抓一条 raw 帧 |
| 3 | op9 `d` 的语义（`true/false` 是否区分可 Resume） | 现在不看 `d`，一律清 session 后重连 | 观察一次 op9 帧 |
| 4 | 4008 后"连接返回的频控信息"具体字段 | 决定 4008 冷却时长 | 触发一次 4008 |
| 5 | 心跳丢 1 次 ACK 就断线是否过于激进 | 当前 `gateway.go:326-335` | 观察抖动网络下的断线频率 |

> 第 2 条即使平台当前恒为字符串，也建议把 `Timestamp` 改成 `json.RawMessage` + `parseAnyTime`（与 `genericData` 一致）——一个字段类型变化不该让整条消息消失。

---

## 6. 执行逻辑说明（改哪里会波及哪里）

### 6.1 入站数据流

```
WS 帧 ──► gateway.payload(op/s/t/d)
          ├─ s>0: setSeq（10s 节流落盘）
          ├─ op0 dispatch: Decode(t, d) ──► host.Event
          │      └─ Core.Handle(ctx, ev)
          │            ├─ KindUnknown → trace，结束
          │            ├─ trace.Received（去重前打印）
          │            ├─ groupAccess → ObserveGroup（登记 + changed 时补任务/取名）
          │            ├─ MarkEventSeen（幂等表，失败即跳过）
          │            ├─ handleGroupLifecycle（入群/退群，先去重后处理）
          │            ├─ resolveActor（admins 表 → ev.Actor）
          │            ├─ InsertMessage + LinkIdentity
          │            ├─ handleReserved（/claim /admin /group /reset /reload）
          │            └─ Dispatcher.Dispatch（per-peer 串行队列，深度 16）
          └─ op1 心跳 / op11 ACK / op7 重连 / op9 清 session
```

`Dispatcher.handle` 的顺序是**固定**的：会话接管 → 静态匹配 → 权限过滤 → 同 priority 让位链 → `Handle`（3s 超时）→ `Reply`（独立 5s ctx）。改这里任何一步的顺序都会破坏一条不变式（见 §6.4）。

### 6.2 出站数据流

```
Plugin.Handle → host.Sender.Reply
   → Sender.splitReply（图文拆两条，固定文本在前）
   → Sender.sendPassive
       ├─ bodyFor：Image → UploadMedia（30s client / 5s ctx）
       ├─ reserve：passiveWindow(used/seq)，先占格再发
       └─ api.SendMessage → httpx.PostJSONOnce
             ├─ 白名单（仅初始 URL）
             ├─ 域名桶（默认桶 / [http.domains]）
             └─ 非 2xx → *httpx.HTTPError → host.HTTPStatusError
```

主动消息 `SendActive` 的顺序相反：**先扣 `active_usage` 日预算，再上传，再发**（`sender.go:344-357`），失败不退额度是明确取舍。

### 6.3 启动 / 关停顺序（改的时候必须保持）

```
启动：config.Load → logx → LoadFonts（~300ms）→ 单实例锁 → store.Open
      → Migrate → httpx → api → core.New → signal.NotifyContext
      → core.Start（schema 校验 → Bind → Init → sched.Start → 插件 Start → ensureOwner → gcLoop）
      → gateway.New → SetReconnector → go gw.Run

关停：ctx.Done / restart → 等 gwDone（5s）→ core.Shutdown
      （sched.Stop → sched.Wait → 插件 Close 逆序）→ run 返回 → db.Close → 释放锁
```

**缺口**：dispatch 与 gcLoop 不在关停序列里（P2-7）；调度任务的收尾写库用的是已取消 ctx（P2-2）。

### 6.4 不变式清单（改动时不能破）

| 不变式 | 由谁守 |
| --- | --- |
| 群闸门在去重/落库/路由之前 | `core.Handle` 的顺序 |
| 生命周期在去重之后、闸门之外 | `core.Handle:270-297` |
| 同一 peer 串行、跨 peer 并行 16 | `Dispatcher.peerLoop` + `sem` |
| 会话一次性消费：先删行再 Handle | `Dispatcher.takeOverSession` + `TestSessionChainsAcrossRounds` |
| 权限先于优先级 | `Dispatcher.handle` 的两段循环 |
| `msg_seq` 单调、窗口过期只清 used | `Sender.reserve` |
| 被动先上传后占格；主动先扣预算后上传 | `Sender.sendPassive` / `sendActiveOne` |
| 任一级 reset 不动 `bot_state`/`admins`/`qq_groups` | `store/reset.go` 的 SQL 清单 |
| 保留前缀清单与 `handleReserved` 严格对应 | `router_test` |

### 6.5 「想改 X 必须同步改哪几处」速查

| 想改 | 必须同步 |
| --- | --- |
| 新增/修改 Manifest 字段 | `host/host.go` 文档 + `router.go`（校验/消费）+ `host.APIVersion` + `ARCHITECTURE §3.2` + 所有插件 Manifest |
| 新增核心保留指令 | `router.go:reservedPrefixes` + `claim.go:handleReserved` + 对应 `handleX` + `ARCHITECTURE §8` + 测试 |
| 新增配置字段 | `config.go` 结构体 + `applyDefaults`（如需默认）+ `Validate`（如需约束）+ `config.example.toml` + README/ARCHITECTURE |
| 改表结构 | `store/schema.go`（幂等 DDL + 旧库迁移分支）+ 所有 Scan/INSERT 列序 + 对应 `reset` 级别 + `ARCHITECTURE §4.1` |
| 改 reset 分级 | `core/reset.go:resetSpecs` + `store/reset.go` 的 DELETE 清单 + 预览 `PeerStats/DataStats` + `ARCHITECTURE §8.3` + 审计文案 |
| 改调度语义 | `scheduler.go` + `store/jobs.go:DueJobs` + `ARCHITECTURE §5.1` + `job_runs` 的 R2 清理 |
| 改出站错误模型 | `httpx.HTTPError` + `host.HTTPStatusError` + `api` 的 err_code 信封 + 插件错误分支 |
| 改渲染行为 | `render/*` + `ARCHITECTURE ADR-0002` + `plugins/md` 的用法 |

---

## 7. 建议动手顺序

1. **先修 P1-1**（关闭码映射）——这是唯一"重启也救不回来、只能改库"的故障。顺手补表驱动测试。
2. **再修 P1-2**（token 失效链路）——同一个 commit 里把 4004 分支接上，避免两次改同一段。
3. **加 err_code 信封**（P2-1）——先在测试里用 fake httpx 造 200+err_code，再改实现；这是"日志说发成功、用户没收到"的根因。
4. **关停时序**（P2-2 / P2-7）：调度收尾用 `WithoutCancel`，Dispatcher 加 `Wait`。这两个放一起改，因为它们共享"关库之前必须停写"这条约束。
5. **配额口径**（P2-4）：把 scheduler 的 location 注入 Sender；部署单元同时钉 `TZ`（双保险）。
6. **reset 审计状态**（P2-3）：加 `failed` 状态，先改 store 常量与查询，再改 core。
7. **平台错误分类**（P2-10 / P2-11）：与 P1-1 同一张表，建议一次性做完。
8. **P3 批量**：渲染 5 条可一个 commit 一起做（都有现成单测入口）；store 索引两条一起；工具链三条一起。
9. 每条改动按项目约定：**一次改动一次 commit，先 push 再关票**；改契约（Manifest/配置/表）先改 `ARCHITECTURE.md` 再改代码。

### 建议新增的永久守卫

| 测试 | 拦住的东西 |
| --- | --- |
| `TestCloseCodeClassification`（表驱动：4006/4007→清 session、4009→保留、4004→清 token、4914/4915→fatal） | P1-1 / P2-10 回归 |
| `TestInvalidateTokenDropsPersisted` | P1-2 回归 |
| `TestSendMessageRejectsBusinessError`（fake httpx 返回 200 + `err_code`） | P2-1 回归 |
| `TestJobStatusPersistedAfterStop` | P2-2 回归 |
| `TestActiveUsageDayUsesSchedulerLocation` | P2-4 回归 |
| `TestReplyDeadlineCoversUpload`（断言上传拿到 ≥30s） | P2-6 回归 |
| `TestRedirectRevalidated`（httptest 302 到未声明域） | P2-9 回归 |
| 渲染 golden：松散列表段间距、tab 不出方框 | P3-1 / P3-3 回归 |

> 反向验证：每条新断言都要先把对应实现改回错的版本，确认**恰好那一条**红、其余保持绿，再还原。全绿但断言恒真的测试等于没写。

---

## 8. 本次审查的边界

- 没有做真实平台联调；所有"平台事实"都标注了来源（官方文档）或"需实测"。
- 没有评估 Torn 插件仓（另一个工作区）的实现，只核对了核心侧的 `host` 契约。
- 性能只做了结构性判断（索引、全表扫、内存上界），没有做压测；`render` 的耗时数据来自子审查的实测样本，未复测。
- 报告里的行号以 `master@a6d7ae8` 为准；后续改动可能漂移。
