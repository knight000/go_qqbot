package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 第二个实例不该白等：只有 /reload 的交接场景才需要给旧进程留余量。
func TestLockWait(t *testing.T) {
	if got := lockWait(false); got != 0 {
		t.Fatalf("普通启动应当立刻判失败，实际等了 %s", got)
	}
	if got := lockWait(true); got != 15*time.Second {
		t.Fatalf("/reload 交接应当留 15s 余量，实际 %s", got)
	}
}

// 启动日志要把实际生效的域名额度打出来，且顺序稳定（map 遍历是随机的）。
func TestFormatDomainLimits(t *testing.T) {
	got := formatDomainLimits(map[string]int{
		"api.torn.com": 800,
		"example.com":  50,
		"torn.com":     100,
	})
	want := "api.torn.com=800, example.com=50, torn.com=100"
	if got != want {
		t.Fatalf("formatDomainLimits=%q，期望 %q", got, want)
	}
	if got := formatDomainLimits(nil); got != "" {
		t.Fatalf("空清单应为空串，实际 %q", got)
	}
}

// 启动完成那一行只报进程级事实：库路径、两把锁与插件清单都是每 Bot 一份，
// 逐个打在容器自己的"Bot 已启动"里。全局行混进一个 db= 会让人以为只有一个库。
func TestLogStartupReportsProcessLevelFacts(t *testing.T) {
	var buf bytes.Buffer
	logStartup(slog.New(slog.NewTextHandler(&buf, nil)), startupInfo{
		ConfigPath: "config.toml",
		DataDir:    "data",
		Version:    "v0.0.0-test",
		Bots:       2,
		Started:    1,
	})

	out := buf.String()
	for _, want := range []string{"config=config.toml", "datadir=data", "bots=2", "started=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("启动日志缺少 %q：%s", want, out)
		}
	}
	if strings.Contains(out, "db=") {
		t.Errorf("全局启动行不该出现单数 db=（每 Bot 各一份）：%s", out)
	}
}

// 每个 Bot 的数据目录必须在启动日志里可见：它由 appid 推导，备份、迁移、
// 清理都靠这一行确认"东西到底落在哪"。secret 一个字节都不许出现。
func TestLogBotsPrintsDataDirAndNeverSecret(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		Store: config.Store{Path: "data/qqbot.db"},
		Bots: []config.Bot{
			{AppID: "102000001", Secret: "super-secret"},
			{AppID: "102000002", Secret: "another-secret", DataDir: "/srv/qqbot/manual"},
		},
	}
	logBots(slog.New(slog.NewTextHandler(&buf, nil)), cfg)

	out := buf.String()
	if got := strings.Count(out, "Bot 配置"); got != 2 {
		t.Fatalf("应每个 Bot 一行，实际 %d 行:\n%s", got, out)
	}
	for _, want := range []string{
		"data_dir=", "appid=102000001", "appid=102000002",
		"data_dir=/srv/qqbot/manual",
		// 库路径也必须是推导结果：运维照着这一行去备份，不能是 [store].path。
		"db=" + filepath.Join("data", "bots", "102000001", "qqbot.db"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("启动日志缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "super-secret") || strings.Contains(out, "another-secret") {
		t.Errorf("启动日志泄露了 secret：\n%s", out)
	}
}

// 退出信号与 /reload 同时就绪时 select 随机选，选中请求就会把 Ctrl-C 变成重启。
// 无论 select 选中哪个 case，只要 ctx 已取消，waitExit 都不能返回重启请求（P3-22）。
func TestWaitExitDropsRestartAfterSignal(t *testing.T) {
	// 两个 case 同时就绪，选中是随机的；重复多轮，任何一轮把请求交出去都算回归。
	for i := 0; i < 100; i++ {
		restarts := make(chan core.RestartRequest, 1)
		restarts <- core.RestartRequest{By: "owner", Role: "owner"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		req, _, fatalErr := waitExit(ctx, restarts, make(chan error, 1))
		if req != nil {
			t.Fatalf("第 %d 轮：退出信号已到，却返回了重启请求 %+v", i, req)
		}
		if fatalErr != nil {
			t.Fatalf("第 %d 轮：没有 fatal 错误: %v", i, fatalErr)
		}
	}

	// 正常情况：没有退出信号时请求照常生效，且不算被丢弃。
	restarts := make(chan core.RestartRequest, 1)
	restarts <- core.RestartRequest{By: "owner", Role: "owner"}
	req, dropped, fatalErr := waitExit(context.Background(), restarts, make(chan error, 1))
	if req == nil || req.By != "owner" {
		t.Fatalf("没有退出信号时应返回重启请求，实际 %+v", req)
	}
	if dropped != nil {
		t.Fatalf("没有退出信号时不该标记丢弃，实际 %+v", dropped)
	}
	if fatalErr != nil {
		t.Fatalf("没有 fatal 错误: %v", fatalErr)
	}

	// 全部网关都停了：进程必须以非 0 退出（单 Bot 时代这条也有）。
	fatal := make(chan error, 1)
	fatal <- errors.New("网关永久性错误")
	_, _, fatalErr = waitExit(context.Background(), make(chan core.RestartRequest), fatal)
	if fatalErr == nil {
		t.Fatal("fatal 通道有错误时 waitExit 必须交出来")
	}
}

// 信号注册必须紧跟 config.Load：字体解析（约 300ms）、拿锁、开库、迁移都在
// 它之后。挪到它们后面，这段窗口里的 SIGTERM 会走默认处理——进程被杀、
// 会话不落盘、systemd 记成 killed by signal（P3-21）。
//
// 这是源码顺序断言：这条不变量没有可注入的观察点（Windows 上也没法给自己发
// SIGTERM），所以直接守住 run() 里几个关键调用的先后。
func TestSignalRegistrationPrecedesSlowStartup(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	order := []string{
		"cfg, err := config.Load(",
		"notifyShutdown(context.Background())",
		"render.LoadFonts()",
		"instance.Start(ctx, instance.Options{",
	}
	prev, prevAt := "", -1
	for _, marker := range order {
		at := strings.Index(src, marker)
		if at < 0 {
			t.Fatalf("main.go 里找不到 %q：启动顺序的守卫要跟着改", marker)
		}
		if at < prevAt {
			t.Fatalf("%q 必须出现在 %q 之后", marker, prev)
		}
		prev, prevAt = marker, at
	}
}

// fakePlugin 是只带名字的插件替身：够 loadedPlugins 与启动日志的断言。
type fakePlugin struct{ name string }

func (f fakePlugin) Manifest() host.Manifest { return host.Manifest{Name: f.name} }
func (fakePlugin) Init(context.Context, host.Host) error {
	return nil
}
func (fakePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) { return nil, nil }
