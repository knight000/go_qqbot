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

	mu   sync.Mutex
	host host.Host
	cfg  map[string]any
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
		probe:     o.probeTimeout(),
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
	if p.host == nil {
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

	// 5. 起新进程，5 秒内完成 init 握手即为成功。
	ctx, cancel := context.WithTimeout(context.Background(), replaceInitTimeout)
	r, err := p.spawn(ctx)
	cancel()
	if err != nil {
		// 文件与进程两样都要回到旧版本，顺序不能反：先换文件再起进程，
		// 否则新进程会去执行一份已经不存在的产物。
		if rerr := renameFile(backup, p.bin); rerr != nil {
			return fmt.Errorf("新版本握手失败（%v），回滚文件也失败: %w", err, rerr)
		}
		if rerr := p.start(context.Background()); rerr != nil {
			return fmt.Errorf("新版本握手失败（%v），重启旧版本也失败: %w", err, rerr)
		}
		return fmt.Errorf("新版本 %s 内没有完成握手，已回滚并重启旧版本: %w", replaceInitTimeout, err)
	}

	p.mu.Lock()
	p.cur = r
	p.failures = 0
	// 新产物等于一次干净的起跑：之前那个版本崩出来的"连续失败"不该跟着它。
	p.disabled = false
	before := p.manifest
	p.manifest = m
	// 可选接口也以新产物为准：它决定 Start/Close 会不会真的过通道，
	// 拿旧产物的能力表去调新进程只会得到"方法未注册"。
	p.optional = make(map[string]bool, len(opts))
	for _, name := range opts {
		p.optional[name] = true
	}
	needStart := p.started && p.optional[rpc.OptionalStart]
	p.mu.Unlock()
	go p.supervise(r)

	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.log.Warn("删除旧版本备份失败", "path", backup, "err", err)
	}
	if needStart {
		// 核心只在启动时调一次 Start；新进程要自己把后台协程起起来。
		// 失败不算替换失败：插件已经在服务，缺的只是一代 Start——与崩溃
		// 重启后"补 Start 失败"同一条口径。
		if err := p.Start(context.Background()); err != nil {
			p.log.Error("插件升级后 Start 失败", "err", err)
		}
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
	if err := p.start(context.Background()); err != nil {
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
