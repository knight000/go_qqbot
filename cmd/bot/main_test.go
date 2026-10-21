package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
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

// 启动日志必须打印插件名：只报数量时，"新插件没编进二进制"与"插件进去了
// 但没注册"在日志里完全一样（P3-17）。
func TestLogStartupListsPluginNames(t *testing.T) {
	var buf bytes.Buffer
	logStartup(slog.New(slog.NewTextHandler(&buf, nil)), startupInfo{
		Version: "v0.0.0-test",
		Plugins: []host.Plugin{fakePlugin{name: "echo"}, fakePlugin{name: "adminping"}},
	})

	out := buf.String()
	if !strings.Contains(out, "plugins=2") {
		t.Fatalf("启动日志应带插件数量，实际: %s", out)
	}
	i := strings.Index(out, "plugin_names=")
	if i < 0 {
		t.Fatalf("启动日志缺 plugin_names，实际: %s", out)
	}
	field := out[i:]
	ai, ei := strings.Index(field, "adminping"), strings.Index(field, "echo")
	if ai < 0 || ei < 0 {
		t.Fatalf("plugin_names 应包含全部插件名，实际: %s", field)
	}
	if ai > ei {
		t.Fatalf("插件名应按字典序输出，实际: %s", field)
	}
}

type fakePlugin struct{ name string }

func (f fakePlugin) Manifest() host.Manifest             { return host.Manifest{Name: f.name} }
func (fakePlugin) Init(context.Context, host.Host) error { return nil }
func (fakePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
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
		"store.Open(",
		"db.Migrate(",
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
