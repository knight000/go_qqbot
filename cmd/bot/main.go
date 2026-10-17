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
	"github.com/knight000/qqbot/internal/config"
	"github.com/knight000/qqbot/internal/core"
	"github.com/knight000/qqbot/internal/gateway"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/httpx"
	"github.com/knight000/qqbot/internal/logx"
	"github.com/knight000/qqbot/internal/store"

	_ "github.com/knight000/qqbot/plugins/adminping"
	_ "github.com/knight000/qqbot/plugins/echo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("c", "config/config.local.toml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	log := logx.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)

	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		return err
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
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := coreImpl.Start(ctx); err != nil {
		return err
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
	})

	gwDone := make(chan struct{})
	go func() {
		defer close(gwDone)
		gw.Run(ctx)
	}()

	log.Info("qqbot 启动完成",
		"config", *cfgPath,
		"db", db.Path(),
		"sandbox", cfg.App.Sandbox,
		"plugins", len(host.Registered()))

	<-ctx.Done()
	stop()
	log.Info("收到退出信号，开始关闭")

	// 先停接入层：让它把 session_id / seq 落盘，下次启动就能 Resume
	select {
	case <-gwDone:
	case <-time.After(5 * time.Second):
		log.Warn("网关关闭超时，强制退出")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coreImpl.Shutdown(shutdownCtx)

	log.Info("已退出")
	return nil
}
