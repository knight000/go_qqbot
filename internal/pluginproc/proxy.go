package pluginproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// ErrUnavailable 表示这个插件当前没有可用的进程：正在退避重启、已被停用，
// 或者还没 Init。调用方据此快速失败，而不是等满一个超时。
//
// 为什么单独一个哨兵：核心的派发层会把 Handler 的错误原样记日志，而
// "插件停用了"与"插件这次算错了"是两条完全不同的排障路径。
var ErrUnavailable = errors.New("pluginproc: 插件进程不可用")

// maxFailures 是熔断阈值：连续失败（超时与崩溃共用计数）到这个数就停用
// 该插件（决策 8）。与 job 熔断的 5 次同口径——同一台机器上两套"几次算坏"
// 只会让运维多记一个数字。
const maxFailures = 5

// proxy 是一个插件子进程在核心眼里的全部形态。
//
// 它实现的接口就是 host.Plugin 加三个可选接口，一个不多：核心只 type-assert
// 那三个（Starter / Closer / JobRunner），所以 proxy 必须全都实现——「插件
// 那边到底有没有」由握手时 plugin.manifest 报上来的 optional 决定，不是
// 靠核心先调一次试探（那会把"插件启动即崩"和"本来就没实现"混成一个现象）。
type proxy struct {
	name     string
	bin      string
	manifest host.Manifest
	// optional 是插件真的实现了哪几个可选接口（plugin.manifest 的应答）。
	optional map[string]bool
	// configFor 在 Init 时取一次配置快照：插件配置不热更新（ADR-0005）。
	configFor func(string) map[string]any
	reg       *rpc.Registry
	log       *slog.Logger
	// backoff 是崩溃重启的等待表（默认 1/2/4/8/16 分钟，测试可缩短）。
	backoff []time.Duration

	mu   sync.Mutex
	host host.Host
	cfg  map[string]any
	// cur 是当前活着的那个进程。nil 表示还没起、正在退避、或已被停用。
	cur *runtimeProc
	// closing 之后不再重启：关停与 /reload 都走 Close，那时"进程退出"是
	// 预期事件，不能触发退避重启。
	closing bool
	// failures 是连续失败次数（超时与崩溃共用，决策 7/8）；disabled 之后
	// 不再起进程，调用一律立刻失败。
	failures int
	disabled bool
	// started 记录核心调过 Start：重启之后要把 Start 补上，否则新进程只是
	// "能接 handle"，后台协程/定时器一个都没起（插件自己不知道被重启过）。
	started bool
}

// newProxy 用探测结果造一个还没起进程的 proxy。
func newProxy(pr probeResult, o Options) host.Plugin {
	optional := make(map[string]bool, len(pr.optional))
	for _, m := range pr.optional {
		optional[m] = true
	}
	name := pr.manifest.Name
	return &proxy{
		name:      name,
		bin:       pr.file,
		manifest:  pr.manifest,
		optional:  optional,
		configFor: o.configFor,
		reg:       o.Registry,
		log:       o.logger().With("plugin", name),
		backoff:   o.backoff(),
	}
}

// Manifest 返回装配期探测到的清单，**不每次过通道**（ADR-0005 决策 3）。
//
// 核心在路由、日志、报告里会反复调它；每次往返既慢，又会把"这个插件声明了
// 什么命令"变成两次独立事实（探测时一次、运行时一次），而路由表是在启动时
// 一次性建好的——真出现差异也来不及改路由。
func (p *proxy) Manifest() host.Manifest { return p.manifest }

// Init 起子进程、把 host.* 挂到连接上、把配置快照交给插件。
//
// 这是「装上」与「能路由」的分界线：Init 返回之后插件才可能被投递事件。
// 配置取自装配期的快照（`[plugins.<name>]` 的原始 map）：插件自己也拿不到
// 配置，这套快照是它唯一的来源（ADR-0005「本 ADR 不做」一节：不做热更新）。
func (p *proxy) Init(ctx context.Context, h host.Host) error {
	p.mu.Lock()
	if p.host != nil {
		p.mu.Unlock()
		return fmt.Errorf("pluginproc: 插件 %s 已经初始化过（Init 只调用一次）", p.name)
	}
	p.host, p.cfg = h, p.configFor(p.name)
	p.mu.Unlock()

	err := p.start(ctx)
	if err == nil {
		return nil
	}
	if isTransport(err) {
		// 进程起不来就是崩溃：走退避重启，启动不失败（决策 6 / 8）。
		p.noteExit(err)
		return nil
	}
	return fmt.Errorf("初始化插件 %s: %w", p.name, err)
}

// Handle 把一条事件投给插件。超时口径由 rpc.CallHandle 兜底（3 秒），
// 与进程内插件完全一致——进程边界不是放宽语义的理由（决策 7）。
func (p *proxy) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	r := p.running()
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, p.name)
	}
	reply, err := r.conn.CallHandle(ctx, ev)
	if err != nil {
		// 只有超时算"这次插件不行"；插件自己返回的业务错误是正常输出，
		// 计入熔断会让一个爱报错的插件被当成坏产物停掉。
		if errors.Is(err, context.DeadlineExceeded) {
			p.noteFailure("handle 超时")
		}
		return nil, err
	}
	p.noteSuccess()
	return reply, nil
}

// RunJob 转发一个到点的任务。
//
// 期限沿用调用方给的 ctx（核心填的是 spec.Timeout，默认 60 秒）；调用方没给
// 时由 rpc.CallRunJob 按 JobTimeout 兜底。两个数字都只写在 pkg/rpc 一处，
// 这里不另拍一个"跨进程更宽松"的值（决策 7）。
func (p *proxy) RunJob(ctx context.Context, spec host.JobSpec) error {
	r := p.running()
	if r == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, p.name)
	}
	err := r.conn.CallRunJob(ctx, spec)
	if errors.Is(err, context.DeadlineExceeded) {
		p.noteFailure("run_job 超时")
	} else if err == nil {
		p.noteSuccess()
	}
	return err
}

// running 取当前进程。关停中一律返回 nil：那一刻的连接正在被三级关闭拆掉，
// 往里投递只会得到一堆"连接已关闭"。
func (p *proxy) running() *runtimeProc {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.disabled {
		return nil
	}
	return p.cur
}

// noteSuccess 把连续失败清零。"连败"是按调用计的，不是按启动计：一个起得来
// 却每次都超时的插件必须能被熔断，而一个崩过又稳定跑了一阵的插件不该带着
// 历史计数走向停用。
func (p *proxy) noteSuccess() {
	p.mu.Lock()
	p.failures = 0
	p.mu.Unlock()
}

// noteFailure 记一次连续失败，到阈值就停用并收掉进程。
func (p *proxy) noteFailure(reason string) {
	p.mu.Lock()
	p.failures++
	n := p.failures
	trip := n >= maxFailures && !p.disabled
	var r *runtimeProc
	if trip {
		p.disabled = true
		r, p.cur = p.cur, nil
	}
	p.mu.Unlock()

	if !trip {
		p.log.Warn("插件调用失败", "failures", n, "reason", reason)
		return
	}
	// 停用是一条要人看见的结论：运维第二天看到的是"某个指令没反应"，
	// 只有这里能说明它为什么不在。
	p.log.Error("插件连续失败，已停用", "failures", n, "reason", reason)
	if r != nil {
		// 卡死/半死的进程不该继续占着内存与句柄。
		r.shutdown()
	}
}

// Start 转发 plugin.start。插件没实现时是空操作：核心对每个 proxy 都会调它
// （proxy 这个类型永远实现了 Starter），"实现没实现"由 manifest 的 optional
// 说了算（pkg/rpc 的 capabilitiesOf 保证探测到的与注册的方法来自同一份断言）。
//
// 进程此刻不在（启动即崩，正在退避）时也返回 nil：这不是"启动失败"，退避
// 重启之后的 Init 会再把 Start 补上。
func (p *proxy) Start(ctx context.Context) error {
	if !p.optional[rpc.OptionalStart] {
		return nil
	}
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()

	r := p.running()
	if r == nil {
		return nil
	}
	return r.conn.CallStart(ctx)
}

// Close 走三级关闭：RPC close → （断连接等 EOF）→ 终止信号 → Kill。
//
// 每一级都有期限：关停不能卡在一个不听话的插件上（ADR-0005「代价」一节），
// 而最后一定落到 Kill——宁可杀掉一个正在写库的插件，也不能留下孤儿进程：
// 孤儿会占着单实例锁之外的一切（端口、KV 写入、平台的主动消息配额）。
//
// 幂等：/reload 与退出信号可能同时触发关停，第二次必须是无操作的成功。
func (p *proxy) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	p.closing = true
	r := p.cur
	p.cur = nil
	p.mu.Unlock()

	if r == nil {
		return nil
	}

	// 第一级：请插件自己收摊。它要靠这一步把内存里的状态写回 KV。
	if p.optional[rpc.OptionalClose] {
		cctx, cancel := context.WithTimeout(ctx, closeRPCGrace)
		err := r.conn.CallClose(cctx)
		cancel()
		if err != nil && !errors.Is(err, rpc.ErrMethodNotFound) {
			p.log.Warn("插件 close 未成功，继续关闭进程", "err", err)
		}
	}

	// 第二级：断连接。插件读到 stdin 的 EOF 会自己退出——这是绝大多数插件的
	// 正常退出路径，与第一级合起来就是"优雅关闭"。
	_ = r.conn.Close()
	if r.waitExit(closeExitWait) {
		return nil
	}

	// 第三级：信号（Windows 上没有 SIGTERM，只能 Kill，见 terminate_*.go）。
	p.log.Warn("插件未在期限内退出，发送终止信号", "wait", closeExitWait.String())
	if r.cmd.Process != nil {
		if err := terminate(r.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			p.log.Warn("发送终止信号失败", "err", err)
		}
	}
	if r.waitExit(closeKillWait) {
		return nil
	}

	// 兜底：Kill。走到这里说明插件既不理会 close 也不理会信号。
	p.log.Error("插件未响应终止信号，强制 Kill")
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	if !r.waitExit(closeKillWait) {
		return fmt.Errorf("pluginproc: 插件 %s 的进程在 Kill 之后仍未退出", p.name)
	}
	return nil
}

// 关闭三级的期限。总预算（3 秒 + 1 秒 Kill）要小到能在核心的关停窗口里
// 余下给"写回状态"的时间，又大到足够一个正常插件把 Close 跑完。
const (
	closeRPCGrace = time.Second
	closeExitWait = 2 * time.Second
	closeKillWait = time.Second
)

// spawn 起一个进程并完成握手。
//
// 失败分两类（见 startFailure 的注释）：进程侧起不来/中途死了属于崩溃，由
// 调用方转成退避重启；其余（含插件在 init 里报的错）原样上抛。
func (p *proxy) spawn(ctx context.Context) (*runtimeProc, error) {
	p.mu.Lock()
	h, cfg := p.host, p.cfg
	p.mu.Unlock()

	r, err := startProcess(p.bin, p.reg, p.log)
	if err != nil {
		return nil, &startFailure{err: err, transport: true}
	}
	// host.* 必须在 Run 之前挂上：插件在 Init 里就会调 Host（Config() 不过
	// 通道，但 SendActive / Scheduler 会），那时代码已经在读这条连接了。
	if err := rpc.ServeHost(r.conn, h); err != nil {
		r.shutdown()
		return nil, err
	}
	go func() { _ = r.conn.Run() }()

	if err := r.conn.CallInit(ctx, p.name, cfg); err != nil {
		r.shutdown()
		// 连接已关＝插件在 init 之前/之中就没了（崩溃）；其余是插件**答了**
		// "我初始化失败"——那是配置写错，按既有口径让启动失败（决策 6）。
		return nil, &startFailure{err: err, transport: errors.Is(err, rpc.ErrConnClosed)}
	}
	return r, nil
}

// startFailure 区分两种"起不来"。
//
// transport=true 表示问题在进程侧（起不来、半路死了、连接断了）：这是崩溃，
// 走退避重启，启动**不**失败（决策 6/8）。transport=false 表示插件活着并
// 明确拒绝了初始化（典型是配置里的密钥不对）：那是人改一下文件的事，
// fail-fast 才是对的——与后端启动失败时不当"没事"是同一条取舍。
type startFailure struct {
	err       error
	transport bool
}

func (e *startFailure) Error() string { return e.err.Error() }
func (e *startFailure) Unwrap() error { return e.err }

func isTransport(err error) bool {
	var f *startFailure
	return errors.As(err, &f) && f.transport
}

// start 起一代进程：握手成功就接管为当前进程，并派一个监工盯着它的退出。
func (p *proxy) start(ctx context.Context) error {
	r, err := p.spawn(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cur = r
	p.mu.Unlock()
	go p.supervise(r)
	return nil
}

// supervise 盯这一代进程的结局。
//
// 两个信号都要看：进程退出（正常崩溃）与连接断开（插件关了 stdout 人却还
// 在，属半死）。无论哪个，都只处理自己那一代（r），绝不碰 p.cur 上后来居上
// 的新进程——否则一次重启失败会把刚起来的进程杀掉，变成稳定的自我踩踏。
func (p *proxy) supervise(r *runtimeProc) {
	select {
	case <-r.exited:
	case <-r.conn.Done():
	}

	p.mu.Lock()
	current := p.cur == r
	closing := p.closing
	if current {
		p.cur = nil
	}
	p.mu.Unlock()

	if closing {
		// 关停路径自己在走三级关闭，监工不许插手：否则它的"连接一断就杀"
		// 会跳过 SIGTERM 那一级，插件连收尾的机会都没有。
		return
	}
	r.shutdown() // 幂等：连接已关就只差"进程若还活着就杀"
	if !current {
		// 这一代已经被别处作废（停用、或被新的一代替换），不再计数。
		return
	}
	p.noteExit(r.exitErr)
}

// noteExit 记一次"进程没了"，并按退避表安排重启；到阈值就停用。
func (p *proxy) noteExit(err error) {
	p.mu.Lock()
	p.failures++
	n := p.failures
	if n >= maxFailures {
		p.disabled = true
		p.mu.Unlock()
		p.log.Error("插件连续失败，已停用", "failures", n, "reason", "进程异常退出")
		return
	}
	delay := p.backoffAt(n)
	p.mu.Unlock()

	p.log.Error("插件进程异常退出，将按退避重启",
		"failures", n, "backoff", delay.String(), "err", err)
	time.AfterFunc(delay, p.restart)
}

// restart 是退避到期后的重试入口。
func (p *proxy) restart() {
	p.mu.Lock()
	closing, disabled := p.closing, p.disabled
	p.mu.Unlock()
	if closing || disabled {
		return
	}
	if err := p.start(context.Background()); err != nil {
		p.log.Error("插件重启失败", "err", err)
		p.noteExit(err)
		return
	}
	p.log.Info("插件已重启", "failures", p.failureCount())

	// 核心只会在启动时调一次 Start；重启出来的是新进程，后台协程要重新起。
	p.mu.Lock()
	needStart := p.started && p.optional[rpc.OptionalStart]
	p.mu.Unlock()
	if needStart {
		if err := p.Start(context.Background()); err != nil {
			p.log.Error("插件重启后 Start 失败", "err", err)
		}
	}
}

func (p *proxy) failureCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures
}

// backoffAt 返回第 n 次失败（从 1 起）之后等多久再试。
func (p *proxy) backoffAt(n int) time.Duration {
	idx := n - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(p.backoff) {
		idx = len(p.backoff) - 1
	}
	return p.backoff[idx]
}
