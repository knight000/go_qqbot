package core

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
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
	// denyReply 为 false 时，权限不足一律静默（默认）。
	// 拒绝语由插件 Manifest/Command 提供，是否发出由全局配置决定。
	denyReply bool
	// sessions 是会话接管的读取入口。为 nil 表示未装配（测试或未开库），
	// 此时会话路由整体不发生——静态匹配是安全退路。
	sessions sessionStore

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
		d.log.Warn("peer 队列已满，丢弃事件", "peer", key, "event", ev.ID, "name", ev.Name)
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

// sessionStore 是 Dispatcher 需要的最小会话视图。用窄接口而不是 *store.DB：
// 单元测试要能构造"行还在但持有者已经不在"这类状态，而真库上很难摆。
type sessionStore interface {
	Session(ctx context.Context, peer string) (sessionRow, bool, error)
	DeleteSession(ctx context.Context, peer string) error
}

// sessionRow 只带投递需要的四列。Peer / CreatedAt 故意不带：
// 前者就是查询键，后者只用于排障，Dispatcher 用不到。
type sessionRow struct {
	Plugin    string
	Scene     string
	State     []byte
	ExpiresAt time.Time
}

func (d *Dispatcher) handle(ctx context.Context, ev *host.Event) {
	// 会话接管必须在这里、不在 Core.Handle：这一层在 per-peer 串行队列里，
	// 在入队前查会话会让会话推进与静态路由并发打到同一个 peer（ADR-0004 决策 6）。
	if d.takeOverSession(ctx, ev) {
		return
	}
	matches := d.router.Match(ev)
	if len(matches) == 0 {
		d.trace.NoMatch(ev)
		d.log.Debug("无插件匹配", "name", ev.Name, "content", ev.Content)
		return
	}

	// 权限先于优先级：高优先级但无权执行的插件，不应挡住后面能处理这条消息的插件。
	// 典型场景是"管理员专用插件"与"公开插件"同时订阅 message。
	first := -1
	var denied []Match
	for i, m := range matches {
		if m.Perm.Admits(ev.Actor) {
			first = i
			break
		}
		denied = append(denied, m)
	}
	for _, m := range denied {
		d.trace.Denied(m, ev)
	}
	if first < 0 {
		// 全部候选都无权限：最多回一条拒绝语（插件声明的），绝不逐条回。
		if len(denied) > 0 {
			d.replyDenied(ctx, ev, denied[0])
		}
		return
	}

	// 同 priority 组内按序执行，遇到不让位者就停在这个插件（§3.4）：
	// “是否继续”是每个匹配者自己的声明（Manifest.Yield），不是排序第一名的属性。
	// 组内无权限的候选跳过并记录，跳过本身不改变“是否继续”。
	group := matches[first].Priority
	for i := first; i < len(matches); i++ {
		m := matches[i]
		if m.Priority != group {
			// 跨 priority 不扇出：低优先级插件不因高优先级让位而执行。
			break
		}
		if !m.Perm.Admits(ev.Actor) {
			d.trace.Denied(m, ev)
			continue
		}
		d.trace.Hit(m, ev)
		d.run(ctx, m, ev)
		if m.Manifest.Yield {
			continue
		}
		// 不让位：组内后面的都不再执行。逐个写进 full 级轨迹——否则就是
		// 「zzz 为什么没反应」永远查不出来的沉默（旧 Broadcast 的实现如此）。
		// 但"没执行"有两种原因，不能混成一句话：无权限的候选本来就不会跑，
		// 它该记「权限不足」；只有有权限、纯粹被挡住的才记「未让位」。
		for _, rest := range matches[i+1:] {
			if rest.Priority != group {
				break
			}
			if !rest.Perm.Admits(ev.Actor) {
				d.trace.Denied(rest, ev)
				continue
			}
			d.trace.Blocked(m, rest)
		}
		break
	}
}

// takeOverSession 尝试把这条消息交给会话持有者，返回 true 表示已投递、
// 调用方不得再走静态匹配。
//
// 为什么每一步失败都落回静态路由：会话是**隐式路由**，用户并不知道自己
// 正在某个插件的会话里。把消息丢掉造成的故障（"机器人不理我"）比多回一条
// 严重得多，所以除了"成功投递"以外的所有分支都退回可见的静态匹配。
func (d *Dispatcher) takeOverSession(ctx context.Context, ev *host.Event) bool {
	// 只有"用户说的话"能推进会话。notice、群生命周期、未知事件都不是答案，
	// 拿平台通知推进会话等于把系统事件当成用户输入。
	if d.sessions == nil || ev.Kind != host.KindMessage {
		return false
	}

	row, ok, err := d.sessions.Session(ctx, ev.PeerID)
	if err != nil {
		d.log.Error("读取会话失败，按无会话处理", "peer", ev.PeerID, "err", err)
		return false
	}
	if !ok {
		return false
	}
	// scene 是冗余列，存在只为了这一条校验：群 openid 与用户 openid 来自
	// 两个平台命名空间，取值万一相同，单聊消息不得消费群会话。
	if row.Scene != string(ev.Scene) {
		d.log.Debug("会话场景不匹配，不接管",
			"peer", ev.PeerID, "session_scene", row.Scene, "scene", ev.Scene)
		return false
	}
	now := time.Now()
	if sessionExpired(row.ExpiresAt, now) {
		// 过期等于行不存在：读路径顺手删掉，GC 只是兜底。
		if err := d.sessions.DeleteSession(ctx, ev.PeerID); err != nil {
			d.log.Warn("清理过期会话失败", "peer", ev.PeerID, "err", err)
		}
		d.log.Info("会话已过期，落回静态路由", "peer", ev.PeerID, "plugin", row.Plugin)
		return false
	}

	rt := d.router.Lookup(row.Plugin)
	if rt == nil {
		// 持有者卸载/重载后从路由表消失，这一行永远不会有人消费。
		if err := d.sessions.DeleteSession(ctx, ev.PeerID); err != nil {
			d.log.Warn("清理悬挂会话失败", "peer", ev.PeerID, "err", err)
		}
		d.log.Warn("会话持有者已不在，落回静态路由", "peer", ev.PeerID, "plugin", row.Plugin)
		return false
	}

	m := Match{
		Plugin:    rt.plugin,
		Manifest:  rt.manifest,
		Perm:      rt.manifest.Permission,
		DenyReply: rt.manifest.DenyReply,
	}
	// 权限先于接管：会话消息没有命令，所以只能用持有者的插件级权限。
	// 无权限**不删行**——否则一次越权尝试就能把管理员的会话顶掉；
	// 消息也不能吞掉，普通成员在群里说的话必须有别的去处。
	if !m.Perm.Admits(ev.Actor) {
		d.trace.Denied(m, ev)
		return false
	}

	// 先删后投（决策 3）：Handle 里再次 Await 写入的新行不会被这次清理吃掉。
	// 删失败就整条放弃投递——宁可这条消息走静态路由，也不能让"一次性"失效。
	if err := d.sessions.DeleteSession(ctx, ev.PeerID); err != nil {
		d.log.Error("删除会话失败，放弃接管", "peer", ev.PeerID, "plugin", row.Plugin, "err", err)
		return false
	}

	ev.Session = &host.SessionContext{Plugin: row.Plugin, State: row.State}
	d.trace.Session(m, ev, time.Until(row.ExpiresAt))
	d.run(ctx, m, ev)
	// 不 yield、不进静态匹配：独占是结构性结果，不是一个可配开关。
	return true
}

// replyDenied 发出插件声明的拒绝语。空串表示静默（默认），
// 避免在群里主动暴露"存在一条你看不到的指令"。
func (d *Dispatcher) replyDenied(ctx context.Context, ev *host.Event, m Match) {
	if !d.denyReply || m.DenyReply == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.sender.Reply(rctx, ev, host.Reply{Content: m.DenyReply}); err != nil {
		d.log.Warn("权限拒绝回复失败", "plugin", m.Manifest.Name, "event", ev.ID, "err", err)
		return
	}
	d.log.Debug("权限不足，已回复拒绝语",
		"plugin", m.Manifest.Name, "scene", ev.Scene, "peer", ev.PeerID,
		"role", ev.Actor.Role, "group_role", ev.Actor.GroupRole)
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

	// 命中的命令与参数按"当前匹配者"写入，位置只能在这里：Match 返回的是全部候选，
	// 在 Match 里写会被后面的候选覆盖；一次派发跑多个插件时（yield=true 的让位链），
	// 每个插件也必须拿到自己命中那条命令的 Command/Args，所以每个 m 各写一次。
	ev.Command, ev.Args = m.Command, m.Args

	start := time.Now()
	reply, err := d.invoke(hctx, m, ev)
	cost := time.Since(start)
	d.trace.PluginDone(m, cost, reply, err)
	if err != nil {
		d.log.Error("插件处理失败",
			"plugin", m.Manifest.Name, "event", ev.ID, "name", ev.Name,
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
