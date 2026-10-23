package pluginproc_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
		Dir: dir,
		Log: logs.logger(),
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
