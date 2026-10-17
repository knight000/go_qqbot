package core

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/host"
)

// Dispatcher 的实现要点（§2.1）：
//   - 同一 peer 串行：msg_seq 必须原子递增，且避免插件对同一用户并发查询；
//   - 跨 peer 并行：工作池上限 16，超限排队而非丢弃；
//   - 每个 Handle 都包 recover，插件 panic 不得带走核心。
type Dispatcher struct {
	router  *Router
	sender  *Sender
	log     *slog.Logger
	handler time.Duration
	trace   *Trace

	sem chan struct{}

	mu    sync.Mutex
	peers map[string]*peerQueue
}

type peerQueue struct {
	ch     chan *host.Event
	closed bool
}

const (
	queueDepth    = 16
	idleEvictTime = 10 * time.Minute
	workerLimit   = 16
)

func NewDispatcher(r *Router, s *Sender, log *slog.Logger, handlerTimeout time.Duration) *Dispatcher {
	if handlerTimeout <= 0 {
		handlerTimeout = 3 * time.Second
	}
	return &Dispatcher{
		router:  r,
		sender:  s,
		log:     log,
		handler: handlerTimeout,
		sem:     make(chan struct{}, workerLimit),
		peers:   make(map[string]*peerQueue),
	}
}

// Dispatch 只做入队，不阻塞 WSS 读循环。
func (d *Dispatcher) Dispatch(ctx context.Context, ev *host.Event) {
	key := string(ev.Scene) + ":" + ev.PeerID

	d.mu.Lock()
	q, ok := d.peers[key]
	if !ok || q.closed {
		q = &peerQueue{ch: make(chan *host.Event, queueDepth)}
		d.peers[key] = q
		go d.peerLoop(ctx, key, q)
	}
	d.mu.Unlock()

	select {
	case q.ch <- ev:
	default:
		// 背压：队列满说明该 peer 处理不过来，宁可丢事件也不拖垮全局
		d.log.Warn("peer 队列已满，丢弃事件", "peer", key, "event", ev.ID, "type", ev.Type)
	}
}

func (d *Dispatcher) peerLoop(ctx context.Context, key string, q *peerQueue) {
	timer := time.NewTimer(idleEvictTime)
	defer timer.Stop()

	for {
		select {
		case ev := <-q.ch:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(idleEvictTime)
			d.handle(ctx, ev)

		case <-timer.C:
			d.mu.Lock()
			if !q.closed && len(q.ch) == 0 {
				q.closed = true
				delete(d.peers, key)
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
			timer.Reset(idleEvictTime)

		case <-ctx.Done():
			return
		}
	}
}

func (d *Dispatcher) handle(ctx context.Context, ev *host.Event) {
	matches := d.router.Match(ev)
	if len(matches) == 0 {
		d.trace.NoMatch(ev)
		d.log.Debug("无插件匹配", "type", ev.Type, "content", ev.Content)
		return
	}

	top := matches[0]
	chosen := matches[:1]
	if top.Manifest.Broadcast {
		chosen = chosen[:0]
		for _, m := range matches {
			if m.Priority != top.Priority {
				break
			}
			chosen = append(chosen, m)
		}
	}

	for _, m := range chosen {
		d.trace.Hit(m, ev)
		d.run(ctx, m, ev)
	}
}

func (d *Dispatcher) run(ctx context.Context, m Match, ev *host.Event) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-d.sem }()

	hctx, cancel := context.WithTimeout(ctx, d.handler)
	defer cancel()

	start := time.Now()
	reply, err := d.invoke(hctx, m, ev)
	cost := time.Since(start)
	d.trace.PluginDone(m, cost, reply, err)
	if err != nil {
		d.log.Error("插件处理失败",
			"plugin", m.Manifest.Name, "event", ev.ID, "type", ev.Type,
			"cost", cost.String(), "err", err)
		return
	}
	if reply == nil {
		return
	}

	// handler ctx 可能已被取消，回复必须用独立 ctx，否则平台侧收到空请求
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer rcancel()
	if err := d.sender.Reply(rctx, ev, *reply); err != nil {
		if errors.Is(err, ErrRateLimited) {
			// 429 是平台侧事实，本地账本需要据此校正；不能盲目重试（重试可能仍是 429）
			d.log.Warn("回复被平台限流(429)，请检查本地额度与平台配额",
				"plugin", m.Manifest.Name, "scene", ev.Scene, "peer", ev.PeerID, "err", err)
			return
		}
		d.log.Warn("回复失败", "plugin", m.Manifest.Name, "event", ev.ID, "err", err)
	}
}

func (d *Dispatcher) invoke(ctx context.Context, m Match, ev *host.Event) (reply *host.Reply, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &panicError{value: rec}
			d.log.Error("插件 panic",
				"plugin", m.Manifest.Name, "event", ev.ID,
				"panic", rec, "stack", string(debug.Stack()))
		}
	}()
	return m.Plugin.Handle(ctx, ev)
}

type panicError struct{ value any }

func (e *panicError) Error() string {
	return "plugin panic"
}
