// Package core 是主体：幂等、落库、路由、频控、调度、GC。
// 这里不出现任何业务词（不出现 torn、天气、签到），业务全部在 plugins/。
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

type Options struct {
	Store   *store.DB
	API     *api.Client
	HTTP    *httpx.Client
	Log     *slog.Logger
	Config  *config.Config
	Plugins []host.Plugin
	// Renderer 是插件可见的渲染能力。Core 只认 host.Renderer 接口，
	// 不 import 任何渲染实现，测试才能注入 fake、不依赖真字体。
	Renderer host.Renderer
	// Trace 为 nil 且配置里打开了 debug 时，自动输出到 stdout。
	// 显式传入只给测试用（可写进 buffer）。
	Trace *Trace
	// Upgrade 是 /upgrade 依赖的升级能力。nil 表示本进程没有装配自更新
	// （单测、或未来某个只要网关的精简构建）。
	Upgrade UpgradeService
}

type Core struct {
	store   *store.DB
	cfg     *config.Config
	api     groupInfoSource
	http    *httpx.Client
	log     *slog.Logger
	plugins []host.Plugin
	trace   *Trace
	render  host.Renderer
	// session 是插件可见的会话接管能力。nil 时 host 会换成拒绝性替身，
	// 与 Renderer 同一条契约：插件不必写判空分支。
	session host.SessionBackend

	sender   *Sender
	router   *Router
	dispatch *Dispatcher
	sched    *Scheduler

	claimMu    sync.Mutex
	claimToken string
	claimExp   time.Time

	startedAt time.Time
	// restart 只缓冲一条：重复的 /reload 直接返回 ErrRestartPending，
	// 避免同一个进程被多条请求推着反复重启。
	restart chan RestartRequest

	// refreshing 保证同一时刻只有一个批量刷新群名的任务：
	// 两个并发刷新会互相打满 30 QPM 的接口配额，还会把同一批群名写两遍。
	refreshing atomic.Bool

	// gcDone 在 gcLoop 退出时关闭。关库之前必须等它：GC 的 Purge* 与
	// vacuum 都在写库，晚于 db.Close 就是一串 database is closed（P2-7）。
	// gcStarted 表示 Start 真的起过它——未 Start 的 Core（单元测试）
	// 没有这个协程，Shutdown 不能在这里等。
	gcDone    chan struct{}
	gcStarted bool

	// reconnect 是网关的主动重连入口（R0）。由 main 在起网关协程之前注入，
	// 因此 Handle 读取它与 main 赋值之间天然有 happens-before。
	reconnect Reconnector

	// upgrade 是 /upgrade 的升级能力（见 UpgradeService）。nil 时命令仍然
	// 被核心消费（否则会落到插件路由），回复"未装配"。
	upgrade UpgradeService
}

func New(o Options) (*Core, error) {
	logger := o.Log
	if logger == nil {
		logger = slog.Default()
	}
	if o.Config == nil {
		return nil, errors.New("core: 缺少配置")
	}
	router, err := NewRouter(o.Plugins)
	if err != nil {
		return nil, fmt.Errorf("装配插件路由: %w", err)
	}
	trace := o.Trace
	if trace == nil && o.Config.Debug.Enabled {
		trace = NewTrace(os.Stdout, Level(o.Config.Debug.Level))
	}
	if trace != nil {
		trace.SetUnknownPayload(o.Config.Debug.UnknownEvents)
		// 群名读本地登记：调试轨迹里的"群"行要显示名字而不是一串 openid。
		// 只在打印群聊事件时调用一次（关闭态没有任何查询）。
		if o.Store != nil {
			trace.SetGroupNames(func(ctx context.Context, openid string) string {
				g, ok, err := o.Store.GetGroup(ctx, openid)
				if err != nil || !ok {
					return ""
				}
				return g.Name
			})
		}
	}
	// 显式判空再赋给接口：nil 的 *store.DB 装进接口是个非 nil 接口，
	// Sender 会以为有账本、调用时再 nil 解引用。
	var usage activeUsageStore
	if o.Store != nil {
		usage = o.Store
	}
	// 同理：会话的后端与读取入口都只在真库上成立。显式判空再赋给接口，
	// 否则 nil 的 *store.DB 装进接口后 `d.sessions == nil` 永远不成立。
	var session host.SessionBackend
	var sessionRowsSrc sessionStore
	if o.Store != nil {
		session = sessionBackend{db: o.Store, router: router, log: logger.With("component", "session")}
		sessionRowsSrc = sessionRows{o.Store}
	}
	sched := NewScheduler(o.Store, logger.With("component", "scheduler"))
	// 日预算的切日与 cron 必须同源：都由调度器的 location 决定。
	sender := NewSender(o.API, logger.With("component", "sender"), activePolicy(o.Config), usage, sched.Location())
	sender.trace = trace
	dispatch := NewDispatcher(router, sender, logger.With("component", "dispatch"), 3*time.Second)
	dispatch.trace = trace
	dispatch.denyReply = o.Config.Permission.DenyReply
	dispatch.sessions = sessionRowsSrc

	// 显式判空再赋给接口：把 nil 的 *api.Client 存进接口会得到一个
	// 非 nil 接口，后续 `c.api == nil` 就永远不成立。
	var info groupInfoSource
	if o.API != nil {
		info = o.API
	}

	return &Core{
		store:     o.Store,
		cfg:       o.Config,
		api:       info,
		http:      o.HTTP,
		log:       logger,
		plugins:   o.Plugins,
		trace:     trace,
		render:    o.Renderer,
		session:   session,
		sender:    sender,
		router:    router,
		dispatch:  dispatch,
		sched:     sched,
		startedAt: time.Now(),
		restart:   make(chan RestartRequest, 1),
		gcDone:    make(chan struct{}),
		upgrade:   o.Upgrade,
	}, nil
}

// 插件装配阶段的期限。Init 与 Start 都是一次请求-应答握手，插件不回帧时
// 必须能判出来：没有期限的话 core.Start 会永久阻塞，进程活着、systemd 显示
// active (running)、日志停在上一行，比"插件崩溃"难查得多（#51）。
//
// 为什么不从 pluginproc.replaceInitTimeout 取值：core 与 pluginproc 刻意
// 不互相 import（core 只按方法名做类型断言认热替换能力）。为什么不提到
// pkg/host：那份契约是给插件作者看的线协议，启动多久算超时是核心自己的
// 策略，写进契约等于让插件被这个数绑住。两处取值同量级、同语义，各自
// 带注释说明即可。
const (
	// pluginInitTimeout：Init 只做一次配置下发，正常亚秒级。5 秒是为了容忍
	// 冷启动遇上杀软扫描（Windows 首次读一个新 exe），同时让"起不来"能在
	// owner 等得起的时限内被判出来。
	pluginInitTimeout = 5 * time.Second
	// pluginStartTimeout：Start 可能触发预热（字体、渲染器、后台协程），
	// 比握手慢一档，给 10 秒。
	pluginStartTimeout = 10 * time.Second
)

// Start 依次完成：校验插件配置 → 绑定任务执行者 → 插件 Init（注册 Job）
// → 启动调度 → 启动插件 → 生成认领令牌。
func (c *Core) Start(ctx context.Context) error {
	// 配置校验整体先于任何 Init：Manifest.config_schema 是「声明即必填」的契约
	// （评审 §4.1）。放到各自 Init 之前逐个做也不行——先校验先 Init 的话，后面
	// 有插件配置写错时，前面的插件已经拿着零值跑起来了，报错就晚了。
	for _, p := range c.plugins {
		m := p.Manifest()
		if err := m.ValidateConfig(c.cfg.Plugin(m.Name)); err != nil {
			return err
		}
	}
	// 再把 Manifest.Intents 与 [bot].intents 位掩码交叉校验：缺位只告警，
	// 见 warnIntentGaps 里"为什么不 fail-fast"。
	c.warnIntentGaps()

	for _, p := range c.plugins {
		c.sched.Bind(p.Manifest().Name, p)
	}

	for _, p := range c.plugins {
		m := p.Manifest()
		svc := host.NewService(host.Options{
			Name:     m.Name,
			Config:   host.NewParams(m.Name, c.cfg.Plugin(m.Name)),
			Sender:   c.sender.ForPlugin(m.Name),
			KV:       c.store,
			DB:       c.store,
			Admin:    adminStore{c.store},
			HTTP:     c.hostHTTPBackend(m.AllowedDomains),
			Sched:    c.sched,
			Log:      c.log,
			Renderer: c.render,
			Session:  c.session,
		})
		if err := withPluginTimeout(ctx, pluginInitTimeout, "初始化", m.Name, func(c context.Context) error {
			return p.Init(c, svc)
		}); err != nil {
			return err
		}
	}

	if err := c.sched.Start(ctx); err != nil {
		return fmt.Errorf("启动调度器: %w", err)
	}

	for _, p := range c.plugins {
		if st, ok := p.(host.Starter); ok {
			if err := withPluginTimeout(ctx, pluginStartTimeout, "启动", p.Manifest().Name, st.Start); err != nil {
				return err
			}
		}
	}

	if err := c.ensureOwner(ctx); err != nil {
		return err
	}
	c.logGroupGateState(ctx)
	c.gcStarted = true
	go c.gcLoop(ctx)

	c.log.Info("核心已启动", "plugins", len(c.plugins), "sandbox", c.cfg.SandboxEnabled())
	return nil
}

// withPluginTimeout 给一次插件装配调用加期限，并把超时翻译成一句能定位的话。
//
// Init 与 Start 两处形状完全相同、只有"做什么"（verb）不同：期限、超时判定、
// 拼错误串各写一遍，改一处就会漏一处。必须在 cancel 之前判超时——cancel 之后
// ctx.Err() 一律是 Canceled，DeadlineExceeded 再也看不出来。
func withPluginTimeout(ctx context.Context, d time.Duration, verb, name string, fn func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, d)
	err := fn(cctx)
	timedOut := errors.Is(err, context.DeadlineExceeded)
	cancel()
	if err == nil {
		return nil
	}
	if timedOut {
		return fmt.Errorf("%s插件 %s timeout（%v）: %w", verb, name, d, err)
	}
	return fmt.Errorf("%s插件 %s: %w", verb, name, err)
}

// hostHTTPBackend 返回按该插件 Manifest.AllowedDomains 收窄过的出站客户端。
//
// 白名单必须在这里定死：`pkg/host` 不再认识域名，插件拿到的客户端就是
// 「只能打它声明过的域」的那一份。将来给不同插件分配不同限流桶也改这一处。
//
// 未配置客户端时必须返回**未包装的 nil 接口**——把 (*httpx.Client)(nil) 或
// hostHTTP{nil} 塞进接口会得到一个非 nil 的接口值，host 侧的判空随之失效，
// 调用即 panic（核心单测大量使用不接 HTTP 的 Core）。
func (c *Core) hostHTTPBackend(domains []string) host.HTTPBackend {
	if c.http == nil {
		return nil
	}
	return hostHTTP{c.http.WithDomains(domains)}
}

// Shutdown 按固定顺序收尾：停调度 → 等调度收尾 → 等 Dispatcher 排空 →
// 等 GC 退出 → 关插件。
//
// 返回非 nil 表示有一步没在期限内退出：此时调用方**不能**当成优雅关闭——
// 关库很可能与在途写并发，必须以非 0 退出，让 systemd 的 Restart=on-failure
// 看见（P3-23）。每一步都必须早于 main 里的 db.Close：调度任务、派发中的
// Handle、GC 的 Purge 都在写库。
func (c *Core) Shutdown(ctx context.Context) error {
	var errs []error

	// 先停调度并等任务收尾，再关插件：任务运行在插件的 JobRunner 里，先 Close
	// 插件等于让任务往已经关掉的东西里写。Stop 是必需的——/reload 路径下 main
	// 不会取消 Start 收到的 ctx，只停信号监听；不主动 Stop 就会在关库之后继续起新桶。
	if c.sched != nil {
		c.sched.Stop()
		if !c.sched.Wait(ctx) {
			c.log.Error("调度任务未在关停期限内退出，任务状态可能未落盘")
			errs = append(errs, fmt.Errorf("调度收尾超时: %w", ctx.Err()))
		}
	}

	// dispatcher 同理：Handle 跑在插件里，先关插件就是让在途消息踩空。
	// 排队中的事件也要处理完——它们已经被平台投递过，丢掉等于这条消息永远没回。
	if c.dispatch != nil && !c.dispatch.Wait(ctx) {
		c.log.Error("Dispatcher 未在关停期限内排空，在途消息可能丢失")
		errs = append(errs, fmt.Errorf("派发排空超时: %w", ctx.Err()))
	}

	if err := c.waitGCLoop(ctx); err != nil {
		errs = append(errs, err)
	}

	ClosePlugins(ctx, c.log, c.plugins)
	return errors.Join(errs...)
}

// ClosePlugins 逆序关闭一组插件：后启动的先关，失败只 Warn 不中断。
//
// 导出给装配层用：`internal/instance` 在 core.New 之前就装好了插件，所以
// 装配中途失败时插件已经活着而 Core 还不存在，那条路径必须自己收尾。
// 两处共用一份实现，避免"核心关了插件但它没关"这类只在异常路径出现的分叉。
func ClosePlugins(ctx context.Context, log *slog.Logger, plugins []host.Plugin) {
	for i := len(plugins) - 1; i >= 0; i-- {
		p := plugins[i]
		cl, ok := p.(host.Closer)
		if !ok {
			continue
		}
		if err := cl.Close(ctx); err != nil {
			log.Warn("关闭插件失败", "plugin", p.Manifest().Name, "err", err)
		}
	}
}

// waitGCLoop 等 GC 协程真正退出。它每 10 分钟才醒一次，ctx 取消后能立刻
// select 到 Done；这里等不到只可能是它正卡在一条 Purge 里。
func (c *Core) waitGCLoop(ctx context.Context) error {
	if !c.gcStarted || c.gcDone == nil {
		return nil
	}
	select {
	case <-c.gcDone:
		return nil
	case <-ctx.Done():
		c.log.Error("GC 未在关停期限内退出，关库可能与清理写并发")
		return fmt.Errorf("GC 收尾超时: %w", ctx.Err())
	}
}

// Handle 是接入层唯一入口：群准入 → 去重 → 落库 → 核心保留指令 → 路由。
func (c *Core) Handle(ctx context.Context, ev *host.Event) {
	if ev == nil {
		c.log.Warn("丢弃空事件")
		return
	}
	// 未知事件只观测、不路由：它是"平台推了但本地不认识"的唯一出口，
	// 不进幂等表、不落库、不触发任何插件（否则就成了绕过 Manifest.Intents 的后门）。
	if ev.Kind == host.KindUnknown {
		c.trace.Unknown(ev)
		c.log.Debug("未知事件，仅记录不路由", "name", ev.Name)
		return
	}
	// 轨迹要显示 Bot 角色（owner/admin/user），而"收到"块打印在去重与闸门之前。
	// 只在 full 级别提前解析：关闭态与 plugin 级别保持原样，不给热路径加查询；
	// resolveActor 幂等，后面那次会直接命中"已解析"分支。
	if c.trace.full() {
		c.resolveActor(ctx, ev)
	}
	// 先打轨迹再去重：调试模式下"收到的每一条"都要看见，包括重复投递的那条。
	c.trace.Received(ctx, ev)
	if ev.ID == "" {
		c.log.Warn("丢弃缺少事件 ID 的事件", "name", ev.Name)
		return
	}

	// 群准入：在去重与落库之前判定，未授权的群不会留下任何业务数据。
	// 两个例外：
	//   - Bot 管理员的 /group 指令，否则新群无法在群内自助接入；
	//   - 群生命周期通知（入群/退群），见下。
	//
	// 生命周期通知不走闸门：它们描述的是机器人自身的状态变化，不是群里的消息。
	// 新群此刻必然还没被放行，走闸门就会被丢掉，结果是"被谁拉进了哪个群"
	// 永远没人知道。它们照样参与后面的去重。
	if ev.Notice == host.NoticeNone {
		if allowed, g, reason := c.groupAccess(ctx, ev); !allowed {
			if !c.groupControlExempt(ctx, ev, g) {
				c.trace.GroupDropped(ev, reason)
				c.log.Debug("群未放行，丢弃事件",
					"peer", ev.PeerID, "mode", g.Mode, "reason", reason)
				return
			}
		}
	}

	seen, err := c.store.MarkEventSeen(ctx, ev.ID)
	if err != nil {
		// 去重表不可用时宁可不处理：重复回复会直接消耗被动额度
		c.log.Error("事件去重失败，跳过", "event", ev.ID, "err", err)
		return
	}
	if !seen {
		c.trace.Duplicate(ev)
		c.log.Debug("重复事件，忽略", "event", ev.ID)
		return
	}

	// 生命周期处理必须排在去重之后：平台重投同一条入群事件时，
	// 不能给所有者发两遍私聊，也不能把 left_at 反复改来改去。
	if c.handleGroupLifecycle(ctx, ev) {
		return
	}

	// 权限视图必须在落库、核心指令与插件路由之前就位：下游只读 ev.Actor。
	// 重复事件不会走到这里，所以每条真正处理的事件只产生一次查询。
	c.resolveActor(ctx, ev)

	if ev.IsMessage() {
		if err := c.store.InsertMessage(ctx, store.Message{
			EventID:      ev.ID,
			MsgID:        ev.MsgID,
			Scene:        string(ev.Scene),
			PeerOpenID:   ev.PeerID,
			SenderOpenID: ev.SenderID,
			MsgType:      ev.MsgType,
			Content:      ev.Content,
			HasMedia:     ev.HasMedia,
		}); err != nil {
			c.log.Warn("消息落库失败", "event", ev.ID, "err", err)
		}
		// 记录跨场景身份映射：群里的 member_openid 与单聊的 user_openid 不通用，
		// 只有 union_openid 能把两者认成同一个人（群认领的 owner 私聊通知依赖它）。
		if ev.UnionID != "" && ev.SenderID != "" {
			if err := c.store.LinkIdentity(ctx, ev.SenderID, ev.UnionID,
				string(ev.Scene), peerScope(ev)); err != nil {
				c.log.Debug("记录身份映射失败", "event", ev.ID, "err", err)
			}
		}
	}

	if c.handleReserved(ctx, ev) {
		return
	}
	c.dispatch.Dispatch(ev)
}

func (c *Core) replyText(ctx context.Context, ev *host.Event, text string) {
	if err := c.sender.Reply(ctx, ev, host.Reply{Content: text}); err != nil {
		c.log.Warn("发送核心回复失败", "event", ev.ID, "err", err)
	}
}

// PrintRaw 让接入层把平台原始 JSON 回吐到同一条调试轨迹上（[debug].raw=true）。
// 放在 Core 上而不是 gateway 里，是为了与消息轨迹共用一把锁、输出不交错。
func (c *Core) PrintRaw(typ string, raw []byte) { c.trace.Raw(typ, raw) }

// activePolicy 把配置转成 Sender 用的闸门。白名单为空表示谁都不能发主动消息。
func activePolicy(cfg *config.Config) ActivePolicy {
	allowed := make(map[string]bool, len(cfg.Active.Plugins))
	for _, name := range cfg.Active.Plugins {
		allowed[name] = true
	}
	return ActivePolicy{
		Enabled:      cfg.ActiveEnabled(),
		Plugins:      allowed,
		DailyPerPeer: cfg.Active.DailyPerPeer,
	}
}

// peerScope 决定一条授权的有效范围：群内授权只在那个群生效，单聊授权全局（仅单聊）。
func peerScope(ev *host.Event) string {
	if ev.Scene == host.SceneGroup {
		return ev.PeerID
	}
	return ""
}
