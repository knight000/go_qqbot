package pluginproc_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginproc"
	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
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
// pidDir 是子进程自报 pid 的目录（每个角色一个文件），供"关停后不留孤儿"
// 类断言使用。
func loadOne(t *testing.T, mode string, mutate func(*pluginproc.Options)) (host.Plugin, *logSink, string) {
	t.Helper()
	dir := t.TempDir()
	install(t, dir, mode)

	pidDir := t.TempDir()
	t.Setenv(pidEnv, pidDir)

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
	return plugins[0], logs, pidDir
}

// initPlugin 让插件进程起来。失败即致命：后面的断言都以"进程在跑"为前提。
func initPlugin(t *testing.T, p host.Plugin, cfg map[string]any) {
	t.Helper()
	if err := p.Init(context.Background(), testHost(p.Manifest().Name, cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// readPid 读某个角色自报的 pid。文件还没出现（进程没起来）返回 0。
func readPid(t *testing.T, pidDir, mode string) int {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(pidDir, mode+".pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil {
		return 0
	}
	return pid
}

// killByPid 是还没实现 Close 那几个用例的兜底清理：无论如何都不留孤儿进程。
func killByPid(t *testing.T, pidDir string) {
	t.Helper()
	entries, err := os.ReadDir(pidDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if pid := readPid(t, pidDir, strings.TrimSuffix(e.Name(), ".pid")); pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	}
}

// waitGone 轮询到进程消失（或超时后报错）。进程退出与句柄回收之间有一小段
// 延迟，所以"立刻断言"会偶发假失败。
func waitGone(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processAlive(pid)
}

// Init 起进程并把配置快照交过去；Handle 把事件转发过去、把回复带回来。
// 顺带验两件容易被忽略的事：stderr 逐行进 slog（带 plugin 前缀），
// 以及 Init 不会重新探测一次清单。
func TestInitSpawnsAndHandleForwards(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, logs, pidDir := loadOne(t, modeEcho, nil)
	initPlugin(t, p, map[string]any{"greeting": "你好"})
	t.Cleanup(func() { killByPid(t, pidDir) })

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

// 卡死的插件必须在 3 秒内被放弃，而且**只放弃这一次**：同一条连接上的下一条
// 消息要照常处理（per-peer 队列继续往下走，ADR-0005 决策 7 / 票 #40 验收）。
//
// 用真子进程而不是接口替身：替身只能证明"proxy 会调 ctx"，证明不了"卡在另一
// 个进程里的调用能被本进程的期限放弃"——后者才是跨进程新增的那条失败路径。
func TestHandleTimesOutAndNextEventStillServes(t *testing.T) {
	p, _, pidDir := loadOne(t, modeSlow, nil)
	initPlugin(t, p, nil)
	t.Cleanup(func() { killByPid(t, pidDir) })

	start := time.Now()
	_, err := p.Handle(context.Background(), message("/slow", "hang"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("卡死的插件必须返回错误，不能挂在那里等")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时错误应可 errors.Is(context.DeadlineExceeded)，实际 %v", err)
	}
	// 3 秒是既定口径（rpc.HandleTimeout），不是这里另拍的数字。
	if elapsed < rpc.HandleTimeout || elapsed > rpc.HandleTimeout+2*time.Second {
		t.Fatalf("超时用了 %s，期望 ≈%s", elapsed, rpc.HandleTimeout)
	}

	// 下一次投递不受影响：超时只取消那一次请求，不毒化连接。
	reply, err := p.Handle(context.Background(), message("/slow", "ok"))
	if err != nil {
		t.Fatalf("超时之后的下一条应正常处理，实际 %v", err)
	}
	if reply == nil || reply.Content != "slow-ok" {
		t.Fatalf("超时之后的下一条回复 = %#v", reply)
	}
}

// Job 侧同一条语义：期限来自调用方（核心给的是 spec.Timeout，默认 60 秒），
// 到点即放弃并把这次调用记为失败——不能因为"跨了进程"就放宽。
func TestRunJobHonoursDeadline(t *testing.T) {
	p, _, pidDir := loadOne(t, modeSlow, nil)
	initPlugin(t, p, nil)
	t.Cleanup(func() { killByPid(t, pidDir) })

	jobber, ok := p.(host.JobRunner)
	if !ok {
		t.Fatal("proxy 必须实现 host.JobRunner：插件那边有没有由 manifest 的 optional 决定")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := jobber.RunJob(ctx, host.JobSpec{Plugin: modeSlow, Name: "hang", Payload: "hang"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("卡死的任务应返回 DeadlineExceeded，实际 %v", err)
	}
}

// 每一次超时都是一次失败，与崩溃共用同一套熔断计数（决策 7/8）：连续 5 次
// 之后停用该插件、收掉它的进程，并留一条可读的「已停用」。
//
// 这里用 150ms 的期限打 5 次而不是等 5×3s：计数与熔断是这条用例要验的东西，
// 3 秒那条口径另有 TestHandleTimesOutAndNextEventStillServes 盯着。
func TestTimeoutBurstTripsBreaker(t *testing.T) {
	p, logs, pidDir := loadOne(t, modeSlow, nil)
	initPlugin(t, p, nil)
	t.Cleanup(func() { killByPid(t, pidDir) })

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := p.Handle(ctx, message("/slow", "hang"))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("第 %d 次应超时，实际 %v", i+1, err)
		}
	}

	if !logs.contains("已停用") {
		t.Fatalf("连败 5 次必须留下一条可读的「已停用」：\n%s", logs.text())
	}
	// 停用之后立刻失败：不能再让调用方白等一个超时。
	start := time.Now()
	_, err := p.Handle(context.Background(), message("/slow", "ok"))
	if !errors.Is(err, pluginproc.ErrUnavailable) {
		t.Fatalf("停用后的调用应立刻返回 ErrUnavailable，实际 %v", err)
	}
	if cost := time.Since(start); cost > 300*time.Millisecond {
		t.Fatalf("停用后还等了 %s 才失败", cost)
	}
	// 卡死的进程不能留着：停用等于不再有它的进程。
	if pid := readPid(t, pidDir, modeSlow); pid > 0 && !waitGone(t, pid, 3*time.Second) {
		t.Fatalf("停用后进程 %d 还在", pid)
	}
}

// 任务参数要原样过通道：per-peer 任务靠 spec.Peer 指定目标群，丢了就等于
// 每条实例都跑在"全局"上。
func TestRunJobForwardsSpec(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, _, pidDir := loadOne(t, modeEcho, nil)
	initPlugin(t, p, nil)
	t.Cleanup(func() { killByPid(t, pidDir) })

	jobber, ok := p.(host.JobRunner)
	if !ok {
		t.Fatal("proxy 必须实现 host.JobRunner")
	}
	if err := jobber.RunJob(context.Background(),
		host.JobSpec{Plugin: modeEcho, Name: "daily", Peer: "g-42"}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	// 插件把收到的东西写进 marker：reply 走不了这条路，只能这样验。
	line := "job=daily peer=g-42"
	if n := countLines(t, marker, line); n != 1 {
		t.Fatalf("插件没收到 %q（实际日志行见 marker 文件）", line)
	}
}
