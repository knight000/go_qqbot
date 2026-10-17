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
	"time"

	"github.com/knight000/qqbot/internal/api"
	"github.com/knight000/qqbot/internal/config"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/httpx"
	"github.com/knight000/qqbot/internal/store"
)

type Options struct {
	Store   *store.DB
	API     *api.Client
	HTTP    *httpx.Client
	Log     *slog.Logger
	Config  *config.Config
	Plugins []host.Plugin
	// Trace 为 nil 且配置里打开了 debug 时，自动输出到 stdout。
	// 显式传入只给测试用（可写进 buffer）。
	Trace *Trace
}

type Core struct {
	store   *store.DB
	cfg     *config.Config
	api     *api.Client
	http    *httpx.Client
	log     *slog.Logger
	plugins []host.Plugin
	trace   *Trace

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
	}
	sender := NewSender(o.API, logger.With("component", "sender"), activePolicy(o.Config))
	sender.trace = trace
	sched := NewScheduler(o.Store, logger.With("component", "scheduler"))
	dispatch := NewDispatcher(router, sender, logger.With("component", "dispatch"), 3*time.Second)
	dispatch.trace = trace
	dispatch.denyReply = o.Config.Permission.DenyReply

	return &Core{
		store:     o.Store,
		cfg:       o.Config,
		api:       o.API,
		http:      o.HTTP,
		log:       logger,
		plugins:   o.Plugins,
		trace:     trace,
		sender:    sender,
		router:    router,
		dispatch:  dispatch,
		sched:     sched,
		startedAt: time.Now(),
		restart:   make(chan RestartRequest, 1),
	}, nil
}

// Start 依次完成：绑定任务执行者 → 插件 Init（注册 Job）→ 启动调度 → 启动插件 → 生成认领令牌。
func (c *Core) Start(ctx context.Context) error {
	for _, p := range c.plugins {
		c.sched.Bind(p.Manifest().Name, p)
	}

	for _, p := range c.plugins {
		m := p.Manifest()
		svc := host.NewService(host.Options{
			Name:    m.Name,
			Config:  host.Params(c.cfg.Plugin(m.Name)),
			Sender:  c.sender.ForPlugin(m.Name),
			KV:      c.store,
			DB:      c.store,
			Admin:   adminStore{c.store},
			HTTP:    c.httpClient(),
			Sched:   c.sched,
			Log:     c.log,
			Domains: m.AllowedDomains,
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
	// 先打轨迹再去重：调试模式下"收到的每一条"都要看见，包括重复投递的那条。
	c.trace.Received(ev)
	if ev.ID == "" {
		c.log.Warn("丢弃缺少事件 ID 的事件", "name", ev.Name)
		return
	}

	// 群准入：在去重与落库之前判定，未授权的群不会留下任何业务数据。
	// 例外只给 Bot 管理员的 /group 指令——否则新群无法在群内自助加入白名单。
	if allowed, g, reason := c.groupAccess(ctx, ev); !allowed {
		if !c.groupControlExempt(ctx, ev, g) {
			c.trace.GroupDropped(ev, reason)
			c.log.Debug("群未放行，丢弃事件",
				"peer", ev.PeerID, "mode", g.Mode, "reason", reason)
			return
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
