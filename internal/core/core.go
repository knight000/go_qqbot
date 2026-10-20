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
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
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

	// reconnect 是网关的主动重连入口（R0）。由 main 在起网关协程之前注入，
	// 因此 Handle 读取它与 main 赋值之间天然有 happens-before。
	reconnect Reconnector
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
	sender := NewSender(o.API, logger.With("component", "sender"), activePolicy(o.Config))
	sender.trace = trace
	sched := NewScheduler(o.Store, logger.With("component", "scheduler"))
	dispatch := NewDispatcher(router, sender, logger.With("component", "dispatch"), 3*time.Second)
	dispatch.trace = trace
	dispatch.denyReply = o.Config.Permission.DenyReply

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
		sender:    sender,
		router:    router,
		dispatch:  dispatch,
		sched:     sched,
		startedAt: time.Now(),
		restart:   make(chan RestartRequest, 1),
	}, nil
}

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
			HTTP:     c.httpClient(),
			Sched:    c.sched,
			Log:      c.log,
			Domains:  m.AllowedDomains,
			Renderer: c.render,
		})
		if err := p.Init(ctx, svc); err != nil {
			return fmt.Errorf("初始化插件 %s: %w", m.Name, err)
		}
	}

	if err := c.sched.Start(ctx); err != nil {
		return fmt.Errorf("启动调度器: %w", err)
	}

	for _, p := range c.plugins {
		if st, ok := p.(host.Starter); ok {
			if err := st.Start(ctx); err != nil {
				return fmt.Errorf("启动插件 %s: %w", p.Manifest().Name, err)
			}
		}
	}

	if err := c.ensureOwner(ctx); err != nil {
		return err
	}
	c.logGroupGateState(ctx)
	go c.gcLoop(ctx)

	c.log.Info("核心已启动", "plugins", len(c.plugins), "sandbox", c.cfg.App.Sandbox)
	return nil
}

// httpClient 当前没有在 Core 里保存引用，单独抽一个方法是为了将来
// 给不同插件分配不同限流桶时只改一处。
func (c *Core) httpClient() *httpx.Client { return c.http }

func (c *Core) Shutdown(ctx context.Context) {
	for i := len(c.plugins) - 1; i >= 0; i-- {
		p := c.plugins[i]
		if cl, ok := p.(host.Closer); ok {
			if err := cl.Close(ctx); err != nil {
				c.log.Warn("关闭插件失败", "plugin", p.Manifest().Name, "err", err)
			}
		}
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
	c.dispatch.Dispatch(ctx, ev)
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
