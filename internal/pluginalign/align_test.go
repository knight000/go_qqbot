package pluginalign

// 这一组用例只观察「对齐之后磁盘上是什么」与「日志怎么解释」：替换的实现
// （临时文件 + rename）是手段，判定矩阵才是契约。探测用注入的替身——真正的
// `--manifest` 协议由 internal/pluginproc 自己的用例盯着，这里不重复起进程。

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/knight000/go_qqbot/pkg/host"
)

const apiVersion = "12"

func testSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func fileName(name string) string { return name + testSuffix() }

// bundleOf 造一份内嵌副本：name → 文件内容，另附版本清单。
func bundleOf(t *testing.T, files map[string]string, versions map[string]string) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	for name, content := range files {
		m[fileName(name)] = &fstest.MapFile{Data: []byte(content)}
	}
	var b strings.Builder
	for _, name := range sortedKeys(versions) {
		b.WriteString(name + "=" + versions[name] + "\n")
	}
	m["_versions"] = &fstest.MapFile{Data: []byte(b.String())}
	return m
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// probeOf 按文件名返回预设的清单。没预设的名字返回错误：那代表"这个文件
// 不是插件"，对齐应当只 warn 不动它。
func probeOf(manifests map[string]host.Manifest) func(context.Context, string) (host.Manifest, error) {
	return func(_ context.Context, bin string) (host.Manifest, error) {
		m, ok := manifests[filepath.Base(bin)]
		if !ok {
			return host.Manifest{}, errors.New("不是插件")
		}
		return m, nil
	}
}

func manifest(name, version, api string) host.Manifest {
	return host.Manifest{Name: name, Version: version, APIVersion: api}
}

// —— 日志捕获 ——

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *logSink) logger() *slog.Logger { return slog.New(&capture{sink: s}) }

func (s *logSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *logSink) contains(sub string) bool {
	for _, line := range strings.Split(s.text(), "\n") {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

type capture struct{ sink *logSink }

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteString(" ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	c.sink.b.WriteString(b.String())
	c.sink.b.WriteString("\n")
	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

// —— 工具 ——

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, fileName(name))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

func names(list []string) string { return strings.Join(list, ",") }

// 内嵌版本更高 → 换成内嵌的字节。这是「换核心即升插件」的唯一机制。
func TestAlignReplacesWhenEmbeddedIsNewer(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "echo", "echo-old")
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-new"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("echo"): manifest("echo", "0.1.0", apiVersion)}),
	})

	if names(rep.Replaced) != "echo" {
		t.Fatalf("应替换 echo，报告 %+v", rep)
	}
	if got := readFile(t, target); got != "echo-new" {
		t.Errorf("替换后内容 = %q，期望 echo-new", got)
	}
	if !logs.contains("替换") || !logs.contains("0.1.0") || !logs.contains("0.2.0") {
		t.Errorf("日志应写明版本变化，实际:\n%s", logs.text())
	}
	// 临时文件必须清干净：点开头虽不会被当成插件，但残留会越积越多。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("替换后留下了临时文件 %s", e.Name())
		}
	}
}

// 只升不降：用户单独升过的插件保持不动，日志给一条说明（ADR-0006 决策 7）。
func TestAlignSkipsWhenDiskIsNewer(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "echo", "echo-user")
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-bundled"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("echo"): manifest("echo", "0.3.0", apiVersion)}),
	})

	if len(rep.Replaced) != 0 || len(rep.Added) != 0 {
		t.Fatalf("磁盘版本更高时不该动文件，报告 %+v", rep)
	}
	if names(rep.Skipped) != "echo" {
		t.Errorf("应记为跳过，报告 %+v", rep)
	}
	if got := readFile(t, target); got != "echo-user" {
		t.Errorf("用户单独升过的插件被改成了 %q", got)
	}
	if !logs.contains("更高") {
		t.Errorf("日志应说明为什么跳过，实际:\n%s", logs.text())
	}
}

// api_version 不匹配的插件根本加载不了，必须换——哪怕它的版本号更高。
// 契约涨版本而插件版本号没动时，只看版本号会永远不换，然后握手处全崩。
func TestAlignReplacesOnAPIVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "echo", "echo-stale-api")
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-fresh"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("echo"): manifest("echo", "0.9.0", "11")}),
	})

	if names(rep.Replaced) != "echo" {
		t.Fatalf("api_version 不匹配时应替换，报告 %+v", rep)
	}
	if got := readFile(t, target); got != "echo-fresh" {
		t.Errorf("替换后内容 = %q，期望 echo-fresh", got)
	}
	if !logs.contains("api_version") {
		t.Errorf("日志应写明是因为契约版本不匹配，实际:\n%s", logs.text())
	}
}

// 删掉插件后启动：自动从内嵌副本补回，不需要 init --plugins。
func TestAlignRestoresMissingFile(t *testing.T) {
	dir := t.TempDir()
	kept := writeFile(t, dir, "md", "md-user")

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-new", "md": "md-new"}, map[string]string{"echo": "0.2.0", "md": "0.3.0"}),
		Versions:   map[string]string{"echo": "0.2.0", "md": "0.3.0"},
		APIVersion: apiVersion,
		Log:        (&logSink{}).logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("md"): manifest("md", "0.3.0", apiVersion)}),
	})

	if names(rep.Added) != "echo" {
		t.Fatalf("应补回 echo，报告 %+v", rep)
	}
	if got := readFile(t, filepath.Join(dir, fileName("echo"))); got != "echo-new" {
		t.Errorf("补回的 echo 内容 = %q", got)
	}
	if got := readFile(t, kept); got != "md-user" {
		t.Errorf("同版本且存在的 md 不该被动，实际 %q", got)
	}
}

// 整个插件目录都不在（首次运行、没跑过 init --plugins）：全部补回。
func TestAlignRestoresAllWhenDirMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", "bin")

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "e", "md": "m"}, map[string]string{"echo": "0.1.0", "md": "0.1.0"}),
		Versions:   map[string]string{"echo": "0.1.0", "md": "0.1.0"},
		APIVersion: apiVersion,
		Log:        (&logSink{}).logger(),
	})

	if names(rep.Added) != "echo,md" {
		t.Fatalf("应补回全部出厂插件，报告 %+v", rep)
	}
	for _, name := range []string{"echo", "md"} {
		if _, err := os.Stat(filepath.Join(dir, fileName(name))); err != nil {
			t.Errorf("%s 未被补回: %v", name, err)
		}
	}
}

// 用户自己的插件（不在内嵌清单里）不归对齐管：不看、不动、不探测。
//
// 出厂插件都在场时一个多余文件都不该探：探测要起进程，一个卡死的第三方插件
// 会把整轮对齐拖到超时。改名兜底只在"有出厂插件没对上"时才启用。
func TestAlignIgnoresUnknownPlugins(t *testing.T) {
	dir := t.TempDir()
	custom := writeFile(t, dir, "custom", "custom-bytes")
	writeFile(t, dir, "echo", "echo-user")
	probed := false

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-new"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        (&logSink{}).logger(),
		Probe: func(_ context.Context, bin string) (host.Manifest, error) {
			switch filepath.Base(bin) {
			case fileName("custom"):
				probed = true
				return manifest("custom", "9.9.9", apiVersion), nil
			case fileName("echo"):
				return manifest("echo", "0.2.0", apiVersion), nil
			}
			return host.Manifest{}, errors.New("不是插件")
		},
	})

	if got := readFile(t, custom); got != "custom-bytes" {
		t.Errorf("用户插件被改动了：%q", got)
	}
	if len(rep.Replaced) != 0 || len(rep.Skipped) != 0 || len(rep.Added) != 0 {
		t.Errorf("用户插件不该进报告，实际 %+v", rep)
	}
	if probed {
		t.Error("清单里没有的插件不该被探测：多起一次进程只是白等，还可能被卡死拖住")
	}
}

// 出厂插件被改了名：认身份靠 Manifest.Name，不靠文件名。
//
// 只按文件名探测会判定 echo 缺失，于是补一份 echo.exe——两个文件自称同一个
// 插件，每次启动刷一条「插件重名」Error。改名后的文件应当被当成 echo 本身。
func TestAlignRecognizesRenamedPlugin(t *testing.T) {
	dir := t.TempDir()
	renamed := writeFile(t, dir, "myecho", "echo-user")
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-new"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("myecho"): manifest("echo", "0.3.0", apiVersion)}),
	})

	if names(rep.Added) != "" {
		t.Fatalf("改名后的插件已被认出，不该补第二份，报告 %+v", rep)
	}
	if names(rep.Skipped) != "echo" {
		t.Errorf("应把 myecho 当成 echo 并按只升不降跳过，报告 %+v", rep)
	}
	if got := readFile(t, renamed); got != "echo-user" {
		t.Errorf("改名后的文件被改成了 %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("目录里应只有改名后的那一份，实际 %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName("echo"))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("不该补回 %s：两份文件自称同一个插件", fileName("echo"))
	}
}

// 替换失败只 warn，不阻断启动，也不影响其它插件。
func TestAlignFailureOnlyWarnsAndContinues(t *testing.T) {
	dir := t.TempDir()
	// 用目录占住目标文件名：rename 一个文件到目录上必然失败。
	if err := os.Mkdir(filepath.Join(dir, fileName("echo")), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "e", "md": "m"}, map[string]string{"echo": "0.1.0", "md": "0.1.0"}),
		Versions:   map[string]string{"echo": "0.1.0", "md": "0.1.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
	})

	if names(rep.Failed) != "echo" {
		t.Fatalf("echo 的替换应失败，报告 %+v", rep)
	}
	if names(rep.Added) != "md" {
		t.Errorf("一个失败不该影响其它插件，报告 %+v", rep)
	}
	if !logs.contains("WARN") {
		t.Errorf("失败必须留一条 warn，实际:\n%s", logs.text())
	}
	if _, err := os.Stat(filepath.Join(dir, fileName("md"))); err != nil {
		t.Errorf("md 未被补回: %v", err)
	}
}

// 版本号无法比较（dev、预发布）时不猜：保留磁盘那份，日志说明。
func TestAlignSkipsUnparsableVersion(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "echo", "echo-dev")
	logs := &logSink{}

	rep := Align(context.Background(), Options{
		Dir:        dir,
		Bundle:     bundleOf(t, map[string]string{"echo": "echo-new"}, map[string]string{"echo": "0.2.0"}),
		Versions:   map[string]string{"echo": "0.2.0"},
		APIVersion: apiVersion,
		Log:        logs.logger(),
		Probe:      probeOf(map[string]host.Manifest{fileName("echo"): manifest("echo", "dev", apiVersion)}),
	})

	if len(rep.Replaced) != 0 {
		t.Fatalf("版本号无法比较时不该替换，报告 %+v", rep)
	}
	if got := readFile(t, target); got != "echo-dev" {
		t.Errorf("文件被改成了 %q", got)
	}
	if !logs.contains("无法比较") {
		t.Errorf("日志应说明版本号无法比较，实际:\n%s", logs.text())
	}
}

// 没有版本清单（本地没编产物、或旧格式）时整轮对齐不做事，连目录都不建。
func TestAlignNoVersionsIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", "bin")
	probed := false

	rep := Align(context.Background(), Options{
		Dir: dir,
		Probe: func(context.Context, string) (host.Manifest, error) {
			probed = true
			return host.Manifest{}, nil
		},
	})

	if len(rep.Added)+len(rep.Replaced)+len(rep.Skipped)+len(rep.Failed) != 0 {
		t.Errorf("没有版本清单时不该做任何事，报告 %+v", rep)
	}
	if probed {
		t.Error("没有版本清单时不该探测任何文件")
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("没有版本清单时不该创建插件目录 %s", dir)
	}
}
