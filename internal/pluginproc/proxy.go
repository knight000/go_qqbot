package pluginproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
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
	// probe 是 `--manifest` 的期限，热升级要用它验新产物的身份。
	probe time.Duration

	mu sync.Mutex
	// hosts 是「哪个 Bot 用哪一份 Host」：共享进程下（ADR-0008 决策 11）
	// 一个插件实例服务 N 个 Bot，host.* 的每条调用都靠 bot_id 在这里选。
	hosts map[string]host.Host
	// cfgs 是每个 Bot 的配置快照（`[plugins.<name>]` 的原始 map）。配置是
	// 业务面的、按 Bot 不同（决策 10），所以按 Bot 各存一份、各 init 一次。
	cfgs map[string]map[string]any
	// cur 是当前活着的那个进程。nil 表示还没起、正在退避、或已被停用。
	cur *runtimeProc
	// closing 之后不再重启：关停与 /reload 都走 Close，那时"进程退出"是
	// 预期事件，不能触发退避重启。
	closing bool
	// replacing 表示正在热替换：旧进程退出与"新进程还没接上"都是预期的，
	// 监工不许插手（既不补退避重启，也不抢着 Kill——三级关闭由替换路径自己走）。
	replacing bool
	// failures 是连续失败次数（超时与崩溃共用，决策 7/8）；disabled 之后
	// 不再起进程，调用一律立刻失败。
	failures int
	disabled bool
	// started 记录核心为哪些 Bot 调过 Start：重启之后要把 Start 补上，否则新
	// 进程只是"能接 handle"，后台协程/定时器一个都没起（插件自己不知道被重启过）。
	started map[string]bool
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
		probe:     o.probeTimeout(),
		hosts:     make(map[string]host.Host),
		cfgs:      make(map[string]map[string]any),
		started:   make(map[string]bool),
	}
}

// Manifest 返回装配期探测到的清单，**不每次过通道**（ADR-0005 决策 3）。
//
// 核心在路由、日志、报告里会反复调它；每次往返既慢，又会把"这个插件声明了
// 什么命令"变成两次独立事实（探测时一次、运行时一次），而路由表是在启动时
// 一次性建好的——真出现差异也来不及改路由。
//
// 唯一会变的是版本号：热升级成功之后换成本进程真正在跑的版本，否则
// `/upgrade` 预览会一直认为"升级没生效"而反复建议同一个版本。加锁是因为
// 这条路要能被替换路径改写，而它在核心的派发路径上是并发读的。
func (p *proxy) Manifest() host.Manifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manifest
}

// BinPath 返回当前插件可执行文件的绝对路径。
//
// 升级路径要用它把新产物落到同一个目录（同卷才谈得上 rename），而目录
// 里的文件名只是标签——只有 proxy 自己知道这一份到底叫什么。
func (p *proxy) BinPath() string { return p.bin }

// Init 为**一个 Bot** 装上这个插件：登记它的 Host，然后（必要时起进程并）
// 发一次 plugin.init。
//
// 这是「这个 Bot 的插件」与「能路由」的分界线：Init 返回之后核心才会为这个
// Bot 投递事件。共享进程下每个 (Bot, 插件) 各调一次，进程只在第一个 Bot 时
// 起——这就是「一套进程服务所有 Bot」的落点（ADR-0008 决策 11）。
//
// 哪个 Bot 由 ctx 里的 host.BotOf 决定（host.Plugin.Init 的签名里没有 bot
// 参数，见 core.Start）。
func (p *proxy) Init(ctx context.Context, h host.Host) error {
	bot, ok := host.BotOf(ctx)
	if !ok || bot.IsZero() {
		return fmt.Errorf("pluginproc: 插件 %s 的 Init 缺少 Bot 上下文", p.name)
	}
	p.mu.Lock()
	if _, dup := p.hosts[bot.AppID]; dup {
		p.mu.Unlock()
		return fmt.Errorf("pluginproc: 插件 %s 已经为 Bot %s 初始化过（Init 每个 Bot 只调用一次）", p.name, bot.AppID)
	}
	p.hosts[bot.AppID] = h
	p.cfgs[bot.AppID] = p.configFor(p.name)
	alive := p.cur != nil
	p.mu.Unlock()

	err := p.initBot(ctx, bot, alive)
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

// initBot 让插件为某个 Bot 完成一次握手。进程不在时先起一个（第一个 Bot
// 顺便把 host.* 的服务端挂上），在的时候直接发这次 init。
func (p *proxy) initBot(ctx context.Context, bot host.BotInfo, alive bool) error {
	if !alive {
		return p.startAll(ctx)
	}
	r := p.running()
	if r == nil {
		// 正在退避/停用：Init 不失败，等重启时 startAll 会把每个 Bot 都补上
		// （与"启动即崩"同一条口径）。
		return nil
	}
	cfg := p.configOf(bot.AppID)
	if err := r.conn.CallInit(ctx, bot, p.name, cfg); err != nil {
		return &startFailure{err: err, transport: errors.Is(err, rpc.ErrConnClosed)}
	}
	return nil
}

func (p *proxy) configOf(botID string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cfg := p.cfgs[botID]; cfg != nil {
		return cfg
	}
	return map[string]any{}
}

// botOf 取本次调用属于哪个 Bot。共享进程下这是唯一的分发依据——没有它
// 就只能猜，而猜错就是跨租户串库。
func (p *proxy) botOf(ctx context.Context) (host.BotInfo, error) {
	bot, ok := host.BotOf(ctx)
	if !ok || bot.IsZero() {
		return host.BotInfo{}, fmt.Errorf("%w: %s 的调用缺少 Bot 上下文", ErrUnavailable, p.name)
	}
	return bot, nil
}

// Handle 把一条事件投给插件。超时口径由 rpc.CallHandle 兜底（3 秒），
// 与进程内插件完全一致——进程边界不是放宽语义的理由（决策 7）。
//
// 事件属于哪个 Bot 由 ctx 决定（core 的 Dispatcher 在 invoke 时标好），
// 插件侧据此拿到那个 Bot 的 Host。
func (p *proxy) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	bot, err := p.botOf(ctx)
	if err != nil {
		return nil, err
	}
	r := p.running()
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, p.name)
	}
	reply, err := r.conn.CallHandle(ctx, bot, ev)
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
	bot, err := p.botOf(ctx)
	if err != nil {
		return err
	}
	r := p.running()
	if r == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, p.name)
	}
	err = r.conn.CallRunJob(ctx, bot, spec)
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

// Start 转发 plugin.start，**按 Bot 各起一次**：插件若起后台协程，必须每个
// Bot 一个（不允许用包级变量存"当前 Bot"，ADR-0008 决策 11）。插件没实现时
// 是空操作：核心对每个 proxy 都会调它（proxy 这个类型永远实现了 Starter），
// "实现没实现"由 manifest 的 optional 说了算（pkg/rpc 的 capabilitiesOf 保证
// 探测到的与注册的方法来自同一份断言）。
//
// 进程此刻不在（启动即崩，正在退避）时也返回 nil：这不是"启动失败"，退避
// 重启之后的 startAll 会把每个 Bot 的 Start 补上。
func (p *proxy) Start(ctx context.Context) error {
	bot, err := p.botOf(ctx)
	if err != nil {
		return err
	}
	if !p.optional[rpc.OptionalStart] {
		return nil
	}
	p.mu.Lock()
	p.started[bot.AppID] = true
	p.mu.Unlock()

	r := p.running()
	if r == nil {
		return nil
	}
	return r.conn.CallStart(ctx, bot)
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
	return p.stopRunning(ctx, r)
}

// stopRunning 是三级关闭的全部动作，Close 与热替换共用。
//
// 共用是必须的：热替换停旧进程时，插件同样要靠第一级把内存里的状态写回 KV。
// 少走一级就等于"每次升级丢一次插件内存里的东西"，而这种丢失不会报错。
func (p *proxy) stopRunning(ctx context.Context, r *runtimeProc) error {
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

// replaceInitTimeout 是新产物完成 init 握手的期限（ADR-0006 决策 6：5 秒）。
//
// 5 秒不是拍脑袋：握手只做一次配置下发，正常在 100 ms 内；留到 5 秒是因为
// 冷启动遇上杀软扫描（Windows 首次读一个新 exe）会明显变慢，而"起不来"
// 必须能在一个 owner 等得起的时限内被判出来——超时就回滚，机器人不能因为
// 一次升级卡在一个起不来的插件上。
const replaceInitTimeout = 5 * time.Second

// ReplaceBinary 用 newPath 指向的产物换掉本插件，只重启这一个子进程。
//
// 顺序（每一步都可回退，ADR-0006 决策 2/6）：
//
//	验明正身（`--manifest`，名字必须是本插件）
//	→ 三级关闭旧进程（与 Close 同一套，插件照样收摊）
//	→ rename(bin, .bin.old) → rename(new, bin)
//	→ 起新进程 + init 握手（5 秒）
//
// 成功：删掉备份、补一次 Start（后台协程属于"代"，新进程要重起）、把清单
// 换成新产物的（版本号从此是事实）。
//
// 失败：rename(.bin.old, bin) 还原、重启旧版本、返回可读错误并记一条 Warn
// ——旧版本继续服务，绝不留下"目录里没有插件"的现场。
//
// 与 Close 的唯一区别：这里**不**把 proxy 标成 closing。closing 是终态
// （关停之后不再起进程），而热替换的结尾正好相反。
//
// Windows 上运行中的 exe 不能覆盖、不能删除，但可以改名（镜像以
// FILE_SHARE_DELETE 打开）；先停进程再换，两个平台的顺序因此可以一致，
// 这里不写死 Unix 语义。
func (p *proxy) ReplaceBinary(newPath string) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return fmt.Errorf("pluginproc: 插件 %s 正在关停，不替换二进制", p.name)
	}
	if p.replacing {
		p.mu.Unlock()
		return fmt.Errorf("pluginproc: 插件 %s 已有一个替换在进行中", p.name)
	}
	if len(p.hosts) == 0 {
		p.mu.Unlock()
		return fmt.Errorf("pluginproc: 插件 %s 还没初始化过，不能替换二进制", p.name)
	}
	p.replacing = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.replacing = false
		p.mu.Unlock()
	}()

	err := p.replace(newPath)
	if err != nil {
		// ADR-0006 决策 6 的另一面：回滚成功也要留一条 Warn。否则现场只剩
		// "某个插件怎么还是旧版本"，没有任何线索指向"升级试过、失败了"。
		p.log.Warn("插件热升级失败，已回滚到旧版本", "bin", p.bin, "new", newPath, "err", err)
	}
	return err
}

// replace 是 ReplaceBinary 的主体（状态判定与日志在调用方）。
func (p *proxy) replace(newPath string) error {
	// 1. 先问新产物"你是谁"：不碰进程、不碰文件。换上一个自称别的插件的
	//    产物比不升级坏得多——路由表会指向一个已经不存在的插件。
	m, opts, err := probeOne(context.Background(), newPath, p.probe)
	if err != nil {
		return fmt.Errorf("新产物无法应答 %s: %w", rpcsdk.ManifestFlag, err)
	}
	if m.Name != p.name {
		return fmt.Errorf("新产物自称插件 %q，本插件是 %q：拒绝替换", m.Name, p.name)
	}

	// 2. 同目录才换：跨目录的 rename 在跨卷时会退化成"复制 + 删除"，
	//    而正在跑的这份文件必须原地被替换（同卷是决策 5 的前提）。
	if filepath.Dir(newPath) != filepath.Dir(p.bin) {
		return fmt.Errorf("新产物 %s 与插件 %s 不在同一目录，拒绝替换", newPath, p.bin)
	}

	// 3. 停旧进程。cur 先摘掉：从这一刻起派发会得到"不可用"，而这正是
	//    替换窗口里应有的答案（投给一个正在被拆掉的连接只会得到一堆错误）。
	p.mu.Lock()
	old := p.cur
	p.cur = nil
	p.mu.Unlock()
	if old != nil {
		if err := p.stopRunning(context.Background(), old); err != nil {
			return p.keepServing(fmt.Errorf("停不掉旧进程: %w", err))
		}
	}

	// 4. 换文件：旧映像先改名让位，新产物顶上来（与核心换盘同一套规则）。
	backup := pluginOldPath(p.bin)
	_ = os.Remove(backup)
	if err := renameFile(p.bin, backup); err != nil {
		return p.keepServing(fmt.Errorf("把旧产物改名为 %s: %w", backup, err))
	}
	if err := renameFile(newPath, p.bin); err != nil {
		if rerr := renameFile(backup, p.bin); rerr != nil {
			return fmt.Errorf("替换 %s: %v（把旧产物改回原名也失败: %v）", p.bin, err, rerr)
		}
		return p.keepServing(fmt.Errorf("替换 %s: %w", p.bin, err))
	}

	// 5. 起新进程并让**每个已登记的 Bot** 重新握手，5 秒内完成即为成功。
	//
	//    清单与可选接口先换：它们决定要不要给各 Bot 补 Start，也决定核心看到
	//    的版本。握手失败时连它们一起回滚——否则核心会以为跑的是新版本。
	p.mu.Lock()
	before, beforeOpts := p.manifest, p.optional
	p.manifest = m
	p.optional = make(map[string]bool, len(opts))
	for _, name := range opts {
		p.optional[name] = true
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), replaceInitTimeout)
	err = p.startAll(ctx)
	cancel()
	if err != nil {
		// 文件与进程两样都要回到旧版本，顺序不能反：先换文件再起进程，
		// 否则新进程会去执行一份已经不存在的产物。
		if rerr := renameFile(backup, p.bin); rerr != nil {
			return fmt.Errorf("新版本握手失败（%v），回滚文件也失败: %w", err, rerr)
		}
		p.mu.Lock()
		p.manifest, p.optional = before, beforeOpts
		p.mu.Unlock()
		if rerr := p.startAll(context.Background()); rerr != nil {
			return fmt.Errorf("新版本握手失败（%v），重启旧版本也失败: %w", err, rerr)
		}
		return fmt.Errorf("新版本 %s 内没有完成握手，已回滚并重启旧版本: %w", replaceInitTimeout, err)
	}

	p.mu.Lock()
	p.failures = 0
	// 新产物等于一次干净的起跑：之前那个版本崩出来的"连续失败"不该跟着它。
	p.disabled = false
	p.mu.Unlock()

	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.log.Warn("删除旧版本备份失败", "path", backup, "err", err)
	}
	// 路由表是启动时一次性建好的（ADR-0005 决策 3），热替换不重建它。
	// 命令集变了必须说出来：否则现场是"插件声明了 /x，敲 /x 却没反应"。
	if !sameCommands(before, m) {
		p.log.Warn("新版本的命令集与旧版本不同，路由要等下一次重启才跟上",
			"from", before.Version, "to", m.Version,
			"commands_before", len(before.Commands), "commands_after", len(m.Commands))
	}
	p.log.Info("插件已热升级", "from", before.Version, "to", m.Version, "bin", p.bin)
	return nil
}

// keepServing 用于"还没换成就失败了"的分支：文件已经回到旧版本（或本来
// 就没动），这里保证还有进程在服务，再把原因原样往上抛。
func (p *proxy) keepServing(cause error) error {
	if err := p.startAll(context.Background()); err != nil {
		return fmt.Errorf("%w（旧版本也没能重新拉起: %v）", cause, err)
	}
	return cause
}

// pluginOldPath 是热替换时旧产物的备份路径：与插件同目录、点开头。
//
// 点开头不是风格问题：装配期的目录扫描会把每个非点开头的普通文件都当成
// 插件去探测（pluginproc.Load），一份残留的备份会变成"插件重名"从而在下次
// 启动时刷一条 Error——那条日志看起来像是升级把插件搞坏了。
func pluginOldPath(bin string) string {
	return filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+".old")
}

// renameBackoff 是 rename 失败后的重试节奏（第一次立刻试，之后最多再等
// 约 400 ms）。总预算刻意压在 1 秒以内：热替换的窗口越长，派发被拒的时间
// 越长，而这类"短暂占用"的真实时长一般只有几十毫秒。
var renameBackoff = []time.Duration{
	0, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
}

// renameFile 把 src 改名成 dst，允许 dst 已存在（同卷原子替换），失败重试几次。
//
// 为什么要重试：Windows 上刚退出的进程映像与杀软扫描会在几百毫秒内继续
// 占着目标文件，`MoveFileEx` 这时返回 "Access is denied"（ADR-0006 决策 5
// 明确认下这条代价）。真实测试里它只在"新进程刚崩、马上要改回来"这一瞬间
// 出现——而那正是最不能失败的一步（失败就没有可用的插件了）。
//
// 重试有天花板：一直失败要如实报错，而不是把热替换卡在这里。
func renameFile(src, dst string) error {
	var err error
	for _, wait := range renameBackoff {
		if wait > 0 {
			time.Sleep(wait)
		}
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
	}
	return err
}

// sameCommands 比较两代产物的命令集（顺序也算：路由表的优先级依赖顺序）。
func sameCommands(a, b host.Manifest) bool {
	if len(a.Commands) != len(b.Commands) {
		return false
	}
	for i := range a.Commands {
		if a.Commands[i] != b.Commands[i] {
			return false
		}
	}
	return true
}

// spawn 起一个进程并挂好 host.* 的服务端，但**不**做 init 握手：握手是
// 按 Bot 的，见 startAll。
//
// 失败分两类（见 startFailure 的注释）：进程侧起不来/中途死了属于崩溃，由
// 调用方转成退避重启；其余（含插件在 init 里报的错）原样上抛。
func (p *proxy) spawn(ctx context.Context) (*runtimeProc, error) {
	r, err := startProcess(p.bin, p.reg, p.log)
	if err != nil {
		return nil, &startFailure{err: err, transport: true}
	}
	// host.* 必须在 Run 之前挂上：插件在 Init 里就会调 Host（Config() 不过
	// 通道，但 SendActive / Scheduler 会），那时代码已经在读这条连接了。
	// 解析函数按 bot_id 查表——一个进程服务 N 个 Bot，这里就是那张表的入口。
	if err := rpc.ServeHost(r.conn, p.resolveHost); err != nil {
		r.shutdown()
		return nil, err
	}
	go func() { _ = r.conn.Run() }()
	return r, nil
}

// resolveHost 是 host.* 服务端的 resolver：按 bot_id 找该 Bot 的 Host。
//
// 找不到必须报错而不是回落到"唯一的那一个"：装配缺陷下多一条显式的失败，
// 比把 A 的 KV 写进 B 的库好得多。
func (p *proxy) resolveHost(botID string) (host.Host, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[botID]; ok && h != nil {
		return h, nil
	}
	return nil, fmt.Errorf("插件 %s 没有 Bot %s 的 Host", p.name, botID)
}

// startAll 起一代进程，并把它与**每个已登记的 Bot** 重新接上：先各发一次
// plugin.init，再给调过 Start 的 Bot 补一次 plugin.start。
//
// 为什么重启要重放全部 init：Init 之后进程才可能被投递事件，而新进程对
// 任何一个 Bot 都是一张白纸——只接手"当前那个 Bot"会让别的租户从此收不到
// 事件，且没有任何报错。
func (p *proxy) startAll(ctx context.Context) error {
	r, err := p.spawn(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cur = r
	started := make(map[string]bool, len(p.started))
	for id, ok := range p.started {
		started[id] = ok
	}
	supportsStart := p.optional[rpc.OptionalStart]
	p.mu.Unlock()
	go p.supervise(r)

	for _, bot := range p.registeredBots() {
		if err := r.conn.CallInit(ctx, bot, p.name, p.configOf(bot.AppID)); err != nil {
			r.shutdown()
			p.mu.Lock()
			if p.cur == r {
				p.cur = nil
			}
			p.mu.Unlock()
			// 连接已关＝插件在 init 之前/之中就没了（崩溃）；其余是插件**答了**
			// "我初始化失败"——那是配置写错，按既有口径让启动失败（决策 6）。
			return &startFailure{err: err, transport: errors.Is(err, rpc.ErrConnClosed)}
		}
	}
	if supportsStart {
		for _, bot := range p.registeredBots() {
			if !started[bot.AppID] {
				continue
			}
			if err := r.conn.CallStart(ctx, bot); err != nil {
				// 失败不算起进程失败：插件已经在服务，缺的只是一代 Start——
				// 与崩溃重启后"补 Start 失败"同一条口径。
				p.log.Error("插件重启后 Start 失败", "bot", bot.AppID, "err", err)
			}
		}
	}
	return nil
}

// registeredBots 返回已登记 Host 的 Bot（顺序无关：每个都是独立的握手）。
func (p *proxy) registeredBots() []host.BotInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]host.BotInfo, 0, len(p.hosts))
	for id := range p.hosts {
		out = append(out, host.BotInfo{AppID: id})
	}
	return out
}

// UnregisterBotHost 摘掉某个 Bot 的 Host：这个 Bot 已经不在本进程里了
// （热删、或该 Bot 的 Core 已关停），它的 host.* 调用不该再被服务——留着
// 表项等于让一条迟到的调用写进一个正在关闭的库。
//
// 它是 internal/instance 的容器在 Bot 退出时调的入口（ADR-0008 决策 15 的
// 热增删也走这里）。进程级的 Close 不受影响：插件进程仍服务其余 Bot。
func (p *proxy) UnregisterBotHost(botID string) {
	p.mu.Lock()
	delete(p.hosts, botID)
	delete(p.cfgs, botID)
	delete(p.started, botID)
	p.mu.Unlock()
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
	replacing := p.replacing
	if current {
		p.cur = nil
	}
	p.mu.Unlock()

	if closing || replacing {
		// 关停路径自己在走三级关闭，热替换也一样：这里的"连接一断就杀"
		// 会跳过第一级（插件连收尾的机会都没有），替换路径也就没法按
		// "先收摊再换文件"的顺序做了。
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
	// replacing 时也要让开：替换路径自己会起进程，这里再起一个就是双开。
	closing, disabled, replacing := p.closing, p.disabled, p.replacing
	p.mu.Unlock()
	if closing || disabled || replacing {
		return
	}
	if err := p.startAll(context.Background()); err != nil {
		p.log.Error("插件重启失败", "err", err)
		p.noteExit(err)
		return
	}
	// startAll 已经把每个 Bot 的 init（与该补的 Start）都重放过了：新进程对
	// 每个租户都要重新接上，只接一个会让其余租户从此静默收不到事件。
	p.log.Info("插件已重启", "failures", p.failureCount(), "bots", len(p.registeredBots()))
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
