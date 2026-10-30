// Command bot 是唯一装配点：加载配置 → 准备进程级共享资源（日志、字体、
// 出站客户端、插件产物）→ 交给 internal/instance 按 [[bots]] 逐个开库、起连接。
//
// 一 Bot 一库（ADR-0008）：每个 Bot 的库在 <datadir>/bots/<appid>/qqbot.db，
// 由 instance 打开并在关停时逐个关闭。main 不碰任何单 Bot 的资源。
//
// 插件不在这个二进制里（ADR-0005 决策 10）：它们是 <datadir>/plugins/bin/
// 下的独立可执行文件，由 internal/pluginproc 在装配期扫描、探测、spawn。
// 所以加插件不需要重编译核心，`go build ./cmd/bot` 也不再需要任何 go:generate。
//
// 出厂副本（四个示例插件）由 internal/pluginbundle 内嵌。启动期只做对齐
// （#52）：内嵌版本更高、或契约版本不匹配才替换，磁盘版本更高则保留；
// 缺文件时补回。释放全部缺失文件仍走显式命令 `qqbot init --plugins`（#42）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	// 内嵌 IANA 时区库：Windows 单文件与精简容器（scratch/alpine）没有 zoneinfo，
	// 不嵌的话 time.LoadLocation("Asia/Shanghai") 会失败并回退 UTC，cron 时间整体偏移 8 小时。
	_ "time/tzdata"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/instance"
	"github.com/knight000/go_qqbot/internal/logx"
	"github.com/knight000/go_qqbot/internal/pluginalign"
	"github.com/knight000/go_qqbot/internal/pluginbundle"
	"github.com/knight000/go_qqbot/internal/pluginproc"
	"github.com/knight000/go_qqbot/internal/render"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// errShutdown 表示进程本身没有故障，只是关停没能在期限内完成。
//
// 与"启动失败"分开只为日志能说清楚：退出码都是非 0，systemd 的
// Restart=on-failure 都应该看见（P3-23）。
var errShutdown = errors.New("关停未在期限内完成")

// shutdownTimeout 覆盖全部 Bot 的关停预算：等每条网关落盘、调度收尾、派发
// 排空、GC 退出与插件关闭。每个 Bot 的网关等待另有一份 5s 的内部上限。
const shutdownTimeout = 10 * time.Second

func main() {
	// 子命令在 flag.Parse 之前分派：`qqbot init -c x` 的 -c 属于 init，
	// 不能落到启动用的 flag 集合里。不带子命令时完全是原来的启动行为。
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(runInit(os.Args[2:], os.Stdout, os.Stderr))
	}

	req, err := run()
	if err != nil {
		switch {
		case errors.Is(err, errShutdown):
			fmt.Fprintln(os.Stderr, "关停失败:", err)
		default:
			fmt.Fprintln(os.Stderr, "启动失败:", err)
		}
		os.Exit(1)
	}
	if req == nil {
		return
	}

	// 走到这里时 run 已经返回：网关断开、session 落盘、数据库关闭。
	// 此时再做进程替换，新进程不会和旧进程抢同一个 gateway session 与 SQLite 连接。
	//
	// 受 devrun 托管时不换映像：退出码就是协议，父进程负责重新产出插件产物、
	// 编译核心、替换二进制再拉起。生产环境没有源码与工具链，不会走到这里。
	if buildinfo.Supervised() {
		os.Exit(buildinfo.ExitRestartRequired)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "重启失败：定位可执行文件:", err)
		os.Exit(1)
	}

	// /upgrade apply 的换盘就发生在这里：网关已断、session 已落盘、库已关。
	// 失败时 Commit 会把旧二进制换回来（进程内回滚），我们照样重启——
	// 重启旧版本总好过让机器人停在这里等人工介入。
	if req.Plan != nil {
		if err := upgrade.Commit(req.Plan, slog.Default()); err != nil {
			fmt.Fprintln(os.Stderr, "换盘失败，继续重启旧版本:", err)
		}
	}
	if err := restartSelf(exe); err != nil {
		fmt.Fprintln(os.Stderr, "重启失败:", err)
		os.Exit(1)
	}
}

// run 正常退出时返回 nil；返回非 nil 表示需要由 main 重新执行自身（/reload）。
func run() (*core.RestartRequest, error) {
	cfgPath := flag.String("c", defaultConfigPath, "配置文件路径")
	debugMode := flag.Bool("debug", false, "调试模式：打印收到的事件、命中的插件与回复内容（等同 [debug].enabled=true）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, err
	}
	// 命令行参数优先于配置文件：临时排查不必改文件，也不会漏提交。
	if *debugMode {
		cfg.Debug.Enabled = true
	}

	// 调试模式顺带把日志降到 debug：两边看到的细节应当一致。
	logLevel := cfg.Log.Level
	if cfg.Debug.Enabled {
		logLevel = "debug"
	}
	log := logx.New(logLevel, cfg.Log.Format)
	slog.SetDefault(log)

	// 配置层的"能用但不该这样写"必须在这里落地：多 Bot 环境下环境变量只覆盖
	// 第一个 Bot，用户如果看不到这条 warn，会以为所有 Bot 都换了凭据。
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	// 逐个打印 Bot 的生效配置：数据目录与库路径由 appid 推导，备份、迁移与
	// 排障都靠这几行确认东西落在哪（secret 永不出现）。
	logBots(log, cfg)

	// 信号注册必须在这里——配置刚读完，后面还要解析字体（约 300ms）、拿单实例锁、
	// 开库、跑迁移。放在它们之后，这段窗口里收到的 Ctrl-C/SIGTERM 会走 Go 的
	// 默认处理：进程被直接杀死，会话不落盘，systemd 记的是 "killed by signal"
	// 而不是一次正常退出（P3-21）。
	ctx, stop := notifyShutdown(context.Background())
	defer stop()

	// datadir 是全仓唯一口径：数据目录就是数据库文件所在的那一层。
	// 升级标记与快照都在它下面（<datadir>/upgrade/），因此这里要早于开库。
	datadir := filepath.Dir(cfg.Store.Path)

	// 自更新的回滚判定必须在这里——拿锁之前、开库之前：
	//   - 回滚的最后一步是重新执行自身，必须先放手（旧进程一个锁都不该握着）；
	//   - 判定要读 <datadir>/upgrade/state.json，只需要配置。
	// 顺序错了会得到一个"回滚后永远等锁"的启动循环。
	exePath, exeErr := os.Executable()
	if exeErr != nil {
		exePath = ""
	}
	if rolledBack, err := handlePendingUpgrade(upgrade.StateDir(datadir), exePath, log); err != nil {
		// 回滚失败不阻止启动：用当前二进制继续跑，比让机器人直接下线好。
		log.Error("升级回滚未能完成，继续用当前二进制启动", "err", err)
	} else if rolledBack {
		// Unix 上 exec 成功不返回；Windows 上这是"拉起子进程"，
		// 本进程随后退出（见 restartSelf 的注释）。
		if err := restartSelf(exePath); err != nil {
			return nil, fmt.Errorf("回滚后重启失败: %w", err)
		}
		return nil, nil
	}
	// 上一次升级留下的 .old 到这里就没用了（已经稳定运行过，
	// 或刚被上面回滚用掉）。删不掉只 warn：Windows 上杀软/旧进程
	// 可能短暂持有它，一份 40 MB 的残留不值得让机器人下线。
	// exePath 为空（定位失败）时不能调：那会变成删当前工作目录下的 ".old"。
	if exePath != "" {
		if err := upgrade.CleanOld(exePath); err != nil {
			log.Warn("清理旧版本残留失败", "path", upgrade.OldPath(exePath), "err", err)
		}
	}

	// 渲染器在启动期同步解析字体（约 300 ms，全部花在 20 MB 字体的字节码扫描上）。
	// 字体是 go:embed 进来的，解析失败只可能是二进制本身有问题——带病运行会在
	// 第一次出图时把错误抛给插件，不如现在直接启动失败。
	//
	// 容器把它原样分给每个 Bot：渲染并发上限是全局的（ADR-0002），每个 Bot
	// 各解析一遍 20 MB 字体既慢又白占内存。
	fonts, err := render.LoadFonts()
	if err != nil {
		return nil, err
	}
	renderer := render.New(fonts, log.With("component", "render"))

	// 出站客户端进程级共享一份：全局限流与域名桶都挂在这一个实例上，
	// 每个 Bot 各建一个等于把"每分钟 100 次"放大成 N 倍。
	hc := httpx.New(httpx.Options{
		Timeout:          cfg.HTTP.Timeout.Duration,
		Retries:          cfg.HTTP.Retries,
		RatePerMin:       cfg.HTTP.RatePerMin,
		DomainRatePerMin: cfg.HTTP.Domains,
		Log:              log.With("component", "httpx"),
	})
	// 域名独立额度放宽了单域名的出站速率，配错会打爆上游——启动时把实际
	// 生效的清单打出来，是上线核对配置的唯一机会（非法值已在 New 里告警剔除）。
	if limits := hc.DomainLimits(); len(limits) > 0 {
		log.Info("出站限流：域名独立额度已启用",
			"default_per_min", cfg.HTTP.RatePerMin, "domains", formatDomainLimits(limits))
	}

	// 插件是 plugins/bin/ 下的独立可执行文件，核心只看见 []host.Plugin
	// （ADR-0005 决策 3）。装配期全部工作收在 pluginproc.Load：扫目录 →
	// 并行跑 `bin --manifest`（2 秒）→ 校验 → 跳过 [plugins].disabled。
	//
	// 探测或校验失败的插件只记 Error、**不让启动失败**：一个过期产物不该让
	// 机器人整体下线，那还会进 systemd 重启循环白烧 Identify 配额（决策 6）。
	registry := rpc.NewRegistry()
	// core.ErrPassiveExhausted 必须过通道：插件调 Host.SendActive 时要能认出
	// "被动窗口用尽"并发自己的降级消息，而不是当成普通错误重试。它故意留在
	// core（没有第二个真实消费方），所以号码只能由实现方注册（决策 9）。
	if err := registry.Register(rpc.CodeImplBase, core.ErrPassiveExhausted); err != nil {
		return nil, err
	}
	// 内嵌副本对齐必须在 Load 之前（#52）：这时插件进程还没起，文件可以
	// 安全替换。判定矩阵在 internal/pluginalign：磁盘插件被判拒载就无条件换，
	// 判为兼容时按插件版本只升不降（内嵌更高才换）。
	//
	// 它是进程级的一次性动作：插件产物目录对所有 Bot 是同一份。
	pluginDir := filepath.Join(datadir, "plugins", "bin")
	align := pluginalign.Align(ctx, pluginalign.Options{
		Dir:      pluginDir,
		Bundle:   pluginbundle.Files(),
		Versions: pluginbundle.Versions(),
		Log:      log.With("component", "pluginalign"),
	})
	if align.Changed() > 0 {
		log.Info("出厂插件已对齐",
			"added", len(align.Added), "replaced", len(align.Replaced))
	}

	// 从这里开始交给容器：按 [[bots]] 逐个拿锁、开库、装插件、起连接。
	// main 只保留进程级的三件事——读配置、准备共享资源、决定何时退出。
	c, err := instance.Start(ctx, instance.Options{
		Config:   cfg,
		Log:      log,
		Renderer: renderer,
		Outbound: hc,
		LockWait: lockWait(buildinfo.RestartedByReload()),
		LoadPlugins: func(ctx context.Context, b config.Bot, blog *slog.Logger) ([]host.Plugin, error) {
			// 每 Bot 一套插件进程是 #62 的过渡形态：#65 会把它们合成一套、
			// 由核心按 Bot 给独立 Host 实例（ADR-0008 决策 11）。
			plugins, err := pluginproc.Load(ctx, pluginproc.Options{
				Dir:       pluginDir,
				ConfigFor: cfg.Plugin,
				Disabled:  cfg.Plugins.Disabled,
				Registry:  registry,
				Log:       blog.With("component", "pluginproc"),
			})
			if err != nil {
				return nil, err
			}
			return plugins, nil
		},
		UpgradeFor: func(b config.Bot, db *store.DB, plugins []host.Plugin) core.UpgradeService {
			// 升级是进程级的（换的是这个二进制），但快照落在该 Bot 自己的库上。
			return upgrade.New(upgrade.Options{
				Enabled:   cfg.UpgradeEnabled(),
				APIBase:   cfg.Upgrade.APIBase,
				AssetBase: cfg.Upgrade.AssetBase,
				Token:     cfg.Upgrade.Token,
				Exe:       exePath,
				DataDir:   datadir,
				Current:   buildinfo.Get().Version,
				Plugins:   loadedPlugins(plugins),
				Snapshots: db,
				// 快照按 Bot 分开：裁剪只留 2 份，共用目录等于让两个租户
				// 互相删备份。state.json 仍在 <datadir>/upgrade（进程级标记）。
				SnapshotRoot: cfg.BotDataDir(b),
				Log:          log.With("component", "upgrade", "bot", b.AppID),
			})
		},
	})
	if err != nil {
		return nil, err
	}

	// 升级后的观察窗：容器已经把全部 Bot 的网关协程起来，从这里开始计时
	// 30 秒。能活过去就删掉 pending 标记（ADR-0006 决策 6）。
	go clearPendingAfter(ctx, upgrade.StateDir(datadir), upgradeHealthyWindow, log)

	bi := buildinfo.Get()
	started := 0
	for _, st := range c.Statuses() {
		if st.Err == nil {
			started++
		}
	}
	logStartup(log, startupInfo{
		ConfigPath: *cfgPath,
		DataDir:    datadir,
		Sandbox:    cfg.SandboxEnabled(),
		Bots:       len(cfg.Bots),
		Started:    started,
		Debug:      cfg.Debug.Enabled,
		Version:    bi.String(),
		Platform:   bi.Platform,
		GoVersion:  bi.GoVer,
		Restarted:  buildinfo.RestartedByReload(),
	})
	if buildinfo.RestartedByReload() {
		log.Info("本次启动由 /reload 触发", "version", bi.String())
	}
	if cfg.Debug.Enabled {
		if cfg.Debug.Level == config.DebugPlugin {
			log.Info("调试模式已开启", "level", cfg.Debug.Level,
				"detail", "只打印触发插件的内容与回复正文")
		} else {
			log.Info("调试模式已开启", "level", cfg.Debug.Level,
				"detail", "收到的每条事件、插件命中与回复内容都会打印到控制台",
				"raw", cfg.Debug.Raw)
		}
	}

	req, dropped, fatalErr := waitExit(ctx, c.RestartRequests(), c.Fatal())
	// select 在 ctx.Done 与 /reload 同时就绪时是随机选：选中请求就把一次
	// Ctrl-C 变成"重新执行自身"，systemd 记成异常重启。waitExit 已经把这种
	// 请求丢掉，这里只负责把现场写进日志（P3-22）。
	if dropped != nil {
		log.Warn("退出信号与重启请求同时到达，按关停处理",
			"by", dropped.By, "role", dropped.Role, "reason", dropped.Reason)
	}
	stop()

	switch {
	case req != nil:
		log.Warn("收到重启请求，开始关闭",
			"by", req.By, "role", req.Role, "reason", req.Reason)
	case fatalErr != nil:
		log.Error("全部 Bot 的网关都已停止，开始关闭", "err", fatalErr)
	default:
		log.Info("收到退出信号，开始关闭")
	}

	// 先停接入层：每个 Bot 的网关在 connectOnce 返回前会把 session_id / seq
	// 落盘，容器等完这些才算"状态落盘完成"——它必须发生在关库之前，否则下次
	// 只能冷 Identify，白烧一次 session 配额。超时同样只记账，退出码在下面
	// 统一变成非 0（P3-23）。
	c.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := c.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Error("核心关停未完成", "err", shutdownErr)
	}

	if shutdownErr != nil {
		// 关停没走完就不重启自己：新进程会和还没退干净的旧进程抢同一份
		// 数据目录，而 systemd 会按失败重启这个进程。
		return nil, fmt.Errorf("%w: %w", errShutdown, shutdownErr)
	}

	if req != nil {
		if buildinfo.Supervised() {
			log.Info("已关闭，交回 devrun 重新编译", "version", bi.String())
		} else {
			log.Info("已关闭，准备重新执行自身", "version", bi.String())
		}
	} else {
		log.Info("已退出")
	}
	if fatalErr != nil {
		// 交给退出码：Restart=on-failure 的 StartLimit 会把"反复因封禁而重启"
		// 收敛成一次人工介入，而不是每 60s 一条无人看的重连日志。
		return nil, fatalErr
	}
	return req, nil
}

// notifyShutdown 返回随 Ctrl-C / SIGTERM 取消的 ctx。
//
// 调用点必须紧跟 config.Load：后面的字体解析（约 300ms）、单实例锁、开库与
// 迁移都在这段窗口里，晚注册就会让期间的 SIGTERM 走默认处理（P3-21）。
func notifyShutdown(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// waitExit 等待本次运行的结束原因：关停信号、/reload 请求，或全部网关都停了。
//
// dropped 非 nil 表示 /reload 请求确实到了，但与退出信号同时就绪。select 对
// 同时就绪的多个 case 是随机选，选中请求就会把一次 Ctrl-C 变成"重新执行自身"。
// 所以这里先判 ctx：信号已到就一律按关停走（P3-22）。
//
// 单个 Bot 的网关永久失败**不是**退出理由（那会把其他租户一起带走）：容器只
// 在一条能用的连接都不剩时往 fatal 里放一条错误，#63 会把它变成 failing 态。
func waitExit(ctx context.Context, restarts <-chan core.RestartRequest, fatal <-chan error) (req, dropped *core.RestartRequest, fatalErr error) {
	select {
	case <-ctx.Done():
	case r := <-restarts:
		req = &r
	case fatalErr = <-fatal:
	}
	if ctx.Err() != nil && req != nil {
		dropped, req = req, nil
	}
	return req, dropped, fatalErr
}

// lockWait 决定等单实例锁的上限。
//
// /reload 拉起的新进程是"交接"：旧进程在 exec 之前就放手了，正常情况下一次
// 就能拿到，15 s 只是万一将来改了重启顺序时的保险。普通启动是"竞争"，第二个
// 实例应当立刻失败——等下去不会让占着锁的进程变成不占，只会让用户以为还在启动。
func lockWait(restartedByReload bool) time.Duration {
	if restartedByReload {
		return 15 * time.Second
	}
	return 0
}

// formatDomainLimits 把域名额度排成稳定顺序的 "host=rate" 列表。
// map 遍历是随机的，而启动日志要能两次重启逐字比对。
func formatDomainLimits(limits map[string]int) string {
	hosts := make([]string, 0, len(limits))
	for h := range limits {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, fmt.Sprintf("%s=%d", h, limits[h]))
	}
	return strings.Join(parts, ", ")
}

// startupInfo 是"启动完成"那一行需要的事实。抽成结构体是为了让日志内容可测。
//
// 这里只放进程级事实：库路径、两把锁与插件清单都是每 Bot 一份，分别打在
// "Bot 已启动"那几行里（见 internal/instance），全局行不该假装只有一个。
type startupInfo struct {
	ConfigPath string
	DataDir    string
	Sandbox    bool
	Bots       int
	Started    int
	Debug      bool
	Version    string
	Platform   string
	GoVersion  string
	Restarted  bool
}

// logBots 在启动期逐个打印 Bot 的实际生效配置。
//
// 数据目录与库路径必须可见：它们由 appid 推导（<datadir>/bots/<appid>/qqbot.db），
// 备份、迁移、清理都靠这几行确认东西落在哪。永不打印 secret，连长度也不打。
func logBots(log *slog.Logger, cfg *config.Config) {
	for _, b := range cfg.Bots {
		log.Info("Bot 配置",
			"appid", b.AppID,
			"data_dir", cfg.BotDataDir(b),
			"db", cfg.BotStorePath(b),
			"cert_type", b.CertType,
			"quota_profile", b.QuotaProfile,
			"enabled", b.IsEnabled())
	}
}

// logStartup 打印启动完成。
//
// 插件数量与名字不在这里：一 Bot 一套插件之后，"这个进程装了哪些插件"不再
// 是一行能回答的问题，每个 Bot 的清单打在它自己的"Bot 已启动"那行（P3-17
// 那条"没进产物 vs 没注册"的守卫仍然成立）。
func logStartup(log *slog.Logger, in startupInfo) {
	log.Info("qqbot 启动完成",
		"config", in.ConfigPath,
		"datadir", in.DataDir,
		"sandbox", in.Sandbox,
		"bots", in.Bots,
		"started", in.Started,
		"debug", in.Debug,
		"version", in.Version,
		"platform", in.Platform,
		"go", in.GoVersion,
		"restarted", in.Restarted)
}
