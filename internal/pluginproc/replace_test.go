package pluginproc_test

// 热替换（/upgrade plugin 的进程侧）：只停一个插件、只换一个文件、只起一个
// 子进程。本套用例全部用真的子进程——要验的正是"进程换了没有、旧版本还能
// 不能服务"，接口替身把这些全绕过去了。

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginproc"
	"github.com/knight000/go_qqbot/pkg/host"
)

// replacer 是核心对插件做的那个可选类型断言的样子：只要方法名对上就算，
// 本包的测试也不 import 任何"声明了它"的包（结构类型，不是名义类型）。
type replacer interface {
	BinPath() string
	ReplaceBinary(newPath string) error
}

// loadPair 装两个插件并都起起来：一个待升级的 echo，一个旁观者 plain。
//
// 旁观者不是凑数：验收要的是"其他插件一点没动"——只验被升级的那个，
// 一个把全目录都重启了的实现照样能过。
func loadPair(t *testing.T) (echo, plain host.Plugin, dir, pidDir, marker string, logs *logSink) {
	t.Helper()
	dir = t.TempDir()
	install(t, dir, modeEcho)
	install(t, dir, modePlain)

	pidDir = t.TempDir()
	t.Setenv(pidEnv, pidDir)
	marker = filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	logs = newLogs()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		ProbeTimeout: probeForTest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("应发现 2 个插件，实际 %d（日志：\n%s）", len(plugins), logs.text())
	}
	t.Cleanup(func() { stopAll(t, plugins, pidDir) })

	byName := map[string]host.Plugin{}
	for _, p := range plugins {
		byName[p.Manifest().Name] = p
	}
	echo, plain = byName[modeEcho], byName[modePlain]
	if echo == nil || plain == nil {
		t.Fatalf("插件集合不对: %v", byName)
	}
	initPlugin(t, echo, nil)
	initPlugin(t, plain, nil)
	return echo, plain, dir, pidDir, marker, logs
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(blob)
}

// exeSuffix / oldPathOf 是测试对"文件叫什么"的独立期望：后缀随平台，
// 备份与插件同目录且点开头（点开头才会被启动扫描忽略）。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func oldPathOf(bin string) string {
	return filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+".old")
}

// 验收 1（进程侧）：只换被升级的那一个插件。
//
// 一次断言咬住五件事，缺一条都能让"看起来成功了"的实现漏过去：
//   - 被升级的插件换成了新版本（清单版本 + 新进程真的在服务）；
//   - 它的 pid 变了、旧进程确实退出了（不是"起了第二个进程"）；
//   - 旧进程走的是三级关闭的第一级（插件收到 close，能把状态写回 KV）；
//   - 旁观插件的 pid 与启动次数一动不动；
//   - 落位文件与备份都被清掉（否则下次启动会看见一个点文件之外的残渣）。
func TestReplaceBinarySwapsOnlyThatPlugin(t *testing.T) {
	echo, _, dir, pidDir, marker, _ := loadPair(t)
	r, ok := echo.(replacer)
	if !ok {
		t.Fatal("proxy 必须实现 BinPath / ReplaceBinary（结构与 upgrade.PluginReplacer 一致）")
	}

	bin := filepath.Join(dir, "echo"+exeSuffix())
	if got := r.BinPath(); got != bin {
		t.Fatalf("BinPath = %q want %q", got, bin)
	}
	oldPid := readPid(t, pidDir, modeEcho)
	plainPid := readPid(t, pidDir, modePlain)
	if oldPid == 0 || plainPid == 0 {
		t.Fatalf("插件没起来：echo=%d plain=%d", oldPid, plainPid)
	}

	staged := stageVersion(t, bin, modeEcho, "0.9.0", false)
	if err := r.ReplaceBinary(staged); err != nil {
		t.Fatalf("ReplaceBinary: %v", err)
	}

	// 1. 版本变了：清单是核心判断"装的是哪一版"的唯一来源。
	if got := echo.Manifest().Version; got != "0.9.0" {
		t.Fatalf("换完之后清单版本 = %q want 0.9.0", got)
	}
	// 2. 进程换了，旧进程真的退出了。
	newPid := readPid(t, pidDir, modeEcho)
	if newPid == 0 || newPid == oldPid {
		t.Fatalf("被升级插件的 pid 没变：old=%d new=%d", oldPid, newPid)
	}
	if !waitGone(t, oldPid, 5*time.Second) {
		t.Errorf("旧进程 %d 还在", oldPid)
	}
	// 3. 关闭走的是第一级（插件自己收摊），不是一刀 Kill。
	if n := countLines(t, marker, "close="+modeEcho); n != 1 {
		t.Errorf("旧进程收到 close 的次数 = %d，期望 1（日志：\n%s)", n, readFile(t, marker))
	}
	// 4. 新进程接了活：Handle 打到的必须是 0.9.0 那一代。
	reply, err := echo.Handle(context.Background(), message("/echo", "升级后"))
	if err != nil {
		t.Fatalf("升级后的 Handle: %v", err)
	}
	if reply == nil || reply.Content != "升级后" {
		t.Fatalf("升级后的回复 = %#v", reply)
	}
	// 5. 旁观者一点没动：pid 不变、启动次数仍是 1。
	if got := readPid(t, pidDir, modePlain); got != plainPid {
		t.Errorf("旁观插件的 pid 变了：%d → %d", plainPid, got)
	}
	if n := countLines(t, marker, "init="+modePlain); n != 1 {
		t.Errorf("旁观插件被启动了 %d 次，期望 1", n)
	}
	// 被升级的插件则是两代（旧的 + 新的）。
	if n := countLines(t, marker, "init="+modeEcho); n != 2 {
		t.Errorf("被升级插件启动次数 = %d，期望 2", n)
	}
	// 6. 残渣清干净：落位文件与备份都不留。
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("落位文件应已被 rename 走: %v", err)
	}
	if _, err := os.Stat(oldPathOf(bin)); !os.IsNotExist(err) {
		t.Errorf("成功之后应删掉备份 %s: %v", oldPathOf(bin), err)
	}
	// 7. 装上的是新内容，不是"文件没换、只重启了进程"。
	if got := readFile(t, bin); !strings.Contains(got, markVersion+":0.9.0") {
		t.Error("插件文件没有换成新产物")
	}
}

// 验收 2（进程侧）：新版本握手失败 → 自动回滚，旧版本继续服务。
//
// 断言三件事：返回的错误能读懂、磁盘上换回了旧文件、旧版本真的又在服务。
// 只断言"返回了 error"是不够的——那样一个"把插件搞没了"的实现也能过。
func TestReplaceBinaryRollsBackWhenHandshakeFails(t *testing.T) {
	echo, _, dir, _, marker, logs := loadPair(t)
	r := echo.(replacer)
	bin := filepath.Join(dir, "echo"+exeSuffix())
	before := readFile(t, bin)

	staged := stageVersion(t, bin, modeEcho, "0.9.0", true)
	err := r.ReplaceBinary(staged)
	if err == nil {
		t.Fatal("新版本握手失败时必须返回错误")
	}
	// 原因要能指向插件自己说的话，而不是"exit status 1"。
	if !strings.Contains(err.Error(), "配置里的 token 为空") {
		t.Errorf("错误里应带上插件的原话: %v", err)
	}

	if got := echo.Manifest().Version; got != "0.0.0" {
		t.Errorf("回滚之后清单版本 = %q，期望还是旧的 0.0.0", got)
	}
	if got := readFile(t, bin); got != before {
		t.Error("回滚之后磁盘上的插件必须是旧产物（逐字节相等）")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("失败之后不该留下新产物: %v", err)
	}
	if _, err := os.Stat(oldPathOf(bin)); !os.IsNotExist(err) {
		t.Errorf("回滚之后不该留下备份: %v", err)
	}

	// 旧版本继续服务：这是整条回滚的意义。
	reply, err := echo.Handle(context.Background(), message("/echo", "回滚之后"))
	if err != nil {
		t.Fatalf("回滚后 Handle: %v", err)
	}
	if reply == nil || reply.Content != "回滚之后" {
		t.Fatalf("回滚后回复 = %#v", reply)
	}
	// 旧版本被重新拉起（第二代），版本仍是旧的。
	if n := countLines(t, marker, "init="+modeEcho+" v0.0.0"); n != 2 {
		t.Errorf("旧版本启动次数 = %d，期望 2（回滚要重启它）:\n%s", n, readFile(t, marker))
	}
	if n := countLines(t, marker, "init="+modeEcho+" v0.9.0"); n != 0 {
		t.Errorf("失败的新版本不该完成过 Init，却有 %d 条记录", n)
	}
	// 现场要留一条可读的 Warn：运维看到的是"升级没成"，不是"插件死了"。
	if !logs.contains("插件热升级失败") {
		t.Errorf("日志里应有回滚说明:\n%s", logs.text())
	}
}

// 换上一个自称别的插件的产物是升级路径上最坏的一种"成功"：文件换了、
// 进程起来了、路由却指向一个不再存在的插件。必须在不碰任何状态之前拒绝。
func TestReplaceBinaryRejectsWrongIdentity(t *testing.T) {
	echo, _, dir, pidDir, _, _ := loadPair(t)
	r := echo.(replacer)
	bin := filepath.Join(dir, "echo"+exeSuffix())
	before := readFile(t, bin)
	oldPid := readPid(t, pidDir, modeEcho)

	// 把 plain 的产物当成 echo 的新版本放进落位路径。
	//
	// 复制时补一行角色标记：本套件靠文件名决定角色，而这里要验的正是
	// "产物自称谁"——文件名不算数，清单里的名字才算。
	plainBin := filepath.Join(dir, "plain"+exeSuffix())
	staged := filepath.Join(dir, "."+filepath.Base(bin)+".new")
	blob := readFile(t, plainBin) + "\n" + markRole + ":" + modePlain + "\n"
	if err := os.WriteFile(staged, []byte(blob), 0o755); err != nil {
		t.Fatal(err)
	}

	err := r.ReplaceBinary(staged)
	if err == nil {
		t.Fatal("产物自称 plain 时不该换上去")
	}
	if !strings.Contains(err.Error(), modePlain) {
		t.Errorf("错误应指出产物自称谁: %v", err)
	}
	if got := readFile(t, bin); got != before {
		t.Error("被拒绝时磁盘上的插件不该被改动")
	}
	if got := readPid(t, pidDir, modeEcho); got != oldPid {
		t.Errorf("被拒绝时不该动正在跑的进程：%d → %d", oldPid, got)
	}
	if _, err := echo.Handle(context.Background(), message("/echo", "还在")); err != nil {
		t.Errorf("被拒绝之后旧进程必须还能服务: %v", err)
	}
}

// 关停中不能换盘：那一刻 cur 已经交给三级关闭，换上一个新进程只会留下
// 一个没人管的孤儿。
func TestReplaceBinaryRefusesWhileClosing(t *testing.T) {
	echo, _, dir, _, _, _ := loadPair(t)
	r := echo.(replacer)
	bin := filepath.Join(dir, "echo"+exeSuffix())
	staged := stageVersion(t, bin, modeEcho, "0.9.0", false)

	if err := echo.(host.Closer).Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := r.ReplaceBinary(staged)
	if err == nil || !strings.Contains(err.Error(), "关停") {
		t.Fatalf("关停中应拒绝替换，实际: %v", err)
	}
}
