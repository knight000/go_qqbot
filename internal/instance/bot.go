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

	apiClient := api.New(api.Options{
		HTTP:     c.outbound,
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
		Store:    db,
		API:      apiClient,
		HTTP:     c.outbound,
		Log:      rt.log,
		Config:   c.cfg,
		Plugins:  plugins,
		Renderer: c.o.Renderer,
		Upgrade:  upgrade,
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
	})
	// 先接线再起协程：Handle 读这个字段与这里写它之间靠 goroutine 启动建立
	// 顺序，反过来就是数据竞争。
	coreImpl.SetReconnector(gw)
	rt.gw = gw

	c.noteGatewayStart()
	go rt.runGateway(ctx, c)
	return rt
}

// runGateway 跑这个 Bot 的连接直到 ctx 取消或遇到永久性错误。
//
// 永久性错误（下架/封禁/intents 无权限）**不再让整个进程退出**：那会把同一台
// 机器上其他健康的 Bot 一起带走。这里只记一条点名到 Bot 的 Error 并放掉一条
// 连接；failing 态、退避与告警是 #63。
func (b *botRuntime) runGateway(ctx context.Context, c *Container) {
	defer close(b.gwDone)
	err := b.gw.Run(ctx)
	if err != nil && ctx.Err() == nil {
		b.log.Error("网关永久性错误，该 Bot 已停止重连，其他 Bot 不受影响", "err", err)
	}
	c.noteGatewayExit(err)
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
