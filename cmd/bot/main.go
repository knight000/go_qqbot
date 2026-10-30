// Command bot 是唯一装配点：加载配置 → 打开存储 → 构造核心 → 连接网关。
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

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/gateway"
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

const (
	// gatewayStopTimeout 是等网关读循环退出的上限。它退出前会把
	// session_id / seq 落盘（connectOnce 的 defer），所以这个等待就是
	// "等状态落盘完成"——必须早于 db.Close。
	gatewayStopTimeout = 5 * time.Second
	// shutdownTimeout 覆盖调度收尾、派发排空、GC 退出与插件关闭。
	shutdownTimeout = 10 * time.Second
)

func main() {
	// 子命令在 flag.Parse 之前分派：`qqbot init -c x` 的 -c 属于 init，
	// 不能落到启动用的 flag 集合里。不带子命令时完全是原来的启动行为。
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(runInit(os.Args[2:], os.Stdout, os.Stderr))
	}

	req, err := run()
	if err != nil {
		var fatal *gateway.FatalError
		switch {
		case errors.As(err, &fatal):
			fmt.Fprintln(os.Stderr, "网关永久性错误，已停止重连:", fatal.Reason)
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

	// 本票只装配第一个 Bot：把进程拆成「进程级容器 + 每 Bot 编排器」是 #62。
	// 校验保证列表非空（config.Validate 对空列表直接报错），所以不判长度。
	bot := cfg.Bots[0]
	logBots(log, cfg)
	if len(cfg.Bots) > 1 {
		// 配置已经能表达多个 Bot，但编排还没落地：静默只跑一个是这里最坏的
		// 处置——用户会以为其余的号也在线。这条 warn 随 #62 一起删。
		log.Warn("本版本只连接第一个 Bot，其余 Bot 尚未编排",
			"configured", len(cfg.Bots), "running", bot.AppID)
	}

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
	fonts, err := render.LoadFonts()
	if err != nil {
		return nil, err
	}
	renderer := render.New(fonts, log.With("component", "render"))

	// 单实例锁：必须在打开数据库、连网关之前拿到。
	//
	// 两个进程跑同一个 bot 会互相顶掉网关 session（日志里是 4902 reset by
	// resume），并且各自回复一遍消息、各记一遍幂等表——不是"慢一点"，是明确错误。
	// 锁分两把：appID 维度挡"同一 bot 换了个数据目录"，数据目录维度挡"两个 bot
	// 共用一份库"。只按数据目录划分是上一版的缺陷：换个 [store].path 就能双开。
	//
	// 注意锁的 defer 注册在 db.Close 之前，所以退出时的顺序是"先关库再放手"，
	// 不会给下一个实例留下一个刚拿到锁但库还开着的窗口。
	//
	// 只有 /reload 需要等：那是交接而不是竞争（旧进程在 exec 前已经放手），
	// 多等一会儿只是保险。普通启动必须 wait=0——双开时等下去没有任何意义，
	// 只会让用户误以为进程还在启动中。
	lock, err := instance.Acquire(instance.Options{
		AppID:    bot.AppID,
		DataPath: cfg.Store.Path + ".lock",
		Wait:     lockWait(buildinfo.RestartedByReload()),
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Warn("释放单实例锁失败", "lock", lock.Path(), "err", err)
		}
	}()

	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		return nil, err
	}
	// 关库是关停序列的最后一步：网关落盘（等 gwDone）→ core.Shutdown
	// （调度收尾 → 派发排空 → GC 退出 → 插件关闭）→ 这里。
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		return nil, err
	}

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
	apiClient := api.New(api.Options{
		HTTP:     hc,
		BaseURL:  cfg.Core.APIBase,
		TokenURL: cfg.Core.TokenURL,
		AppID:    bot.AppID,
		Secret:   bot.Secret,
		State:    db,
		Log:      log.With("component", "api"),
	})

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
	align := pluginalign.Align(ctx, pluginalign.Options{
		Dir:      filepath.Join(datadir, "plugins", "bin"),
		Bundle:   pluginbundle.Files(),
		Versions: pluginbundle.Versions(),
		Log:      log.With("component", "pluginalign"),
	})
	if align.Changed() > 0 {
		log.Info("出厂插件已对齐",
			"added", len(align.Added), "replaced", len(align.Replaced))
	}

	procPlugins, err := pluginproc.Load(ctx, pluginproc.Options{
		Dir:       filepath.Join(datadir, "plugins", "bin"),
		ConfigFor: cfg.Plugin,
		Disabled:  cfg.Plugins.Disabled,
		Registry:  registry,
		Log:       log.With("component", "pluginproc"),
	})
	if err != nil {
		return nil, err
	}
	plugins := procPlugins

	coreImpl, err := core.New(core.Options{
		Store:   db,
		API:     apiClient,
		HTTP:    hc,
		Log:     log,
		Config:  cfg,
		Plugins: plugins,
		// 核心只认 host.Renderer 接口，不 import 渲染实现包。
		Renderer: renderer,
		Upgrade: upgrade.New(upgrade.Options{
			Enabled:   cfg.UpgradeEnabled(),
			APIBase:   cfg.Upgrade.APIBase,
			AssetBase: cfg.Upgrade.AssetBase,
			Token:     cfg.Upgrade.Token,
			Exe:       exePath,
			DataDir:   datadir,
			Current:   buildinfo.Get().Version,
			Plugins:   loadedPlugins(plugins),
			Snapshots: db,
			Log:       log.With("component", "upgrade"),
		}),
	})
	if err != nil {
		return nil, err
	}

	if err := coreImpl.Start(ctx); err != nil {
		return nil, err
	}

	gw := gateway.New(gateway.Options{
		API:        apiClient,
		Store:      db,
		Log:        log,
		Intents:    cfg.Bot.Intents,
		ShardTotal: cfg.Bot.ShardTotal,
		ShardID:    cfg.Bot.ShardID,
		Resume:     cfg.Bot.Resume,
		Handler:    coreImpl.Handle,
		RawDump:    rawDumpFunc(cfg, coreImpl),
	})
	// 先接线再起协程：Handle 里读这个字段与这里写它之间靠 goroutine 启动建立顺序，
	// 反过来（先起 gw.Run 再 SetReconnector）就是数据竞争。
	coreImpl.SetReconnector(gw)

	// gwRes 收 Run 的返回值：正常退出（ctx 取消）是 nil，永久性错误
	// （下架/封禁/intents 无权限）是一个可判定的 *gateway.FatalError。
	gwDone := make(chan struct{})
	gwRes := make(chan error, 1)
	go func() {
		defer close(gwDone)
		gwRes <- gw.Run(ctx)
	}()

	// 升级后的观察窗：core.Start 已经完成、网关协程已经起来，从这里开始
	// 计时 30 秒。能活过去就删掉 pending 标记（ADR-0006 决策 6）。
	go clearPendingAfter(ctx, upgrade.StateDir(datadir), upgradeHealthyWindow, log)

	bi := buildinfo.Get()
	logStartup(log, startupInfo{
		ConfigPath:  *cfgPath,
		DBPath:      db.Path(),
		LockPath:    lock.Path(),
		AppLockPath: lock.AppPath(),
		Sandbox:     cfg.SandboxEnabled(),
		Bots:        len(cfg.Bots),
		Debug:       cfg.Debug.Enabled,
		Version:     bi.String(),
		Platform:    bi.Platform,
		GoVersion:   bi.GoVer,
		Restarted:   buildinfo.RestartedByReload(),
		Plugins:     plugins,
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

	req, dropped, gwFatal := waitExit(ctx, coreImpl.RestartRequests(), gwDone, gwRes)
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
	case gwFatal != nil:
		log.Error("网关永久性错误，停止重连，开始关闭", "err", gwFatal)
	default:
		log.Info("收到退出信号，开始关闭")
	}

	// 先停接入层：connectOnce 返回前会把 session_id / seq 落盘，所以等 gwDone
	// 就是等"网关状态落盘完成"——它必须发生在 db.Close 之前，否则下次只能冷
	// Identify，白烧一次 session 配额。
	//
	// 超时不能静默：那一刻网关可能还在写库，照常往下走就是 db.Close 与在途写
	// 并发。这里只记账，退出码在下面统一变成非 0（P3-23）。
	gwStopped := true
	select {
	case <-gwDone:
	case <-time.After(gatewayStopTimeout):
		gwStopped = false
		log.Error("网关读循环未在期限内退出，状态可能未落盘",
			"timeout", gatewayStopTimeout.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := coreImpl.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Error("核心关停未完成", "err", shutdownErr)
	}
	if !gwStopped {
		shutdownErr = errors.Join(shutdownErr, errors.New("网关读循环未退出"))
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
	if gwFatal != nil {
		// 交给退出码：Restart=on-failure 的 StartLimit 会把"反复因封禁而重启"
		// 收敛成一次人工介入，而不是每 60s 一条无人看的重连日志。
		return nil, gwFatal
	}
	return req, nil
}

// rawDumpFunc 只在 [debug].enabled 与 [debug].raw 同时为真时，
// 把平台原始 JSON 接到核心的调试轨迹上；否则返回 nil，接入层不做任何额外工作。
func rawDumpFunc(cfg *config.Config, c *core.Core) func(string, []byte) {
	if !cfg.Debug.Enabled || !cfg.Debug.Raw {
		return nil
	}
	return c.PrintRaw
}

// notifyShutdown 返回随 Ctrl-C / SIGTERM 取消的 ctx。
//
// 调用点必须紧跟 config.Load：后面的字体解析（约 300ms）、单实例锁、开库与
// 迁移都在这段窗口里，晚注册就会让期间的 SIGTERM 走默认处理（P3-21）。
func notifyShutdown(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// waitExit 等待本次运行的结束原因：关停信号、/reload 请求，或网关自己退出。
//
// dropped 非 nil 表示 /reload 请求确实到了，但与退出信号同时就绪。select 对
// 同时就绪的多个 case 是随机选，选中请求就会把一次 Ctrl-C 变成"重新执行自身"。
// 所以这里先判 ctx：信号已到就一律按关停走（P3-22）。
func waitExit(ctx context.Context, restarts <-chan core.RestartRequest, gwDone <-chan struct{}, gwRes <-chan error) (req, dropped *core.RestartRequest, gwFatal error) {
	select {
	case <-ctx.Done():
	case r := <-restarts:
		req = &r
	case <-gwDone:
		// 网关自己退出了。正常退出只发生在 ctx 取消之后，所以走到这里往往是
		// 永久性错误（4001/4002/4010–4014/4914/4915）：再连也不会好，
		// 但也不能留着一个"进程在跑、机器人已死"的躯壳。
		gwFatal = <-gwRes
	}
	if ctx.Err() != nil && req != nil {
		dropped, req = req, nil
	}
	return req, dropped, gwFatal
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
type startupInfo struct {
	ConfigPath  string
	DBPath      string
	LockPath    string
	AppLockPath string
	Sandbox     bool
	Bots        int
	Debug       bool
	Version     string
	Platform    string
	GoVersion   string
	Restarted   bool
	Plugins     []host.Plugin
}

// logBots 在启动期逐个打印 Bot 的实际生效配置。
//
// 数据目录必须可见：它由 appid 推导（[store].path 所在目录下的 bots/<appid>/），
// 用户只有在这一行才能确认"这台机器的数据到底落在哪"——备份、迁移、清理都靠它。
// 永不打印 secret，连长度也不打。
func logBots(log *slog.Logger, cfg *config.Config) {
	for _, b := range cfg.Bots {
		log.Info("Bot 配置",
			"appid", b.AppID,
			"data_dir", cfg.BotDataDir(b),
			"cert_type", b.CertType,
			"quota_profile", b.QuotaProfile,
			"enabled", b.IsEnabled())
	}
}

// logStartup 打印启动完成。插件既报数量也报名字：
//
// 只报 plugins=4 时，"新插件没进二进制"与"插件进去了但没注册"看起来一模一样，
// 现场只能靠"指令没反应"反推，而构建日志里全是成功（P3-17）。
func logStartup(log *slog.Logger, in startupInfo) {
	log.Info("qqbot 启动完成",
		"config", in.ConfigPath,
		"db", in.DBPath,
		"lock", in.LockPath,
		"app_lock", in.AppLockPath,
		"sandbox", in.Sandbox,
		"bots", in.Bots,
		"debug", in.Debug,
		"version", in.Version,
		"platform", in.Platform,
		"go", in.GoVersion,
		"restarted", in.Restarted,
		"plugins", len(in.Plugins),
		"plugin_names", pluginNames(in.Plugins))
}

// pluginNames 把已装配的插件按名字排序后用逗号连起来。
// 排序只为两次启动能逐字比对——pluginproc.Load 的顺序来自文件名扫描。
func pluginNames(plugins []host.Plugin) string {
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Manifest().Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
