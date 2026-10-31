package instance

// Container 是「一个 Core 托管多个 Bot」的装配层：它按 [[bots]] 的顺序逐个
// 取锁、开库、起连接，关停时按固定顺序放掉（见包文档）。
//
// 锁从"一个进程一份"变成"每个 Bot 各取一份"：appID 维度约束"这台机器上的这个
// Bot"，数据目录维度约束"这一份 SQLite"。ADR-0008 决策 1 把数据边界定在库上，
// 所以两个维度都按 Bot 展开，一个 Bot 被另一个 Core 占着时只拒它一个。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

const (
	// gatewayStopTimeout 是等某个 Bot 的网关读循环退出的上限。
	//
	// 它退出前会把 session_id / seq 落盘（connectOnce 的 defer），所以这个等待
	// 就是"等状态落盘完成"——必须早于该 Bot 的 db.Close，否则下次只能冷
	// Identify，白烧一次 session 配额。
	gatewayStopTimeout = 5 * time.Second
	// assemblyCleanupTimeout 是装配失败时放掉已占资源的期限。它不跟调用方的
	// ctx 走：关停信号恰好落在装配中途时，锁与库仍然必须被放掉。
	assemblyCleanupTimeout = 5 * time.Second
)

// Options 是启动一个 Core 的全部输入。
//
// 进程级共享的东西（配置、日志、渲染器、出站客户端、插件加载器）在这里，
// 每 Bot 的差异全部来自 Config.Bots：容器不额外接受一份"Bot 列表"参数，
// 否则两处列表就会各说各话。
type Options struct {
	Config *config.Config
	Log    *slog.Logger
	// Renderer 是插件可见的渲染能力，进程内共享一份（渲染并发上限也是全局的，
	// 见 ADR-0002）。由调用方在启动期同步解析字体后注入：那是进程级的一次性
	// 工作，容器不该自己重做 N 遍。
	Renderer host.Renderer
	// Outbound 是**根**出站客户端：容器按 Bot 从它派生各自的一份（见
	// httpx.Client.Partition）。必须由调用方传——它同时也是启动期那行
	// "域名独立额度已启用"的日志源，容器再造一份就会出现两个根限流器。
	//
	// 根客户端自己只服务"派生"：它不承载任何 Bot 的请求，所以它的桶
	// 永远不会被冷却，也就不存在跨租户污染。
	Outbound *httpx.Client
	// Handshake 只用于 WSS 握手（测试注入信任自签证书的客户端，生产留 nil
	// 走默认）。它与 Outbound 是两个东西：前者是"连得上"，后者是"发得出去"。
	Handshake *http.Client
	// LoadPlugins 装配**进程级**的插件集：一套插件进程服务所有 Bot
	// （ADR-0008 决策 11）。nil 表示这个 Core 不加载插件（会打一条 warn，
	// 不是静默）。
	//
	// 这是个回调而不是容器自己去调 pluginproc.Load：插件目录、停用名单、
	// 错误码表都是装配点（cmd/bot）的事实，容器只负责"进程级加载一次、
	// 所有 Bot 共享这一组句柄"。
	LoadPlugins func(ctx context.Context, log *slog.Logger) ([]host.Plugin, error)
	// UpgradeFor 返回某个 Bot 的升级能力；nil 表示不装配自更新。
	UpgradeFor func(bot config.Bot, db *store.DB, plugins []host.Plugin) core.UpgradeService
	// LockWait 是每个 Bot 等单实例锁的上限：/reload 交接给 15s，普通启动 0。
	LockWait time.Duration
	// ReconnectBackoff 覆盖 failing 态的重连退避序列，末项即上限。nil 用默认的
	// 1/2/4/8/16 分钟；测试注入毫秒级，否则一个用例要跑十几分钟。
	ReconnectBackoff []time.Duration
}

// Container 是进程级容器：持有共享资源，按配置顺序逐个装配 Bot。
//
// 它不重启、不重读配置：/reload 仍是整个进程 exec（ADR-0008 决策 15），
// 单 Bot 热增删改是 #68。
type Container struct {
	cfg *config.Config
	log *slog.Logger
	o   Options

	outbound *httpx.Client // 根客户端，只供 Partition 派生（见 Options.Outbound）
	stagger  *Stagger
	// jobs 是多 Bot 时的共享任务桶额度池；nil 表示每个 Core 自建一份容量 4
	// 的池（单 Bot 与"只有一个启用的 Bot"时都是这个形态，行为与旧版一致）。
	jobs   *core.JobPool
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// plugins 是**进程级共享**的插件句柄：启动时加载一次，所有 Bot 共用。
	// 它们的生命周期不归任何单个 Bot——由 Shutdown 在最后统一关。
	plugins []host.Plugin
	// bots 按配置顺序记录每个"启用"的 Bot 的装配结果（失败的也在里面）。
	bots []*botRuntime
	// live 是还没退出的网关数；starting 表示启动循环还没走完。
	//
	// 两者只服务一件事：**全部**网关都因永久性错误退出时让进程退出，交给人看
	// （与单 Bot 时代"不让进程活着、机器人已死"一致）。只剩一个坏租户时进程
	// 必须继续跑，#63 会把它变成 failing 态。
	live     int
	starting bool
	fatal    chan error

	restarts chan core.RestartRequest
}

// Status 是一个 Bot 的装配结果，供启动日志与测试断言。
type Status struct {
	AppID   string
	DataDir string
	DBPath  string
	// LockPath / AppLockPath 是两把锁落在哪，排障时照着 `ls` 就行。
	LockPath    string
	AppLockPath string
	Plugins     int
	// Err 非 nil 表示这个 Bot 没起来；其余 Bot 照常运行。
	Err error
}

// Start 按配置里的顺序启动所有启用的 Bot，返回进程级容器。
//
// 只有"整机起不来"才返回 error：没有配置、一个启用的 Bot 都没有。单个 Bot
// 的失败（锁被占、凭据写错、数据目录没权限）记在 Status.Err 里，进程继续跑
// 其余的——ADR-0008 的用户故事 5 就是这条。
func Start(ctx context.Context, o Options) (*Container, error) {
	if o.Config == nil {
		return nil, errors.New("instance: 缺少配置")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	cfg := o.Config
	enabled := 0
	for _, b := range cfg.Bots {
		if b.IsEnabled() {
			enabled++
		}
	}
	if enabled == 0 {
		return nil, errors.New("没有启用的 Bot：所有 [[bots]] 都是 enabled = false，进程无事可做")
	}
	if o.Outbound == nil {
		return nil, errors.New("instance: 缺少出站客户端（Options.Outbound）")
	}
	if o.LoadPlugins == nil {
		log.Warn("没有装配插件加载器：所有 Bot 都不会加载任何插件")
	}

	runCtx, cancel := context.WithCancel(ctx)
	c := &Container{
		cfg:      cfg,
		log:      log,
		o:        o,
		outbound: o.Outbound,
		stagger:  NewStagger(cfg.Bot.GatewayStaggerInterval()),
		ctx:      runCtx,
		cancel:   cancel,
		starting: true,
		fatal:    make(chan error, 1),
		restarts: make(chan core.RestartRequest, 1),
	}

	warnLegacyStore(log, cfg)
	log.Info("开始启动 Bot",
		"bots", len(cfg.Bots), "enabled", enabled,
		"stagger", c.stagger.Interval().String())

	// 插件只加载一次：一套进程服务所有 Bot（ADR-0008 决策 11）。加载失败是
	// 整机级失败——没有插件时"启动了但什么都不响应"比报错难查得多。
	shared, err := c.loadPlugins(runCtx, log)
	if err != nil {
		cancel()
		return nil, err
	}
	c.plugins = shared
	logPluginSet(shared, log)

	// 多 Bot 时任务桶共享一份池：调度桶并行 4 是按进程内存实测调的，每个
	// Core 各持一份等于 4×N，且一个租户能占满自己那 4 格（ADR-0008 决策 13）。
	// 只有一个启用的 Bot 时不注入：那时它就是唯一租户，凭空把并行砍到 1
	// 只是让慢任务更慢。
	if enabled > 1 {
		c.jobs = core.NewJobPool()
	}

	startedAt := time.Now()
	started := 0
	for _, b := range cfg.Bots {
		if !b.IsEnabled() {
			log.Info("Bot 已停用，不连接", "bot", b.AppID)
			// 停用也要落一行状态：库里留着的 failing/active 会让第二天排障的人
			// 误判成"它自己挂的"，而实际上是人工关的。
			c.markSuspended(runCtx, b)
			continue
		}
		// 关停期间不产生新的 Bot 启动：信号可能正好落在两个 Bot 之间。
		if runCtx.Err() != nil {
			log.Info("收到关停信号，停止启动剩余的 Bot", "pending", enabled-started)
			break
		}
		rt := c.startBot(runCtx, b)
		c.bots = append(c.bots, rt)
		if rt.err != nil {
			log.Error("Bot 未启动，其余 Bot 继续",
				"bot", b.AppID,
				"db", cfg.BotStorePath(b),
				"err", rt.err)
			continue
		}
		started++
		logBotStarted(rt)
		go c.pumpRestarts(rt.coreImpl, b.AppID)
		// 等这个 Bot 的第一次取网关地址落定再装下一个：错峰的意义就是
		// 让 N 条连接一条一条来（GET /gateway 限 2 QPM）。闸门本身保证
		// 串行与间隔，这里只是把代价算进启动时长并让日志顺序可读。
		//
		// gwDone 那一条是防挂死的兜底：网关若在"取地址"之前就退出（例如
		// Handler 没接上），gateDone 永远不会关，启动会一直等下去。
		select {
		case <-rt.gateDone:
		case <-rt.gwDone:
		case <-runCtx.Done():
		}
	}

	c.mu.Lock()
	c.starting = false
	live := c.live
	c.mu.Unlock()
	// 启动阶段结束：取地址不再排队。之后的重连各自发各自的请求——把错峰套到
	// 运行期，一次掉线就要多等一个间隔。
	c.stagger.Disarm()
	if started > 0 && live == 0 && runCtx.Err() == nil {
		// 所有网关在启动期就都退了（永久性错误）：留一个空壳没有意义。
		//
		// 非阻塞发送：从上面放开 starting 到这一刻之间，最后一个网关的
		// noteGatewayExit 可能已经抢先塞满了那一格——直接写会永远堵在这里。
		select {
		case c.fatal <- errors.New("全部 Bot 的网关均已退出"):
		default:
		}
	}

	// 一套插件进程服务所有 Bot：进程数 = 插件数，不再随租户数线性增长
	// （ADR-0008 决策 11 要消灭的就是那个乘法）。这一行是"到底起了几个
	// 插件进程"的唯一权威来源。
	//
	// 摘要排在完成行之前：它是"起了什么"的收口，而最后一行留给"起完了"这个
	// 事实——两行都在同一个函数里，天然只打一次。
	c.logBotStateSummary(runCtx)
	log.Info("Bot 启动完成",
		"bots", len(cfg.Bots), "enabled", enabled,
		"started", started, "failed", enabled-started,
		"plugins_total", len(c.plugins),
		"elapsed", time.Since(startedAt).Round(time.Millisecond).String())
	return c, nil
}

// botStateTally 是按状态汇总的一组计数。
//
// 四个状态与 core.BotState 一一对应，另外两个数不落在状态里：Total 是配置里
// 登记的 Bot 总数（含停用的），NotStarted 是其中"根本没起来"的个数
// （它们没有可读的库，计数记进 Failing，见 tallyBotStates）。
type botStateTally struct {
	Total        int
	Active       int
	PendingClaim int
	Failing      int
	Suspended    int
	NotStarted   int
}

// String 生成启动摘要的正文（验收文案，改字要同步改 #69 的验收）。
func (t botStateTally) String() string {
	return fmt.Sprintf("共 %d 个 Bot：%d 个 active、%d 个 pending_claim、%d 个 failing、%d 个 suspended",
		t.Total, t.Active, t.PendingClaim, t.Failing, t.Suspended)
}

// tallyBotStates 汇总所有已登记 Bot 的状态。
//
// 取值口径与 #63 完全一致：库里有 bot_state 就用它，没有行时按 admins 是否
// 为空推导——唯一读法是 core.LoadBotState，这里不另立一套（两套推导迟早会
// 在"启动摘要说 pending_claim、闸门却按 active 放行"上分叉）。
//
// 三类 Bot 各有各的读法：
//   - 起成功的：读它自己的库；
//   - 配置里停用的：不连接，库里的状态在启动时已由 markSuspended 落成
//     suspended，这里用配置事实（不为读一行再开一次库）；
//   - 没起来的（锁被占、开库失败…）：没有可读的库，记进 Failing 并单列
//     NotStarted——"配置里有、实际没跑"是必须能被看见的事实，混进 active
//     才是真正的误导。
func tallyBotStates(ctx context.Context, log *slog.Logger, bots []config.Bot, runtimes map[string]*botRuntime) botStateTally {
	t := botStateTally{Total: len(bots)}
	for _, b := range bots {
		if !b.IsEnabled() {
			t.Suspended++
			continue
		}
		rt := runtimes[b.AppID]
		if rt == nil || rt.db == nil {
			t.Failing++
			t.NotStarted++
			continue
		}
		st, err := core.LoadBotState(ctx, rt.db)
		if err != nil {
			log.Warn("读取 Bot 状态失败，按 failing 计入启动摘要", "bot", b.AppID, "err", err)
			t.Failing++
			continue
		}
		switch st {
		case core.BotStateActive:
			t.Active++
		case core.BotStatePendingClaim:
			t.PendingClaim++
		case core.BotStateFailing:
			t.Failing++
		default:
			t.Suspended++
		}
	}
	return t
}

// logBotStateSummary 打启动摘要：一行回答"这台机器上现在有几个 Bot、各是什么
// 状态"。正文给人扫，attrs 给 grep 与看板——两者同源，不会各说各话。
func (c *Container) logBotStateSummary(ctx context.Context) {
	runtimes := make(map[string]*botRuntime, len(c.bots))
	for _, rt := range c.bots {
		runtimes[rt.cfg.AppID] = rt
	}
	t := tallyBotStates(ctx, c.log, c.cfg.Bots, runtimes)
	c.log.Info(t.String(),
		"bots", t.Total,
		"active", t.Active,
		"pending_claim", t.PendingClaim,
		"failing", t.Failing,
		"suspended", t.Suspended,
		"not_started", t.NotStarted)
}

// loadPlugins 装配进程级的插件集。没有加载器时返回空集：这条分支已在
// Start 里 warn 过，不在每个 Bot 上重复刷屏。
func (c *Container) loadPlugins(ctx context.Context, log *slog.Logger) ([]host.Plugin, error) {
	if c.o.LoadPlugins == nil {
		return nil, nil
	}
	plugins, err := c.o.LoadPlugins(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("装配插件: %w", err)
	}
	return plugins, nil
}

// logPluginSet 打出这次进程级装配的插件清单。
//
// 插件名必须可见：只报数量时"新插件没打进产物"与"插件进去了但没注册"在日志里
// 完全一样（P3-17）。共享之后这份清单不再按 Bot 重复，所以只打一次。
func logPluginSet(plugins []host.Plugin, log *slog.Logger) {
	if len(plugins) == 0 {
		return
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Manifest().Name)
	}
	sort.Strings(names)
	log.Info("进程级插件已装配", "plugins", len(plugins), "plugin_names", strings.Join(names, ","))
}

// logBotStarted 打出这个 Bot 实际用的库与两把锁。
//
// 库路径必须可见：它由 appid 推导，备份、迁移、清理都靠这一行确认东西落在哪。
// 插件不在这一行：它是进程级共享的，由 logPluginSet 统一打一次。
//
// 这里不再打 appid：rt.log 已经预置 `bot=<appid>`（见 startBot），同一行里
// 再拼一个 appid 只是把同一个值写两遍，还会让"按 bot 检索"的 grep 出现两种键名。
func logBotStarted(rt *botRuntime) {
	rt.log.Info("Bot 已启动",
		"db", rt.db.Path(),
		"lock", rt.lock.Path(),
		"app_lock", rt.lock.AppPath(),
		"plugins", len(rt.plugins))
}

// Stop 断开所有 Bot 的接入层。ctx 取消也会走到同一个结果（它们同源），
// 这里显式提供是为了关停顺序在调用点读得出来：Stop → 等网关落盘 → 关核心。
func (c *Container) Stop() { c.cancel() }

// Shutdown 关停全部 Bot：并行等每条网关落盘 → 并行核心收尾（调度、派发、
// GC）→ 各自注销 Host、关库、放锁 → **最后统一关插件**。
//
// 顺序不能反：插件在写库，先关库会得到一串 database is closed；而插件是共享
// 的，任何一个 Bot 关它都会带走别人正在用的进程，所以它排在所有 Bot 之后
// （ADR-0008 决策 11）。
//
// 两段都并行：各 Bot 彼此独立（各自的库），串行只会让最后一个 Bot 分到的
// 期限变少。ctx 是整体预算，超时或未按时落盘的 Bot 会被记下来，调用方据此
// 以非 0 退出（与单 Bot 时代一致）。
func (c *Container) Shutdown(ctx context.Context) error {
	c.Stop()

	errs := c.waitGateways(ctx)

	var wg sync.WaitGroup
	results := make([]error, len(c.bots))
	for i, b := range c.bots {
		wg.Add(1)
		go func(i int, rt *botRuntime) {
			defer wg.Done()
			results[i] = rt.close(ctx)
		}(i, b)
	}
	wg.Wait()
	for _, err := range results {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// 所有 Core 都收完了才轮到插件：它们到此为止不会再收到任何调用。
	core.ClosePlugins(ctx, c.log, c.plugins)
	c.plugins = nil
	return errors.Join(errs...)
}

// waitGateways 并行等每条网关读循环退出（含 session/seq 落盘）。
//
// 必须并行：串行 N×gatewayStopTimeout 会吃光调用方那 10s 的关停预算，
// 三个 Bot 卡住就让后面几个变成假告警——而它们其实是同时收到取消的，
// 串行等待多等不到任何东西。ctx 是整体预算，用尽后剩下的不再等。
func (c *Container) waitGateways(ctx context.Context) []error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, b := range c.bots {
		if b.gw == nil {
			continue
		}
		wg.Add(1)
		go func(rt *botRuntime) {
			defer wg.Done()
			if rt.waitGateway(ctx) {
				return
			}
			mu.Lock()
			errs = append(errs, fmt.Errorf("Bot %s 的网关未按时退出", rt.cfg.AppID))
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	return errs
}

// RestartRequests 汇总所有 Bot 的重启请求（/reload、/upgrade apply）。
//
// 通道只有一格：同一时刻只允许一条待处理请求，重复请求直接丢掉——否则
// 两个 Bot 同时 /reload 会让进程被推着反复 exec。
func (c *Container) RestartRequests() <-chan core.RestartRequest { return c.restarts }

// Fatal 在**全部**已启动的网关都因永久性错误退出时收到一条错误。
//
// 单个 Bot 被封只记日志（见 botRuntime.runGateway），failing 态是 #63；
// 但整机一个能用的连接都不剩时继续跑没有意义，交给 systemd 的
// Restart=on-failure 与 StartLimit 收敛成一次人工介入。
func (c *Container) Fatal() <-chan error { return c.fatal }

// Statuses 返回每个已启用 Bot 的装配结果（顺序与配置一致，含失败的）。
func (c *Container) Statuses() []Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Status, 0, len(c.bots))
	for _, b := range c.bots {
		out = append(out, b.status(c.cfg))
	}
	return out
}

// markSuspended 把配置里停用的 Bot 标成 suspended。
//
// 只开库写一行就关：不取锁、不装插件、不起核心。状态必须落库——这是"这个
// Bot 为什么没连"的唯一持久答案，重启之后仍然成立，而日志会被轮转掉。
func (c *Container) markSuspended(ctx context.Context, b config.Bot) {
	db, err := store.Open(c.cfg.BotStorePath(b))
	if err != nil {
		c.log.Error("停用 Bot 的状态未落库（打开库失败）", "bot", b.AppID, "err", err)
		return
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		c.log.Error("停用 Bot 的状态未落库（迁移失败）", "bot", b.AppID, "err", err)
		return
	}
	st, err := core.ReconcileBotState(ctx, db, false)
	if err != nil {
		c.log.Error("停用 Bot 的状态未落库", "bot", b.AppID, "err", err)
		return
	}
	c.log.Info("Bot 状态标记为 suspended", "bot", b.AppID, "state", string(st))
}

// noteGatewayStart 记一条活着的网关。启动循环用它判断"启动期就全退了"。
func (c *Container) noteGatewayStart() {
	c.mu.Lock()
	c.live++
	c.mu.Unlock()
}

// noteGatewayExit 维护"还有几个 Bot 的网关活着"。runGateway 在退出后调用。
func (c *Container) noteGatewayExit(err error) {
	if err == nil {
		// 只有 ctx 取消才会返回 nil：关停路径，不参与"全部失联"的判定。
		return
	}
	c.mu.Lock()
	c.live--
	live, starting := c.live, c.starting
	c.mu.Unlock()
	// 启动期不作判定：这时 live 还没数完，早退一个不等于全灭。
	if starting || live > 0 {
		return
	}
	select {
	case c.fatal <- err:
	default:
	}
}

// pumpRestarts 把某个 Bot 的重启请求转发到容器级通道，并补上 Bot 标识：
// 日志里"谁要重启"是多 Bot 之后第一个要回答的问题。
func (c *Container) pumpRestarts(cc *core.Core, appid string) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case req := <-cc.RestartRequests():
			select {
			case c.restarts <- req:
				c.log.Info("收到重启请求",
					"bot", appid, "by", req.By, "role", req.Role, "reason", req.Reason)
			default:
				c.log.Warn("已有待处理的重启请求，忽略重复请求", "bot", appid, "by", req.By)
			}
		}
	}
}

// warnLegacyStore 提示单 Bot 时代那份库不会被读取。
//
// 一 Bot 一库之后 [store].path 只提供目录，库在 <datadir>/bots/<appid>/qqbot.db。
// 老部署的库还在原地、数据也还在，但进程不会再看它——不说这一句，运维会以为
// 群名单与管理员"丢了"。这里不自动迁移：搬库要处理 WAL 与备份，属于人工决定。
func warnLegacyStore(log *slog.Logger, cfg *config.Config) {
	legacy := filepath.Clean(cfg.Store.Path)
	for _, b := range cfg.Bots {
		// data_dir 被显式指到 datadir 时，两者恰好是同一个文件：不是遗留形态。
		if samePath(cfg.BotStorePath(b), legacy) {
			return
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	log.Warn("检测到单 Bot 时代的数据库，本版本按 Bot 分库后不再读取它",
		"legacy_db", legacy,
		"hint", "每个 Bot 的库在 <datadir>/bots/<appid>/qqbot.db；确认数据后手工迁移或删除旧文件")
}

// samePath 按绝对路径比较，避免 "./data/qqbot.db" 与 "data/qqbot.db" 被当成两份。
func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return absA == absB
}
