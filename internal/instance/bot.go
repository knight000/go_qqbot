package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/gateway"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// botRuntime 是一个 Bot 的全部运行期资源，也是它的编排器。
//
// 「一 Bot 一库」在代码里的落点就是这里：一个 Bot 一份 store、一个 api 客户端
// （令牌缓存因此天然按 Bot 分区）、一条网关连接、一套插件进程。容器只按
// 配置顺序把它们逐个装起来，彼此之间不共享任何可变状态。
//
// 插件进程目前在每 Bot 各起一套，是过渡形态：ADR-0008 决策 11 要求共享一套
// 进程、由核心按 Bot 给独立 Host 实例，那是 #65 的事。在那之前，宁可多花内存，
// 也不能让两个 Bot 的事件落到同一个插件的 Host 上——那会直接串库。
type botRuntime struct {
	cfg config.Bot
	log *slog.Logger

	db      *store.DB
	lock    *Lock
	plugins []host.Plugin
	// coreImpl 是核心；为 nil 表示装配在它之前就失败了。
	coreImpl *core.Core
	gw       *gateway.Client
	// conn 是 runGateway 真正驱动的连接。生产恒等于 gw，测试注入假 runner：
	// 退避与状态迁移不需要真起一条 WebSocket。
	conn gatewayRunner
	// backoff 是 failing 态的重连退避序列，最后一项即上限。测试用毫秒注入。
	backoff []time.Duration
	// stagger 是进程级共享的错峰闸门；gateDone 在这个 Bot 的第一次取地址
	// 落定后关闭，容器靠它把错峰的代价算进启动时长。
	//
	// 用信号而不是估算时长：GET /gateway 可能成功、可能 5s 超时、也可能被
	// ctx 打断。容器想知道的是"这一次排队已经结束"，不是"大概过了多久"。
	stagger  *Stagger
	gateOnce sync.Once
	gateDone chan struct{}

	// gwDone 在网关读循环退出时关闭。它退出前已经把 session_id / seq 落盘，
	// 所以 waitGateway 等它就是在等"状态落盘完成"。
	gwDone chan struct{}

	// err 非 nil 表示这个 Bot 没起来（锁被占、开库失败、插件装配失败…）。
	// 容器据此把它记进状态并在日志里指名，但不影响其他 Bot。
	err error
}

// Do 让这个 Bot 走一次共享的错峰闸门，实现 api.Gate。
//
// 闸门是进程级一份（跨 Bot 共享，否则跨 Bot 撞配额依旧发生），"第一次落定"
// 是本 Bot 一份——容器只关心自己刚装的那一个。
func (b *botRuntime) Do(ctx context.Context, fn func(context.Context) error) error {
	err := b.stagger.Do(ctx, fn)
	b.gateOnce.Do(func() { close(b.gateDone) })
	return err
}

// status 汇总这个 Bot 的装配结果。库与两把锁的路径必须带上：它们由 appid
// 推导，排障时第一件事就是照着 `ls` 一遍。
func (b *botRuntime) status(cfg *config.Config) Status {
	st := Status{
		AppID:   b.cfg.AppID,
		DataDir: cfg.BotDataDir(b.cfg),
		DBPath:  cfg.BotStorePath(b.cfg),
		Plugins: len(b.plugins),
		Err:     b.err,
	}
	if b.lock != nil {
		st.LockPath, st.AppLockPath = b.lock.Path(), b.lock.AppPath()
	}
	return st
}

// failingReconnectBackoff 是 failing 态的重连退避：1/2/4/8/16 分钟。
//
// 与调度器熔断（internal/core/scheduler.go 的 maxBackoff）同一个形状与同一个
// 上限：这两处都是"远端不可用，重试只会更糟"，没有理由用两套节奏。
var failingReconnectBackoff = []time.Duration{
	time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
}

// gatewayRunner 是 runGateway 依赖的最小接口（*gateway.Client 实现它）。
type gatewayRunner interface {
	Run(ctx context.Context) error
}

// startBot 按「锁 → 库 → 迁移 → 客户端 → 插件 → 核心 → 网关」装配一个 Bot。
//
// 任何一步失败都只把这个 Bot 标成未启动，并把已经占下的资源按逆序放掉：
// 一个 Bot 的凭据写错、数据目录没权限、appID 被另一个 Core 占着，都不该让
// 同一台机器上其他租户跟着下线。
func (c *Container) startBot(ctx context.Context, b config.Bot) *botRuntime {
	rt := &botRuntime{
		cfg:      b,
		log:      c.log.With("bot", b.AppID),
		stagger:  c.stagger,
		gateDone: make(chan struct{}),
		gwDone:   make(chan struct{}),
		backoff:  c.o.ReconnectBackoff,
	}
	if len(rt.backoff) == 0 {
		rt.backoff = failingReconnectBackoff
	}
	// 装配失败必须在这里就地把资源放掉：容器只负责记录状态与继续下一个，
	// 它不该知道"这个 Bot 拿到了几把锁、开了哪个库"。
	defer func() {
		if rt.err != nil {
			// 用脱离取消的短期限：关停信号可能正好落在装配中途，而"放掉
			// 已经占下的锁与库"不能因为 ctx 已取消就跳过。
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), assemblyCleanupTimeout)
			defer cancel()
			if err := rt.close(cleanupCtx); err != nil {
				rt.log.Warn("清理未完成的装配失败", "err", err)
			}
		}
	}()
	dbPath := c.cfg.BotStorePath(b)

	// 锁必须在开库与连网关之前拿到。两把锁各按 Bot 取：appID 维度挡"同一个
	// Bot 换了个数据目录"，数据目录维度挡"两个 Bot 指向同一份库"。
	lock, err := Acquire(LockOptions{
		AppID:    b.AppID,
		DataPath: dbPath + ".lock",
		Wait:     c.o.LockWait,
	})
	if err != nil {
		rt.err = err
		return rt
	}
	rt.lock = lock

	db, err := store.Open(dbPath)
	if err != nil {
		rt.err = err
		return rt
	}
	rt.db = db
	if err := db.Migrate(ctx); err != nil {
		rt.err = err
		return rt
	}
	// 启动时把库里的状态与配置对齐。走到这里的 Bot 一定是启用状态（停用的
	// 在 Container.Start 就跳过了），所以这一步只负责把上次的 suspended 解除。
	if _, err := core.ReconcileBotState(ctx, db, true); err != nil {
		rt.err = err
		return rt
	}

	// 每个 Bot 一份出站客户端：429 冷却桶是按域名记的，共享一份等于让一个
	// 租户的 429 把同进程里所有租户一起禁言（ADR-0008 决策 14）。代价是
	// 放弃跨 Bot 的连接池复用——隔离正确性优先。
	outbound := c.outbound.Partition(rt.log.With("component", "httpx"))

	apiClient := api.New(api.Options{
		HTTP:     outbound,
		BaseURL:  c.cfg.Core.APIBase,
		TokenURL: c.cfg.Core.TokenURL,
		AppID:    b.AppID,
		Secret:   b.Secret,
		// State 指向这个 Bot 自己的库：access_token 因此按 Bot 分区，
		// A 的令牌不会出现在 B 的 bot_state 里。
		State: db,
		Log:   rt.log.With("component", "api"),
		Gate:  rt,
	})

	plugins, err := c.loadPlugins(ctx, b, rt.log)
	if err != nil {
		rt.err = err
		return rt
	}
	rt.plugins = plugins

	var upgrade core.UpgradeService
	if c.o.UpgradeFor != nil {
		upgrade = c.o.UpgradeFor(b, db, plugins)
	}

	coreImpl, err := core.New(core.Options{
		Store: db,
		API:   apiClient,
		// 插件出站也走这个 Bot 自己的客户端：域名额度与冷却桶因此按 Bot 分区，
		// 与核心自身的调用同一份额度（不是两份额度）。
		HTTP:    outbound,
		Log:     rt.log,
		Config:  c.cfg,
		Plugins: plugins,
		// 渲染器是进程级共享的一份（字体与内存上限都是全局的），但每个 Bot
		// 各包一层"同时最多 1 个渲染任务"的闸门：单租户连续提交渲染不该
		// 把全局那 2 格吃满，让别的租户只能排队。
		Renderer: renderGateFor(c.o.Renderer),
		Upgrade:  upgrade,
		BotID:    b.AppID,
		// 认领凭证就是配置文件里这个 Bot 的 secret（ADR-0008 决策 4）：
		// 核心不生成、不派发、不打印任何凭据。
		Secret: b.Secret,
		// c.jobs 为 nil 时 Scheduler 自建一份容量 4 的池（单 Bot 行为不变）；
		// 非 nil 时所有 Bot 共享它，并因此各自同时只跑 1 个桶。
		Jobs: c.jobs,
	})
	if err != nil {
		rt.err = err
		return rt
	}
	rt.coreImpl = coreImpl

	if err := coreImpl.Start(ctx); err != nil {
		rt.err = err
		return rt
	}

	var rawDump func(string, []byte)
	if c.cfg.Debug.Enabled && c.cfg.Debug.Raw {
		rawDump = coreImpl.PrintRaw
	}
	gw := gateway.New(gateway.Options{
		API:        apiClient,
		Store:      db,
		Log:        rt.log,
		Intents:    c.cfg.Bot.Intents,
		ShardTotal: c.cfg.Bot.ShardTotal,
		ShardID:    c.cfg.Bot.ShardID,
		Resume:     c.cfg.Bot.Resume,
		Handler:    coreImpl.Handle,
		RawDump:    rawDump,
		HTTPClient: c.o.Handshake,
		// 握手成功才叫"重连成功"：TCP 连上、甚至 Identify 发出去了都不算，
		// 只有 READY/RESUMED 才证明凭据与身份是好的。failing → active 只认它。
		OnReady: func(bool) { coreImpl.NoteConnected() },
	})
	// 先接线再起协程：Handle 读这个字段与这里写它之间靠 goroutine 启动建立
	// 顺序，反过来就是数据竞争。
	coreImpl.SetReconnector(gw)
	rt.gw = gw
	rt.conn = gw

	c.noteGatewayStart()
	go rt.runGateway(ctx, c)
	return rt
}

// runGateway 跑这个 Bot 的连接；failing 态时按 1/2/4/8/16 分钟退避重连。
//
// 为什么循环在网关之外：gateway.Client.Run 自己只管"链路抖动"——它内部就带
// 指数退避。failing 是另一个层次的问题（凭据失效、身份被封、AppID 下架），
// 只有核心能判（连续鉴权类业务码），所以退避节奏必须由这里接住：Run 被
// FatalError 打断之后，只要状态还是 failing 就等下一档再试，而不是收工。
//
// 非 failing 的退出保持原语义：永久性错误不带走整个进程（那会连累同一台
// 机器上其他租户），只记一条点名到 Bot 的 Error 并把连接放掉。
func (b *botRuntime) runGateway(ctx context.Context, c *Container) {
	defer close(b.gwDone)
	failures := 0
	for {
		err := b.conn.Run(ctx)
		if ctx.Err() != nil {
			c.noteGatewayExit(nil)
			return
		}
		if err != nil {
			b.log.Error("网关永久性错误，该 Bot 已停止重连，其他 Bot 不受影响", "err", err)
		}
		if !b.failing(ctx) {
			c.noteGatewayExit(err)
			return
		}
		wait := b.backoffFor(failures)
		failures++
		b.log.Warn("Bot 处于 failing，退避后重连",
			"backoff", wait.String(), "attempt", failures)
		if !sleepCtx(ctx, wait) {
			c.noteGatewayExit(nil)
			return
		}
	}
}

// failing 读一次 Bot 状态。读不出来就按非 failing 处理：状态层坏了应当让
// 连接停在"已退出"，而不是拿一个猜出来的状态去无限重连。
func (b *botRuntime) failing(ctx context.Context) bool {
	if b.coreImpl == nil {
		return false
	}
	st, err := b.coreImpl.BotState(ctx)
	return err == nil && st == core.BotStateFailing
}

// backoffFor 返回第 n 次（从 0 起）退避时长，超出序列时钉在最后一项。
func (b *botRuntime) backoffFor(n int) time.Duration {
	if n >= len(b.backoff) {
		return b.backoff[len(b.backoff)-1]
	}
	return b.backoff[n]
}

// sleepCtx 等到 d 或 ctx 取消，返回是否睡满。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitGateway 等读循环退出并落盘，返回它是否按时退出。
//
// 两个上限同时生效：单个 Bot 的 gatewayStopTimeout（挂住的那一条不该拖住
// 别人）与调用方的整体关停预算 ctx。超时不等于失败——只是"状态可能没落盘"，
// 下次可能要冷 Identify。
func (b *botRuntime) waitGateway(ctx context.Context) bool {
	if b.gw == nil {
		return true
	}
	t := time.NewTimer(gatewayStopTimeout)
	defer t.Stop()
	select {
	case <-b.gwDone:
		return true
	case <-t.C:
		b.log.Error("网关读循环未在期限内退出，状态可能未落盘",
			"timeout", gatewayStopTimeout.String())
		return false
	case <-ctx.Done():
		b.log.Error("关停预算已用尽，不再等网关落盘", "err", ctx.Err())
		return false
	}
}

// close 按装配的逆序释放：核心（内含调度收尾、派发排空、关插件）→ 库 → 锁。
//
// 装配中途失败时也走这里：coreImpl 为 nil 就直接关插件，库与锁各自判空。
// 顺序不能反——插件与核心都在写库，先关库会得到一串 database is closed。
func (b *botRuntime) close(ctx context.Context) error {
	var errs []error
	if b.coreImpl != nil {
		if err := b.coreImpl.Shutdown(ctx); err != nil {
			b.log.Warn("核心关停未完成", "err", err)
			errs = append(errs, err)
		}
		// Shutdown 一定会走到关插件那一步，别再关第二遍。
	} else {
		// 装配在 core.New 之前就失败了：插件已经活着，但没有 Core 替它收尾。
		core.ClosePlugins(ctx, b.log, b.plugins)
	}
	b.plugins = nil
	if b.db != nil {
		path := b.db.Path()
		if err := b.db.Close(); err != nil {
			b.log.Warn("关闭数据库失败", "db", path, "err", err)
			errs = append(errs, fmt.Errorf("关闭 %s: %w", path, err))
		}
		b.db = nil
	}
	if b.lock != nil {
		if err := b.lock.Release(); err != nil {
			b.log.Warn("释放单实例锁失败", "lock", b.lock.Path(), "err", err)
			errs = append(errs, err)
		}
		b.lock = nil
	}
	return errors.Join(errs...)
}
