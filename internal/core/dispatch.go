package core

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
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

	// runCtx 是 peerLoop 的生命周期，与网关收到的 ctx 解耦：关停时先要把
	// 队列排空（排空期间的写库不能被打断），排空之后才由 Wait 取消它让协程
	// 退出。直接复用网关 ctx 的话，信号一到协程就退出，队列里剩下的事件被
	// 丢掉、正在 Handle 里的写库也会 context canceled（P2-7）。
	runCtx    context.Context
	stopLoops context.CancelFunc
	loops     sync.WaitGroup

	// sendCtx 是发送的生命周期：与 handler 的 3s 截止解耦（否则图片上传会被
	// 压回 5s），但只由关停取消。Wait 到期时掐断在途发送，避免一条 30s 上传
	// 把关库拖到发送之后（现场就是 "database is closed"），也让关停不必为
	// 一条发不出去的图超期退出。
	sendCtx     context.Context
	cancelSends context.CancelFunc

	mu    sync.Mutex
	peers map[string]*peerQueue
	// inflight 是"已入队、尚未处理完"的事件数，Wait 靠它判断排空。
	inflight int
	// idle 在 inflight 从 0 变正时重建、归零时关闭，供 Wait 使用。
	idle chan struct{}
	// draining 为 true 之后 Dispatch 不再接收新事件：关停中的进程不该再
	// 往队列里塞东西，否则 Wait 永远等不到归零。
	draining bool
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
	runCtx, stopLoops := context.WithCancel(context.Background())
	sendCtx, cancelSends := context.WithCancel(context.Background())
	return &Dispatcher{
		router:      r,
		sender:      s,
		log:         log,
		handler:     handlerTimeout,
		sem:         make(chan struct{}, workerLimit),
		runCtx:      runCtx,
		stopLoops:   stopLoops,
		sendCtx:     sendCtx,
		cancelSends: cancelSends,
		peers:       make(map[string]*peerQueue),
	}
}

// Dispatch 只做入队，不阻塞 WSS 读循环。
func (d *Dispatcher) Dispatch(ev *host.Event) {
	key := string(ev.Scene) + ":" + ev.PeerID

	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		d.log.Warn("关停中，丢弃新事件", "peer", key, "event", ev.ID, "name", ev.Name)
		return
	}
	q, ok := d.peers[key]
	if !ok || q.closed {
		q = &peerQueue{ch: make(chan *host.Event, queueDepth)}
		d.peers[key] = q
		d.loops.Add(1)
		go d.peerLoop(key, q)
	}

	// 入队与计数必须和"查找队列"在同一个临界区：peerLoop 的空闲回收会
	// 摘掉空队列，若在解锁之后才发送，事件会落进没人读的 channel ——
	// inflight 永远不归零，Wait 只能等到超时。
	if d.inflight == 0 {
		d.idle = make(chan struct{})
	}
	d.inflight++
	select {
	case q.ch <- ev:
		d.mu.Unlock()
	default:
		d.finishOne()
		d.mu.Unlock()
		// 背压：队列满说明该 peer 处理不过来，宁可丢事件也不拖垮全局
		d.log.Warn("peer 队列已满，丢弃事件", "peer", key, "event", ev.ID, "name", ev.Name)
	}
}

// finishOne 记账"一条事件处理完了"，并在归零时唤醒 Wait。
func (d *Dispatcher) finishOne() {
	d.inflight--
	if d.inflight == 0 {
		close(d.idle)
	}
}

func (d *Dispatcher) peerLoop(key string, q *peerQueue) {
	defer d.loops.Done()

	timer := time.NewTimer(idleEvictTime)
	defer timer.Stop()

	for {
		select {
		case ev := <-q.ch:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(idleEvictTime)
			d.handle(d.runCtx, ev)
			d.mu.Lock()
			d.finishOne()
			d.mu.Unlock()

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

		case <-d.runCtx.Done():
			return
		}
	}
}

// Wait 关掉入口、等在途与排队的事件处理完，然后让 per-peer 协程退出。
//
// 返回 false 表示 ctx 先到期：此时关库会与在途写并发，调用方必须以非 0
// 退出（P3-23），不能当成"优雅关闭"糊过去。
func (d *Dispatcher) Wait(ctx context.Context) bool {
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()

	drained := true
	for {
		d.mu.Lock()
		if d.inflight == 0 {
			d.mu.Unlock()
			break
		}
		idle := d.idle
		d.mu.Unlock()

		select {
		case <-idle:
		case <-ctx.Done():
			// 关停期限已到：在途发送必须立刻放手，否则一条 30s 的上传会
			// 把关库推到发送之后（写库撞 "database is closed"），关停也
			// 必然以超期收场。掐断后再给 handler 一点收尾时间——发送一旦
			// 返回，剩下的只是记日志，正常在毫秒级结束。
			d.cancelSends()
			drained = waitClosed(idle, sendCancelGrace)
		}
		if !drained {
			break
		}
	}

	// 超时也要停协程：卡住的 Handle 再不放弃，进程就退不掉了。
	// 这一步之后的写库失败是超时的代价，已经由返回 false 报告给调用方。
	d.stopLoops()

	exited := make(chan struct{})
	go func() {
		d.loops.Wait()
		close(exited)
	}()

	// ctx 可能已经到期（超时路径）。不能再用它等协程退出：select 在两个
	// case 同时就绪时随机选，会让"排空已顺利完成"随机报成失败。
	waitCtx, cancel := ctx, context.CancelFunc(func() {})
	if ctx.Err() != nil {
		waitCtx, cancel = context.WithTimeout(context.Background(), sendCancelGrace)
	}
	defer cancel()

	select {
	case <-exited:
		return drained
	case <-waitCtx.Done():
		return false
	}
}

// sendCancelGrace 是掐断在途发送之后等 handler 收尾的窗口。发送已经返回，
// 剩下的只有日志与返回，正常在毫秒级；给 2s 只为容忍一次日志落盘。
const sendCancelGrace = 2 * time.Second

// waitClosed 在 d 时间内等 ch 关闭，false 表示没等到。
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
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

	// 发送预算由 Sender 按回复内容决定（纯文本 5s、含上传 30s），这里只剥掉
	// handler 的截止时间，不再自己拍一个 5s——那会把上传的 30s 压回 5s。
	// 但剥掉的取消权必须由关停接手（sendCtx）：否则关停只能干等满一整条
	// 上传预算，30s 之后才开始关库。
	// 插件拿不到 Reply 的错误，超时判断与降级只能在核心内做。
	if err := d.sender.Reply(d.sendCtx, ev, *reply); err != nil {
		if errors.Is(err, host.ErrRateLimited) {
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
