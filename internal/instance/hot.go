package instance

// 单 Bot 热增删改（#68）。
//
// 本文件是 ADR-0008 决策 15/16 的落点：配置文件增删一个 [[bots]] 条目不需要
// 重启进程，/bot reload <appid> 只重连那一个。三条不变量在任何路径上都必须
// 成立，它们也是这堆代码看上去"啰嗦"的原因：
//
//  1. **别人的连接不动**。每个 Bot 有自己的 ctx、自己的库、自己的出站客户端，
//     停一个只取消它那一条（bot.go 的 rt.cancel）。
//  2. **数据保留**。删条目 = 停连接 + 标 suspended(config_gone)，库原样留着；
//     真删只有显式 /bot remove --purge（R3 语义）。
//  3. **不串库、不 panic**。停一个 Bot 前先等它那条网关读循环退出——Handler
//     是同步跑在读循环里的，所以"读循环退出"= "在途事件已经处理完"；
//     排队未跑的事件由 Core.Shutdown 的派发排空接住，之后才关库。
//
// 结构性操作全部串行在 hotMu 上（含启动与关停）：一次 reload 要连开/关几次
// 连接，中途被另一个 reload 插进来只会得到错误的差集。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/store"
)

// 编译期确认容器就是核心要的那个热操作入口：签名一改，这里立刻报错，
// 而不是等到 /bot 被真的发出来才发现。
var _ core.BotHotService = (*Container)(nil)

// loadConfig 重读磁盘上的配置。
//
// 必须走 config.Load（而不是"拿启动时那份改一改"）：reload 的意义就是让
// 文件成为唯一事实源，包括 secret 这种只可能在最后被替换的字段。
func (c *Container) loadConfig() (*config.Config, error) {
	if c.o.ConfigPath == "" {
		return nil, errors.New("未配置配置文件路径，热重载不可用（用 /reload 重启进程）")
	}
	cfg, err := config.Load(c.o.ConfigPath)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// botEntry 找到某个 appid 的配置条目，并报告它是否**仍在**配置里。
//
// 已从配置消失的条目落在 retired 里：库路径由 appid 推导、但 data_dir 可以
// 被条目覆盖，忘了它就等于对着默认路径找一个不在那里的库。
func (c *Container) botEntry(appid string) (config.Bot, bool) {
	cfg := c.config()
	for _, b := range cfg.Bots {
		if b.AppID == appid {
			return b, true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.retired[appid]; ok {
		return b, false
	}
	return config.Bot{AppID: appid}, false
}

// findRuntime 按 appid 找运行期（没装起来的也在名单里，err 非 nil）。
func (c *Container) findRuntime(appid string) *botRuntime {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rt := range c.bots {
		if rt.cfg.AppID == appid {
			return rt
		}
	}
	return nil
}

// Snapshot 返回 /bot list 要的全部事实。
//
// 顺序：配置里的条目按配置顺序在前，已从配置消失的（retired）按 appid 排序
// 跟在后面。后者必须出现——否则"purge 一个已经删掉条目的 Bot"这件事在
// /bot list 里没有任何入口提示。
func (c *Container) Snapshot(ctx context.Context) ([]core.BotView, error) {
	// 与结构性热操作串行：一次 reload 中途读到的名单会是"半套"，
	// 而 /bot list 是给人做判断用的，半套比没有更糟。
	if c.shuttingDown() {
		return nil, errors.New("进程正在关停")
	}
	c.hotMu.Lock()
	defer c.hotMu.Unlock()

	cfg := c.config()
	views := make([]core.BotView, 0, len(cfg.Bots))
	seen := make(map[string]bool, len(cfg.Bots))
	for _, b := range cfg.Bots {
		seen[b.AppID] = true
		views = append(views, c.viewOf(ctx, b, true))
	}

	c.mu.Lock()
	gone := make([]config.Bot, 0, len(c.retired))
	for id, b := range c.retired {
		if !seen[id] {
			gone = append(gone, b)
		}
	}
	c.mu.Unlock()
	sort.Slice(gone, func(i, j int) bool { return gone[i].AppID < gone[j].AppID })
	for _, b := range gone {
		views = append(views, c.viewOf(ctx, b, false))
	}
	return views, nil
}

// viewOf 组装一个 Bot 的快照。
//
// 运行中的直接读内存里的核心（不额外开库）；没在跑的临时开库读状态与
// admins。库文件不存在时按"从未运行过"处理：pending_claim + 未认领，
// 不为了报告一行状态就在磁盘上造一个空库。
func (c *Container) viewOf(ctx context.Context, b config.Bot, inConfig bool) core.BotView {
	v := core.BotView{
		AppID:    b.AppID,
		CertType: b.CertType,
		Enabled:  inConfig && b.IsEnabled(),
	}

	rt := c.findRuntime(b.AppID)
	if rt != nil {
		// Running 认的是"这条网关还算在活的连接里"，不是"装配过"：
		// 一个永久性错误退出的网关仍然有 rt.gw，但它已经不在服务了。
		v.Running = rt.err == nil && c.liveGateway(rt)
		if rt.err != nil {
			v.Err = rt.err.Error()
		}
		if rt.coreImpl != nil {
			if st, err := rt.coreImpl.BotState(ctx); err == nil {
				v.State = string(st)
			}
		}
		if rt.db != nil {
			if n, err := rt.db.CountAdmins(ctx); err == nil {
				v.Claimed = n > 0
			}
			if reason, err := core.LoadSuspendReason(ctx, rt.db); err == nil {
				v.SuspendReason = reason
			}
		}
		if v.State == "" && v.Err == "" {
			// 核心还没装配好：至少把库里的状态读出来。
			c.fillStateFromDB(ctx, &v, b)
		}
		return v
	}

	if !inConfig {
		v.Enabled = false
	}
	c.fillStateFromDB(ctx, &v, b)
	return v
}

// liveGateway 报告这个 Bot 的网关是否还计在 live 里（见 noteGatewayExit）。
func (c *Container) liveGateway(rt *botRuntime) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return rt.gwLive
}

// fillStateFromDB 临时开一份连接读状态（只读，不迁移、不创造文件）。
func (c *Container) fillStateFromDB(ctx context.Context, v *core.BotView, b config.Bot) {
	path := c.config().BotStorePath(b)
	if _, err := os.Stat(path); err != nil {
		// 从没运行过：没有库就没有状态，按推导值报告。
		v.State = string(core.BotStatePendingClaim)
		return
	}
	db, err := store.Open(path)
	if err != nil {
		v.Err = "打开库失败: " + err.Error()
		return
	}
	defer func() { _ = db.Close() }()
	if st, err := core.LoadBotState(ctx, db); err == nil {
		v.State = string(st)
	} else {
		v.Err = err.Error()
	}
	if n, err := db.CountAdmins(ctx); err == nil {
		v.Claimed = n > 0
	}
	if reason, err := core.LoadSuspendReason(ctx, db); err == nil {
		v.SuspendReason = reason
	}
}

// ReloadAll 重读配置文件并按 appid 求差集（ADR-0008 决策 15）。
//
// 差集判定只看"装一个 Bot 需要什么"，不看全局配置：
//
//	配置文件有、运行态没有          → 新增（enabled=false 则只标 suspended）
//	两边都有、enabled false→true    → 启动
//	两边都有、enabled true→false    → 停用（数据保留）
//	两边都有、secret/cert_type/data_dir/quota_profile 变了 → 重连
//	两边都有、以上都没变            → 不动（这是"只动有变化的"的全部含义）
//	配置文件没有、运行态有          → 停用，标 suspended(config_gone)
//
// 全局配置（数据目录、插件路径、日志级别、[core].owners）**不热改**：
// 它们要么必然要求重启，要么改了只会得到半生效状态（决策 15）。新装/重连的
// Bot 会拿到新读到的这份配置，已经在跑的那些仍用启动时那份——这一点写进
// impl-68 的已知边界，不假装全局也热加载了。
func (c *Container) ReloadAll(ctx context.Context, self string) (core.BotReloadSummary, error) {
	var sum core.BotReloadSummary

	// 先判再锁：关停已经开始时立刻返回，别把自己挂在 hotMu 上——
	// Shutdown 正持着这把锁等派发排空，而派发里排着的可能就是这个 handler。
	if c.shuttingDown() {
		return sum, errors.New("进程正在关停，拒绝热重载")
	}
	c.hotMu.Lock()
	defer c.hotMu.Unlock()
	if c.shuttingDown() {
		return sum, errors.New("进程正在关停，拒绝热重载")
	}

	next, err := c.loadConfig()
	if err != nil {
		return sum, err
	}
	old := c.config()
	oldByID := indexBots(old.Bots)
	newByID := indexBots(next.Bots)

	// 先把配置换成新读到的这份：之后新装/重连的 Bot 拿到的是它，路径推导
	// 与配额也一致。已经在跑的那些仍用自己的那份（全局配置不热改）。
	c.setConfig(next)

	// 任务桶是**进程级共享**的额度池（#66），它在启动装配时按"会不会有多个
	// 启用的 Bot"决定建不建，而 Core 一旦建好就拿不到后补的池。所以从 1 个
	// 热增到 2 个之后，两个 Core 会各自持一份容量 4 —— 总并发变成 8。
	// 这是本票不做的事（要修得让 Core 的池可替换），但不能装作没发生。
	if c.jobs == nil && countEnabled(next.Bots) > 1 {
		c.log.Warn("热重载后启用多个 Bot，但任务桶仍是每 Bot 一份（总并发 8 而非 4）；要回到共享 4 需重启进程",
			"enabled", countEnabled(next.Bots))
	}

	for _, nb := range next.Bots {
		ob, existed := oldByID[nb.AppID]
		switch {
		case !existed:
			if !nb.IsEnabled() {
				// 新条目但显式停用：不连，但状态要落库，否则 /bot list 里
				// 它的状态是"从没运行过"而不是"配置里关着"。
				c.markSuspended(ctx, nb, core.SuspendReasonConfigDisabled)
				sum.Stopped++
				continue
			}
			if nb.AppID == self {
				sum.Deferred = append(sum.Deferred, nb.AppID)
				continue
			}
			if err := c.launch(ctx, nb); err != nil {
				sum.Failed = append(sum.Failed, nb.AppID)
				continue
			}
			sum.Added++

		case ob.IsEnabled() != nb.IsEnabled():
			if nb.AppID == self {
				sum.Deferred = append(sum.Deferred, nb.AppID)
				continue
			}
			if nb.IsEnabled() {
				// 从停用改回启用：复用已有的库与锁，数据原样回来。
				if rt := c.findRuntime(nb.AppID); rt != nil {
					c.stop(ctx, rt, "")
				}
				if err := c.launch(ctx, nb); err != nil {
					sum.Failed = append(sum.Failed, nb.AppID)
					continue
				}
				sum.Restarted++
				continue
			}
			if rt := c.findRuntime(nb.AppID); rt != nil {
				c.stop(ctx, rt, core.SuspendReasonConfigDisabled)
			} else {
				c.markSuspended(ctx, nb, core.SuspendReasonConfigDisabled)
			}
			sum.Stopped++

		case botNeedsReconnect(ob, nb):
			if nb.AppID == self {
				sum.Deferred = append(sum.Deferred, nb.AppID)
				continue
			}
			if err := c.reconnect(ctx, nb); err != nil {
				sum.Failed = append(sum.Failed, nb.AppID)
				continue
			}
			sum.Restarted++

		default:
			sum.Unchanged++
		}
	}

	// 配置里删掉了：停连接、保数据、留一条"配置里没了"的停用原因。
	for _, ob := range old.Bots {
		if _, ok := newByID[ob.AppID]; ok {
			continue
		}
		if ob.AppID == self {
			sum.Deferred = append(sum.Deferred, ob.AppID)
			continue
		}
		c.remember(ob)
		if rt := c.findRuntime(ob.AppID); rt != nil {
			c.stop(ctx, rt, core.SuspendReasonConfigGone)
		} else {
			c.markSuspended(ctx, ob, core.SuspendReasonConfigGone)
		}
		sum.Stopped++
	}

	c.log.Info("配置热重载完成",
		"added", sum.Added, "restarted", sum.Restarted,
		"stopped", sum.Stopped, "unchanged", sum.Unchanged,
		"failed", len(sum.Failed), "deferred", len(sum.Deferred))
	return sum, nil
}

// ReloadOne 只重连 appid 这一个，**不重读配置**。
//
// 不重读是刻意的：这个命令的语义是"这条连接状态不对，单独换一条"，
// 而不是"把配置文件里的改动应用一部分"。要应用改动用 /bot reload。
func (c *Container) ReloadOne(ctx context.Context, self, appid string) error {
	if c.shuttingDown() {
		return errors.New("进程正在关停，拒绝重连")
	}
	c.hotMu.Lock()
	defer c.hotMu.Unlock()
	if c.shuttingDown() {
		return errors.New("进程正在关停，拒绝重连")
	}
	if appid == self {
		return core.ErrBotSelfOp
	}
	rt := c.findRuntime(appid)
	if rt == nil {
		return fmt.Errorf("%s 不在运行：新增或重新启用它用 /bot reload", appid)
	}
	// 条目取自运行中的那一份（不是重读配置）：/bot reload <appid> 的语义
	// 是"换一条连接"，不是"应用配置改动"。
	return c.reconnect(ctx, rt.cfg)
}

// reconnect 停掉再装回同一个条目（在 hotMu 下调用）。
//
// 装回用的是**同一个 Bot 结构**：secret、data_dir、quota 都不变，只是换了
// 一条连接、一份出站客户端与一个核心。库与锁在 stop 里已经放掉，所以
// 这里能重新拿到它们。
func (c *Container) reconnect(ctx context.Context, b config.Bot) error {
	if rt := c.findRuntime(b.AppID); rt != nil {
		c.stop(ctx, rt, "")
	}
	return c.launch(ctx, b)
}

// launch 装配并登记一个新 Bot（在 hotMu 下调用）。
func (c *Container) launch(ctx context.Context, b config.Bot) error {
	rt := c.startBot(c.ctx, b)
	c.addRuntime(rt)
	if rt.err != nil {
		c.log.Error("热操作未能启动 Bot",
			"appid", b.AppID, "db", c.config().BotStorePath(b), "err", rt.err)
		return rt.err
	}
	logBotStarted(rt)
	go c.pumpRestarts(rt.ctx, rt.coreImpl, b.AppID)
	return nil
}

// stop 停掉一个 Bot：先断接入层、等在途事件收尾，再放掉核心与库（在 hotMu 下调用）。
//
// 顺序是这条路径上唯一重要的事：
//   - 先 cancel：网关读循环退出，Handler 是同步跑在它里面的，所以"读循环
//     退出"就意味着这条 Bot 的在途事件已经处理完；
//   - 再等 gwDone：它关闭前会把 session_id / seq 落盘；
//   - 最后 rt.close：核心收尾（调度、派发排空、GC）→ 摘 Host → 关库 → 放锁。
//
// 反过来（先关库）会得到一串 database is closed，而且看起来像"库里数据坏了"。
func (c *Container) stop(ctx context.Context, rt *botRuntime, reason string) {
	rt.cancel()
	if rt.gw != nil {
		rt.waitGateway(ctx)
	}
	if err := rt.close(ctx); err != nil {
		rt.log.Warn("停机未完全收尾", "err", err)
	}
	c.dropRuntime(rt)
	if reason != "" {
		c.markSuspended(ctx, rt.cfg, reason)
	}
	c.log.Info("Bot 已停止", "appid", rt.cfg.AppID, "reason", reason, "db", c.config().BotStorePath(rt.cfg))
}

// PurgeStats 返回 purge 会删掉多少行（预览，不动数据）。
func (c *Container) PurgeStats(ctx context.Context, appid string) (store.ResetCounts, error) {
	if c.shuttingDown() {
		return store.ResetCounts{}, errors.New("进程正在关停")
	}
	c.hotMu.Lock()
	defer c.hotMu.Unlock()
	b, inConfig := c.botEntry(appid)
	if inConfig {
		return store.ResetCounts{}, core.ErrBotStillConfigured
	}
	path := c.config().BotStorePath(b)
	if _, err := os.Stat(path); err != nil {
		return store.ResetCounts{}, fmt.Errorf("找不到 %s 的库（%s）：它可能从未在本机运行过", appid, path)
	}
	db, err := store.Open(path)
	if err != nil {
		return store.ResetCounts{}, err
	}
	defer func() { _ = db.Close() }()
	return db.DataStats(ctx)
}

// Purge 对一个已不在配置里的 Bot 走 R3 全量清理（决策 16）。
//
// 「先删媒体文件、再删行」与 /reset all 是同一条硬约定（ADR §8.3）：反过来
// 会留下无引用的孤儿文件。保留的东西也一致——admins、qq_groups、bot_state
// 都不动，所以再加回配置条目时群名单、历史归属与认领关系都还在。
func (c *Container) Purge(ctx context.Context, appid string) (store.ResetCounts, int, error) {
	if c.shuttingDown() {
		return store.ResetCounts{}, 0, errors.New("进程正在关停")
	}
	c.hotMu.Lock()
	defer c.hotMu.Unlock()

	b, inConfig := c.botEntry(appid)
	if inConfig {
		// 双重保险：核心在发码与确认时各判了一次，装配层再判一次。
		// 配置里还在就清库 = 下一次 reload 会用空库把 Bot 拉起来继续服务。
		return store.ResetCounts{}, 0, core.ErrBotStillConfigured
	}
	if rt := c.findRuntime(appid); rt != nil {
		// 正常情况下 reload 已经把它停了；这里兜的是"配置在别处被改，
		// 而这道指令先到"的次序。
		c.stop(ctx, rt, core.SuspendReasonConfigGone)
	}

	path := c.config().BotStorePath(b)
	if _, err := os.Stat(path); err != nil {
		return store.ResetCounts{}, 0, fmt.Errorf("找不到 %s 的库（%s）：它可能从未在本机运行过", appid, path)
	}
	db, err := store.Open(path)
	if err != nil {
		return store.ResetCounts{}, 0, err
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		return store.ResetCounts{}, 0, err
	}

	paths, err := db.AllMediaPaths(ctx)
	if err != nil {
		return store.ResetCounts{}, 0, err
	}
	files := core.RemoveMediaFiles(c.log, c.config().Media.Dir, paths)
	counts, err := db.ClearAllData(ctx)
	if err != nil {
		return counts, files, err
	}

	c.mu.Lock()
	delete(c.retired, appid)
	c.mu.Unlock()
	c.log.Warn("已清理 Bot 的全部业务数据",
		"appid", appid, "db", path, "files", files,
		"messages", counts.Messages, "media", counts.MediaRows)
	return counts, files, nil
}

// remember 记下"曾经在配置里"的条目，供之后 purge 推导库路径。
func (c *Container) remember(b config.Bot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retired == nil {
		c.retired = make(map[string]config.Bot)
	}
	c.retired[b.AppID] = b
}

// setConfig 换掉当前生效的配置（热重载的一部分）。
func (c *Container) setConfig(cfg *config.Config) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// indexBots 按 appid 建索引。重复 appid 在 config.Validate 就被拒了，
// 这里后写覆盖前写只是"不会 panic"的兜底。
func indexBots(bots []config.Bot) map[string]config.Bot {
	out := make(map[string]config.Bot, len(bots))
	for _, b := range bots {
		out[b.AppID] = b
	}
	return out
}

// botNeedsReconnect 判断同一个 appid 的关键字段是否变了。
//
// 只列**装一个 Bot 需要**的字段：secret（令牌）、cert_type（Host.Bot() 的
// 认证类型）、data_dir（库在哪）、quota_profile（软预算）。
// 换 appid 等于换一个 Bot（差集按 appid 做，不会走到这里）。
func botNeedsReconnect(ob, nb config.Bot) bool {
	return ob.Secret != nb.Secret ||
		ob.CertType != nb.CertType ||
		strings.TrimSpace(ob.DataDir) != strings.TrimSpace(nb.DataDir) ||
		ob.QuotaProfile != nb.QuotaProfile
}

// shuttingDown 报告进程是否已经进入关停。热操作在拿 hotMu 之前先看它。
func (c *Container) shuttingDown() bool { return c.ctx.Err() != nil }

// countEnabled 数配置里有几个启用的 Bot。
func countEnabled(bots []config.Bot) int {
	n := 0
	for _, b := range bots {
		if b.IsEnabled() {
			n++
		}
	}
	return n
}
