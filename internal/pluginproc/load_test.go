package pluginproc_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginproc"
)

// —— 日志捕获 ——
//
// 探测与熔断的结论**只**出现在日志里（"跳过这个插件""已停用"），所以日志
// 就是这些能力的可观察面；断言它比断言内部字段更接近真实用户看到的东西。

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *logSink) write(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.WriteString(line)
	s.b.WriteString("\n")
}

func (s *logSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// contains 逐行包含判断：断言"某一行里有这段话"，避免跨行误命中。
func (s *logSink) contains(sub string) bool {
	for _, line := range strings.Split(s.text(), "\n") {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

type capturing struct {
	sink  *logSink
	attrs []slog.Attr
}

func newLogs() *logSink { return &logSink{} }

func (l *logSink) logger() *slog.Logger { return slog.New(&capturing{sink: l}) }

func (c *capturing) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturing) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteString(" ")
	b.WriteString(r.Message)
	for _, a := range c.attrs {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	c.sink.write(b.String())
	return nil
}

func (c *capturing) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(c.attrs)+len(attrs))
	merged = append(merged, c.attrs...)
	merged = append(merged, attrs...)
	return &capturing{sink: c.sink, attrs: merged}
}

func (c *capturing) WithGroup(string) slog.Handler { return c }

// —— 工具 ——

// countLines 数出满足前缀的行数：探测次数这类"只该发生一次"的事实用它断言。
func countLines(t *testing.T, path, prefix string) int {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("读标记文件: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// 探测一次、缓存住：`Manifest()` 在装配后会被核心反复调用（路由、日志、
// 报告），每次都起进程既慢又把"清单是什么"变成两次独立事实。
func TestLoadCachesManifestFromSingleProbe(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, modeEcho)
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(markerEnv, marker)

	logs := newLogs()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		ProbeTimeout: probeForTest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("应发现 1 个插件，实际 %d（日志：\n%s）", len(plugins), logs.text())
	}
	if got := plugins[0].Manifest().Name; got != modeEcho {
		t.Fatalf("Manifest().Name = %q，期望 %q", got, modeEcho)
	}
	// 再读两次：缓存值不能每次都起进程重新探测。
	_ = plugins[0].Manifest()
	_ = plugins[0].Manifest()
	if n := countLines(t, marker, "probe="); n != 1 {
		t.Fatalf("探测次数 = %d，期望 1（Manifest() 必须返回装配期缓存值）", n)
	}
}

// 清单不合法的插件被跳过、记一条 Error，但 Load 本身不报错——否则一个过期
// 产物就能让整台机器人起不来（ADR-0005 决策 6）。
//
// 三个子场景分别咬三条不同的校验：api_version 不符、核心保留前缀、清单不是
// JSON。第三条最容易被当成"启动失败"糊过去，而现场表现会是"某个指令没反应"。
func TestLoadSkipsInvalidManifests(t *testing.T) {
	cases := []struct {
		mode string
		want string // 日志里必须出现的片段
	}{
		{modeBadAPI, "api_version"},
		{modeReserved, "/reset"},
		{modeGarbage, "清单不是合法 JSON"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			dir := t.TempDir()
			install(t, dir, c.mode)

			logs := newLogs()
			plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
				Dir:          dir,
				Log:          logs.logger(),
				ProbeTimeout: probeForTest,
			})
			if err != nil {
				t.Fatalf("单个插件坏掉不该让 Load 失败: %v", err)
			}
			if len(plugins) != 0 {
				t.Fatalf("坏清单的插件应被跳过，实际返回 %d 个", len(plugins))
			}
			if !logs.contains(c.want) {
				t.Fatalf("日志里没有 %q，现场就没有任何线索：\n%s", c.want, logs.text())
			}
			if !logs.contains(c.mode) {
				t.Fatalf("日志没指出是哪个插件：\n%s", logs.text())
			}
		})
	}
}

// 探测卡死必须被到点掐断：否则一个坏文件就让整个启动停在那里（2 秒 ×
// 串行还会翻倍）。这里同时断言它没留下探测用的孤儿进程——探测进程也是
// 子进程，杀不干净就是"任务管理器里一堆 exe"的来源之一。
func TestLoadKillsProbeThatHangsPastTimeout(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, modeHangManifest)
	install(t, dir, modeEcho) // 一个正常插件：坏的那个不该连累它

	pidDir := t.TempDir()
	t.Setenv(pidEnv, pidDir)
	t.Cleanup(func() { killByPid(t, pidDir) })

	logs := newLogs()
	start := time.Now()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		ProbeTimeout: time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("探测超时不该让 Load 失败: %v", err)
	}
	// 默认 2 秒换成 1 秒之后，整轮必须远快于"等满再排队"——并行探测的
	// 意义就在这里：坏文件只花一份超时，而不是每个插件各花一份。
	if elapsed > 3*time.Second {
		t.Fatalf("Load 用了 %s，超时的插件把启动拖住了", elapsed)
	}
	if len(plugins) != 1 || plugins[0].Manifest().Name != modeEcho {
		t.Fatalf("应只剩正常插件，实际 %d 个（日志：\n%s）", len(plugins), logs.text())
	}
	if !logs.contains("没有应答") {
		t.Fatalf("超时没有留下可读的日志：\n%s", logs.text())
	}
	pid := readPid(t, pidDir, modeHangManifest)
	if pid == 0 {
		t.Fatalf("卡死的探测进程没留下 pid：\n%s", logs.text())
	}
	if !waitGone(t, pid, 2*time.Second) {
		t.Fatalf("卡死的探测进程 %d 还活着：探测超时没有真的掐断它", pid)
	}
}

// [plugins].disabled 是"装而不跑"（决策 13）：名字在这里的插件不进核心，
// 但其它插件照常加载。
func TestLoadSkipsDisabledPlugin(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, modeEcho)
	install(t, dir, modeSlow)

	logs := newLogs()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		Disabled:     []string{modeEcho},
		ProbeTimeout: probeForTest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Manifest().Name != modeSlow {
		t.Fatalf("应只加载未被停用的插件，实际 %d 个（日志：\n%s）", len(plugins), logs.text())
	}
	if !logs.contains("disabled") || !logs.contains(modeEcho) {
		t.Fatalf("停用没有留下可读的日志：\n%s", logs.text())
	}
}

// 两个文件自称同一个插件名（升级残留一个旧产物）时只留一个：都装上会有两个
// 进程抢同一条路由，表现为同一条消息回两遍，而日志里两个插件都"正常"。
func TestLoadDropsDuplicateManifestName(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, modeEcho)
	install(t, dir, modeAlias) // 自称 echo

	logs := newLogs()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		Log:          logs.logger(),
		ProbeTimeout: probeForTest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("同名插件应只留一个，实际 %d 个（日志：\n%s）", len(plugins), logs.text())
	}
	if !logs.contains("重名") {
		t.Fatalf("重名没有留下可读的日志：\n%s", logs.text())
	}
}
