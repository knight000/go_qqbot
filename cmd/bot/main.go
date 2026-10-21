// Command bot 是唯一装配点：加载配置 → 打开存储 → 构造核心 → 连接网关。
//
// 内置插件在本文件 blank import（P1 编译期注册，见 ARCHITECTURE.md §3.1）；
// plugins/ 下的其余插件由 tools/genplugins 生成到 plugins_gen.go，见 §3.5。
//
//go:generate go run ../../tools/genplugins
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/instance"
	"github.com/knight000/go_qqbot/internal/logx"
	"github.com/knight000/go_qqbot/internal/render"
	"github.com/knight000/go_qqbot/internal/store"

	_ "github.com/knight000/go_qqbot/plugins/adminping"
	_ "github.com/knight000/go_qqbot/plugins/bootnotice"
	_ "github.com/knight000/go_qqbot/plugins/echo"
	_ "github.com/knight000/go_qqbot/plugins/md"
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
	req, err := run()
	if err != nil {
		if errors.Is(err, errShutdown) {
			fmt.Fprintln(os.Stderr, "关停失败:", err)
		} else {
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
	// 受 devrun 托管时不换映像：退出码就是协议，父进程负责重新生成装配、编译、
	// 替换二进制再拉起。这是新放入 plugins/ 的插件能生效的唯一路径——重新执行
	// 同一份二进制不会改变链接进来的插件集合。
	if buildinfo.Supervised() {
		os.Exit(buildinfo.ExitRestartRequired)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "重启失败：定位可执行文件:", err)
		os.Exit(1)
	}
	if err := restartSelf(exe); err != nil {
		fmt.Fprintln(os.Stderr, "重启失败:", err)
		os.Exit(1)
	}
}

// run 正常退出时返回 nil；返回非 nil 表示需要由 main 重新执行自身（/reload）。
func run() (*core.RestartRequest, error) {
	cfgPath := flag.String("c", "config/config.local.toml", "配置文件路径")
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
	// 两个进程共用一份数据目录会互相顶掉网关 session（日志里是 4902 reset by
	// resume），并且各自回复一遍消息、各记一遍幂等表——不是"慢一点"，是明确错误。
	// 锁按数据库路径划分：不同 [store].path 就是不同实例，可以同时跑。
	//
	// 注意锁的 defer 注册在 db.Close 之前，所以退出时的顺序是"先关库再放手"，
	// 不会给下一个实例留下一个刚拿到锁但库还开着的窗口。
	lockWait := 2 * time.Second
	if buildinfo.RestartedByReload() {
		// /reload 是交接而不是竞争：run 返回时锁已经放开，正常情况下新进程
		// 一次就能拿到。多等一会儿只是保险——万一将来改了重启顺序（比如
		// 先拉起子进程再放手），这里不会变成"重启后起不来"。
		lockWait = 15 * time.Second
	}
	lock, err := instance.Acquire(cfg.Store.Path+".lock", lockWait)
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
		BaseURL:  cfg.App.APIBase,
		TokenURL: cfg.App.TokenURL,
		AppID:    cfg.App.ID,
		Secret:   cfg.App.Secret,
		State:    db,
		Log:      log.With("component", "api"),
	})

	coreImpl, err := core.New(core.Options{
		Store:   db,
		API:     apiClient,
		HTTP:    hc,
		Log:     log,
		Config:  cfg,
		Plugins: host.Registered(),
		// 核心只认 host.Renderer 接口，不 import 渲染实现包。
		Renderer: renderer,
	})
	if err != nil {
		return nil, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	gwDone := make(chan struct{})
	go func() {
		defer close(gwDone)
		gw.Run(ctx)
	}()

	bi := buildinfo.Get()
	log.Info("qqbot 启动完成",
		"config", *cfgPath,
		"db", db.Path(),
		"lock", lock.Path(),
		"sandbox", cfg.App.Sandbox,
		"debug", cfg.Debug.Enabled,
		"version", bi.String(),
		"platform", bi.Platform,
		"go", bi.GoVer,
		"restarted", buildinfo.RestartedByReload(),
		"plugins", len(host.Registered()))
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

	var req *core.RestartRequest
	select {
	case <-ctx.Done():
	case r := <-coreImpl.RestartRequests():
		req = &r
	}
	stop()

	if req != nil {
		log.Warn("收到重启请求，开始关闭",
			"by", req.By, "role", req.Role, "reason", req.Reason)
	} else {
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
