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
		Dir:          dir,
		Log:          logs.logger(),
		ProbeTimeout: probeForTest,
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
	t.Cleanup(func() { stopAll(t, plugins, pidDir) })
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
//
// Kill 之后必须 Wait：Windows 上 os.Process.Release 只是把 handle 字段丢掉、
// 并不 CloseHandle（Go 的 exec_windows.go 里它是 NOOP），只要那个句柄还在，
// 被杀进程的 exe 映像就一直被映射着——文件删不掉，t.TempDir 清理会报
// Access is denied，看起来像"留了孤儿"，其实是清理自己没撒手。Wait 会在
// 等进程退出之后关掉句柄。Unix 上 Wait 对非子进程返回 ECHILD，无害。
func killByPid(t *testing.T, pidDir string) {
	t.Helper()
	entries, err := os.ReadDir(pidDir)
	if err != nil {
		return
	}
	var pids []int
	for _, e := range entries {
		if pid := readPid(t, pidDir, strings.TrimSuffix(e.Name(), ".pid")); pid > 0 {
			pids = append(pids, pid)
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
				_, _ = proc.Wait()
			}
		}
	}
	for _, pid := range pids {
		if !waitGone(t, pid, 5*time.Second) {
			t.Logf("清理时进程 %d 仍未退出", pid)
		}
	}
}

// stopAll 是每个用例结尾的兜底清理，顺序不能反过来：先走生产那条关停路径，
// 再按 pid 补刀。
//
// 为什么不直接 killByPid：proxy 会把"进程被外部杀掉"当成一次崩溃，退避一
// 到期就重启一个新进程——用默认退避是 1 分钟后（看不见），本套用例把退避
// 压到了 10ms，清理就会和重启赛跑：杀掉的进程刚放掉 exe，新进程又把它映射
// 回去，t.TempDir 的清理永远删不掉那个文件。Close 会置 closing，监工就不再
// 重启，这才是"关停"的定义。
func stopAll(t *testing.T, plugins []host.Plugin, pidDir string) {
	t.Helper()
	for _, p := range plugins {
		if c, ok := p.(host.Closer); ok {
			_ = c.Close(context.Background())
		}
	}
	killByPid(t, pidDir)
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

	p, logs, _ := loadOne(t, modeEcho, nil)
	initPlugin(t, p, map[string]any{"greeting": "你好"})

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
	p, _, _ := loadOne(t, modeSlow, nil)
	initPlugin(t, p, nil)

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
	p, _, _ := loadOne(t, modeSlow, nil)
	initPlugin(t, p, nil)

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

// 崩溃策略（决策 8）：非零退出按 1/2/4/8/16 分钟退避重启，连败 5 次停用并
// 留一条可读的「已停用」，而核心继续跑——其它插件照常服务。
//
// 退避表在这里注入成 10ms：1 分钟是给人看的节奏，不是这条用例要验的东西；
// 要验的是"每次崩溃都算一次失败、第 5 次停用、别的插件不受影响"。
func TestCrashBacksOffThenDisables(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, modeCrashy)
	install(t, dir, modeEcho)

	pidDir := t.TempDir()
	t.Setenv(pidEnv, pidDir)

	logs := newLogs()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		Backoff:      []time.Duration{10 * time.Millisecond},
		ProbeTimeout: probeForTest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("应发现 2 个插件，实际 %d（日志：\n%s）", len(plugins), logs.text())
	}
	t.Cleanup(func() { stopAll(t, plugins, pidDir) })
	var crashy, echo host.Plugin
	for _, p := range plugins {
		switch p.Manifest().Name {
		case modeCrashy:
			crashy = p
		case modeEcho:
			echo = p
		}
	}

	// 启动即崩的插件不能让 Init 失败（否则整台机器人起不来，还会进
	// systemd 重启循环白烧 Identify 配额，决策 6）。
	if err := crashy.Init(context.Background(), testHost(modeCrashy, nil)); err != nil {
		t.Fatalf("启动即崩不该让 Init 失败: %v", err)
	}
	if err := echo.Init(context.Background(), testHost(modeEcho, nil)); err != nil {
		t.Fatalf("正常插件 Init: %v", err)
	}

	if !waitUntil(5*time.Second, func() bool { return logs.contains("已停用") }) {
		t.Fatalf("连败 5 次没有停用：\n%s", logs.text())
	}
	if !logs.contains("plugin=" + modeCrashy + " ") {
		t.Fatalf("停用日志没说是哪个插件：\n%s", logs.text())
	}
	// 停用之后立即失败，不再白起进程。
	if _, err := crashy.Handle(context.Background(), message("/echo", "x")); !errors.Is(err, pluginproc.ErrUnavailable) {
		t.Fatalf("停用后应返回 ErrUnavailable，实际 %v", err)
	}
	// 其它插件完全不受影响：崩溃的隔离收益就在这里。
	reply, err := echo.Handle(context.Background(), message("/echo", "还在"))
	if err != nil || reply == nil || reply.Content != "还在" {
		t.Fatalf("另一个插件应该照常工作，实际 reply=%#v err=%v", reply, err)
	}
}

// 插件自己拒绝初始化（比如配置里的密钥不对）属于配置写错，与"产物崩溃"是
// 两件事：前者按既有口径 fail-fast，后者才走退避重启（决策 6 的对照）。
func TestInitRejectedByPluginIsFatal(t *testing.T) {
	p, _, _ := loadOne(t, modeBadInit, nil)

	err := p.Init(context.Background(), testHost(modeBadInit, nil))
	if err == nil {
		t.Fatal("插件在 Init 里报错应当让启动失败，而不是被当成崩溃吞掉")
	}
	if !strings.Contains(err.Error(), "配置") {
		t.Fatalf("错误应保留插件的原文，实际 %v", err)
	}
}

// waitUntil 轮询到条件成立。
func waitUntil(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ok()
}

// 关停三级（决策 8 的代价一节）：先请插件自己收摊（plugin.close），再断连接
// 让它读到 EOF 退出，最后才是信号。顺序不能反——先杀进程会让插件的 Close
// 永远跑不到，而很多插件要靠它把内存里的状态写回 KV。
//
// 关完不能留孤儿进程：这是本票的硬验收（Windows 上任务管理器里也不许剩）。
func TestCloseRunsPluginHookBeforeTerminating(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, _, pidDir := loadOne(t, modeEcho, nil)
	initPlugin(t, p, nil)

	closer, ok := p.(host.Closer)
	if !ok {
		t.Fatal("proxy 必须实现 host.Closer：核心的关停只认这个接口")
	}
	pid := readPid(t, pidDir, modeEcho)
	if pid == 0 {
		t.Fatal("没拿到插件进程的 pid")
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 1. 第一级真的发生了：插件的 Close 写了 marker。
	if n := countLines(t, marker, "close="+modeEcho); n != 1 {
		t.Fatalf("插件 Close 被调用 %d 次，期望 1（先杀进程就一次都不会有）", n)
	}
	// 2. 进程真的没了。
	if !waitGone(t, pid, 3*time.Second) {
		t.Fatalf("关停后进程 %d 还在：留了孤儿", pid)
	}
	// 3. 幂等：关停路径可能被调两次（/reload 与信号竞争），第二次不能报错。
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("第二次 Close 应无操作地成功，实际 %v", err)
	}
	// 4. 关停之后不再有"可投递的进程"。
	if _, err := p.Handle(context.Background(), message("/echo", "x")); !errors.Is(err, pluginproc.ErrUnavailable) {
		t.Fatalf("关停后 Handle 应返回 ErrUnavailable，实际 %v", err)
	}
}

// 插件不理会 close、也不理会 EOF（半死的现场）：三级关闭必须还能把它收掉，
// 否则每次 /reload 都留一个进程，几次之后机器上全是它。
func TestCloseEscalatesWhenPluginIgnoresCloseAndEOF(t *testing.T) {
	p, logs, pidDir := loadOne(t, modeStubborn, nil)
	initPlugin(t, p, nil)

	pid := readPid(t, pidDir, modeStubborn)
	if pid == 0 {
		t.Fatal("没拿到插件进程的 pid")
	}
	start := time.Now()
	if err := p.(host.Closer).Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !waitGone(t, pid, time.Second) {
		t.Fatalf("插件无视 close 与 EOF 时，最后一级 Kill 也没收掉进程 %d", pid)
	}
	// 每一级都有期限，所以整条路径是有上界的（不是"一直等插件自己良心发现"）。
	if cost := time.Since(start); cost > 8*time.Second {
		t.Fatalf("三级关闭用了 %s，期限没起作用（日志：\n%s）", cost, logs.text())
	}
}

// 没实现可选接口的插件：Start / Close 是空操作，不是"方法未注册"的错误。
// 这条靠握手时报上来的 optional 判断——去猜（先调一次、失败就当没有）会把
// 真正的故障（插件启动即崩）和"本来就没实现"混成同一个现象。
func TestOptionalMethodsAreSkippedWhenNotImplemented(t *testing.T) {
	p, _, _ := loadOne(t, modePlain, nil)
	initPlugin(t, p, nil)

	if err := p.(host.Starter).Start(context.Background()); err != nil {
		t.Fatalf("插件没实现 Start 时应当是空操作，实际 %v", err)
	}
	err := p.(host.JobRunner).RunJob(context.Background(), host.JobSpec{Plugin: modePlain, Name: "n"})
	if !errors.Is(err, rpc.ErrMethodNotFound) {
		t.Fatalf("插件没实现 RunJob 时应当如实报方法未注册，实际 %v", err)
	}
	if err := p.(host.Closer).Close(context.Background()); err != nil {
		t.Fatalf("插件没实现 Close 时关停仍要收掉进程，实际 %v", err)
	}
}

// Start 在实现了 Starter 的插件上必须真的转发过去（核心在 Init 之后统一调它）。
func TestStartForwardedToPlugin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, _, _ := loadOne(t, modeEcho, nil)
	initPlugin(t, p, nil)

	if err := p.(host.Starter).Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := countLines(t, marker, "start="+modeEcho); n != 1 {
		t.Fatalf("插件 Start 被调用 %d 次，期望 1", n)
	}
}

// 任务参数要原样过通道：per-peer 任务靠 spec.Peer 指定目标群，丢了就等于
// 每条实例都跑在"全局"上。
func TestRunJobForwardsSpec(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	p, _, _ := loadOne(t, modeEcho, nil)
	initPlugin(t, p, nil)

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
