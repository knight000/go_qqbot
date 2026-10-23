package pluginproc_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginproc"
	"github.com/knight000/go_qqbot/pkg/host"
)

// message 造一条进站消息。Command / Args 由核心在路由阶段填好（契约 8）。
func message(command, args string) *host.Event {
	return &host.Event{
		ID:        "m-" + command + "-" + args,
		Kind:      host.KindMessage,
		Scene:     host.SceneGroup,
		PeerID:    "g1",
		SenderID:  "u1",
		Content:   strings.TrimSpace(command + " " + args),
		Command:   command,
		Args:      args,
		MsgID:     "m1",
		Timestamp: time.Now(),
		Actor: host.Principal{
			OpenID: "u1", Scene: host.SceneGroup, PeerID: "g1",
			Role: host.RoleUser, GroupRole: host.GroupRoleMember,
		},
	}
}

// testHost 是插件可见的 Host：真 host.Service（生产那条装配路径），只是
// 后端全空——本用例的插件只读 Config() 与 Plugin()。
func testHost(name string, cfg map[string]any) host.Host {
	return host.NewService(host.Options{Name: name, Config: host.NewParams(name, cfg)})
}

// loadOne 是绝大多数用例的开头：装一个角色、Load、拿回那个插件。
// pidFile 指向子进程自报的 pid，供"关停后不留孤儿"类断言使用。
func loadOne(t *testing.T, mode string, mutate func(*pluginproc.Options)) (host.Plugin, *logSink, string) {
	t.Helper()
	dir := t.TempDir()
	install(t, dir, mode)

	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv(pidEnv, pidFile)

	logs := newLogs()
	o := pluginproc.Options{
		Dir: dir,
		Log: logs.logger(),
		ConfigFor: func(string) map[string]any {
			return map[string]any{"greeting": "你好"}
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	plugins, err := pluginproc.Load(context.Background(), o)
	if err != nil {
		t.Fatalf("Load: %v（日志：\n%s）", err, logs.text())
	}
	if len(plugins) != 1 {
		t.Fatalf("应发现 1 个插件，实际 %d（日志：\n%s）", len(plugins), logs.text())
	}
	return plugins[0], logs, pidFile
}

// initPlugin 让插件进程起来。失败即致命：后面的断言都以"进程在跑"为前提。
func initPlugin(t *testing.T, p host.Plugin, cfg map[string]any) {
	t.Helper()
	if err := p.Init(context.Background(), testHost(p.Manifest().Name, cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// killByPid 是还没实现 Close 那几个用例的兜底清理：无论如何都不留孤儿进程。
func killByPid(t *testing.T, pidFile string) {
	t.Helper()
	blob, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

// Init 起进程并把配置快照交过去；Handle 把事件转发过去、把回复带回来。
// 顺带验两件容易被忽略的事：stderr 逐行进 slog（带 plugin 前缀），
// 以及 Init 不会重新探测一次清单。
func TestInitSpawnsAndHandleForwards(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, logs, pidFile := loadOne(t, modeEcho, nil)
	initPlugin(t, p, map[string]any{"greeting": "你好"})
	t.Cleanup(func() { killByPid(t, pidFile) })

	ctx := context.Background()
	// 1. 配置快照必须过通道（插件读的是 plugin.init 给的 map，不是它自己的环境）。
	reply, err := p.Handle(ctx, message("/cfg", ""))
	if err != nil {
		t.Fatalf("Handle(/cfg): %v", err)
	}
	if reply == nil || reply.Content != "你好" {
		t.Fatalf("/cfg 回复 = %#v，期望配置里的 greeting", reply)
	}
	// 2. 普通命令的参数与回复要一字不差地往返。
	reply, err = p.Handle(ctx, message("/echo", "参数 带空格"))
	if err != nil {
		t.Fatalf("Handle(/echo): %v", err)
	}
	if reply == nil || reply.Content != "参数 带空格" {
		t.Fatalf("/echo 回复 = %#v", reply)
	}
	// 3. 插件 stderr 逐行转发，且带 plugin=<name>：排障靠这一条定位是哪个插件。
	// 逐行转发 + 同一行上带 plugin=<name>：两件事一起断言，否则"多打了一遍
	// 日志"和"打给了错的插件"都会漏过去。
	for _, want := range []string{"init: 第一行", "init: 第二行"} {
		if !logs.contains(want + " plugin=" + modeEcho) {
			t.Errorf("日志里缺少带 plugin 前缀的 stderr 行 %q：\n%s", want, logs.text())
		}
	}
	// 4. 清单来自装配期那一次探测，Init 不重新探测。
	if n := countLines(t, marker, "probe="); n != 1 {
		t.Fatalf("探测次数 = %d，期望 1", n)
	}
}
