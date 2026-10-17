// Command bot 是唯一装配点：加载配置 → 打开存储 → 构造核心 → 连接网关。
//
// 加插件只需在下方 blank import 一行（P1 编译期注册），见 ARCHITECTURE.md §3.1。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	// 内嵌 IANA 时区库：Windows 单文件与精简容器（scratch/alpine）没有 zoneinfo，
	// 不嵌的话 time.LoadLocation("Asia/Shanghai") 会失败并回退 UTC，cron 时间整体偏移 8 小时。
	_ "time/tzdata"

	"github.com/knight000/qqbot/internal/api"
	"github.com/knight000/qqbot/internal/buildinfo"
	"github.com/knight000/qqbot/internal/config"
	"github.com/knight000/qqbot/internal/core"
	"github.com/knight000/qqbot/internal/gateway"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/httpx"
	"github.com/knight000/qqbot/internal/logx"
	"github.com/knight000/qqbot/internal/store"

	_ "github.com/knight000/qqbot/plugins/adminping"
	_ "github.com/knight000/qqbot/plugins/bootnotice"
	_ "github.com/knight000/qqbot/plugins/echo"
)

func main() {
	req, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
	if req == nil {
		return
	}

	// 走到这里时 run 已经返回：网关断开、session 落盘、数据库关闭。
	// 此时再做进程替换，新进程不会和旧进程抢同一个 gateway session 与 SQLite 连接。
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

	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		return nil, err
	}

	hc := httpx.New(httpx.Options{
		Timeout:    cfg.HTTP.Timeout.Duration,
		Retries:    cfg.HTTP.Retries,
		RatePerMin: cfg.HTTP.RatePerMin,
	})
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

	gwDone := make(chan struct{})
	go func() {
		defer close(gwDone)
		gw.Run(ctx)
	}()

	bi := buildinfo.Get()
	log.Info("qqbot 启动完成",
		"config", *cfgPath,
		"db", db.Path(),
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

	// 先停接入层：让它把 session_id / seq 落盘，下次启动就能 Resume
	select {
	case <-gwDone:
	case <-time.After(5 * time.Second):
		log.Warn("网关关闭超时，强制退出")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coreImpl.Shutdown(shutdownCtx)

	if req != nil {
		log.Info("已关闭，准备重新执行自身", "version", bi.String())
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
