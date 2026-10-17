// Package core 是主体：幂等、落库、路由、频控、调度、GC。
// 这里不出现任何业务词（不出现 torn、天气、签到），业务全部在 plugins/。
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

type Core struct {
	store   *store.DB
	cfg     *config.Config
	http    *httpx.Client
	log     *slog.Logger
	plugins []host.Plugin

	sender   *Sender
	router   *Router
	dispatch *Dispatcher
	sched    *Scheduler

	claimMu    sync.Mutex
	claimToken string
	claimExp   time.Time
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
	sender := NewSender(o.API, logger.With("component", "sender"))
	sched := NewScheduler(o.Store, logger.With("component", "scheduler"))

	return &Core{
		store:    o.Store,
		cfg:      o.Config,
		http:     o.HTTP,
		log:      logger,
		plugins:  o.Plugins,
		sender:   sender,
		router:   router,
		dispatch: NewDispatcher(router, sender, logger.With("component", "dispatch"), 3*time.Second),
		sched:    sched,
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
			Sender:  c.sender,
			KV:      c.store,
			DB:      c.store,
			Admin:   c.store,
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

// Handle 是接入层唯一入口：去重 → 落库 → 核心保留指令 → 路由。
func (c *Core) Handle(ctx context.Context, ev *host.Event) {
	if ev == nil {
		c.log.Warn("丢弃空事件")
		return
	}
	if ev.ID == "" {
		c.log.Warn("丢弃缺少事件 ID 的事件", "type", ev.Type)
		return
	}

	seen, err := c.store.MarkEventSeen(ctx, ev.ID)
	if err != nil {
		// 去重表不可用时宁可不处理：重复回复会直接消耗被动额度
		c.log.Error("事件去重失败，跳过", "event", ev.ID, "err", err)
		return
	}
	if !seen {
		c.log.Debug("重复事件，忽略", "event", ev.ID)
		return
	}

	if ev.IsMessage() {
		if err := c.store.InsertMessage(ctx, store.Message{
			EventID:      ev.ID,
			MsgID:        ev.MsgID,
			Scene:        string(ev.Scene),
			PeerOpenID:   ev.PeerID,
			SenderOpenID: ev.SenderID,
			MsgType:      0,
			Content:      ev.Content,
			HasMedia:     ev.HasMedia,
		}); err != nil {
			c.log.Warn("消息落库失败", "event", ev.ID, "err", err)
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
