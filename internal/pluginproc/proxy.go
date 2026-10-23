package pluginproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// errUnavailable 表示这个插件当前没有可用的进程：正在退避重启、已被停用，
// 或者还没 Init。调用方据此快速失败，而不是等满 3 秒超时。
//
// 为什么单独一个哨兵：核心的派发层会把 Handler 的错误原样记日志，而
// "插件停用了"与"插件这次算错了"是两条完全不同的排障路径。
var errUnavailable = errors.New("pluginproc: 插件进程不可用")

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

	mu   sync.Mutex
	host host.Host
	cfg  map[string]any
	// cur 是当前活着的那个进程。nil 表示还没起、正在退避、或已被停用。
	cur *runtimeProc
	// closing 之后不再重启：关停与 /reload 都走 Close，那时"进程退出"是
	// 预期事件，不能触发退避重启。
	closing bool
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

	if err := p.spawn(ctx); err != nil {
		return fmt.Errorf("初始化插件 %s: %w", p.name, err)
	}
	return nil
}

// Handle 把一条事件投给插件。超时口径由 rpc.CallHandle 兜底（3 秒），
// 与进程内插件完全一致——进程边界不是放宽语义的理由（决策 7）。
func (p *proxy) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	r := p.running()
	if r == nil {
		return nil, fmt.Errorf("%w: %s", errUnavailable, p.name)
	}
	return r.conn.CallHandle(ctx, ev)
}

// running 取当前进程。关停中一律返回 nil：那一刻的连接正在被三级关闭拆掉，
// 往里投递只会得到一堆"连接已关闭"。
func (p *proxy) running() *runtimeProc {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return nil
	}
	return p.cur
}

// spawn 起一个进程并完成握手。
func (p *proxy) spawn(ctx context.Context) error {
	p.mu.Lock()
	h, cfg := p.host, p.cfg
	p.mu.Unlock()

	r, err := startProcess(p.bin, p.reg, p.log)
	if err != nil {
		return err
	}
	// host.* 必须在 Run 之前挂上：插件在 Init 里就会调 Host（Config() 不过
	// 通道，但 SendActive / Scheduler 会），那时代码已经在读这条连接了。
	if err := rpc.ServeHost(r.conn, h); err != nil {
		r.shutdown()
		return err
	}
	go func() { _ = r.conn.Run() }()

	if err := r.conn.CallInit(ctx, p.name, cfg); err != nil {
		r.shutdown()
		return err
	}

	p.mu.Lock()
	p.cur = r
	p.mu.Unlock()
	return nil
}
