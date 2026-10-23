package pluginbundle_test

// 这一组用例要的是**真产物**：内嵌副本里真的躺着四个插件可执行文件，
// Extract 到临时目录后 pluginproc.Load 能把它们当成装机插件发现。
// 单元测试用 fstest.MapFS 观察释放语义（extract_test.go），这里观察的是
// 「发行二进制里的那份东西到底能不能用」——两者互为补充。
//
// 本平台没有出厂副本（或本地没跑 go run ./tools/build plugins）时跳过：
// 那是合法的构建状态（ADR-0005 决策 11 的代价），不是失败。

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginbundle"
	"github.com/knight000/go_qqbot/internal/pluginproc"
)

// wantNames 是四个示例插件在本平台的产物文件名。
// Windows 的内嵌副本带 .exe，名字就是带后缀的那个——文件名只是标签，
// 真正的名字由 --manifest 决定（pluginproc 这么认，这里也这么认）。
func wantNames() []string {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	return []string{
		"adminping" + suffix,
		"bootnotice" + suffix,
		"echo" + suffix,
		"md" + suffix,
	}
}

func requireBundle(t *testing.T) {
	t.Helper()
	if len(pluginbundle.Names()) == 0 {
		t.Skip("本平台没有出厂插件，或没跑 go run ./tools/build plugins：跳过着装期验证")
	}
}

func TestEmbeddedBundleIsTheFourExamplePlugins(t *testing.T) {
	requireBundle(t)

	got := strings.Join(pluginbundle.Names(), ",")
	want := strings.Join(wantNames(), ",")
	if got != want {
		t.Fatalf("内嵌插件清单 = %s，期望 %s", got, want)
	}
}

// 释放 → 装载：四个插件都要被 pluginproc 发现、校验通过。
// 这是 ADR-0005 决策 10/11 的端到端验证：产物与探测协议对得上。
func TestExtractThenLoadFindsFourPlugins(t *testing.T) {
	requireBundle(t)

	dst := t.TempDir()
	if err := pluginbundle.Extract(dst); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	names := loadNames(t, dst)
	if len(names) != 4 {
		t.Fatalf("装载到 %d 个插件 %v，期望 4 个", len(names), names)
	}
	for _, want := range []string{"adminping", "bootnotice", "echo", "md"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("缺少插件 %s：%v", want, names)
		}
	}
}

// 启动期只读目录、不释放（ADR-0005 决策 12：删除即卸载）。
//
// 这条守的是 pluginproc.Load：它一旦开始"发现文件缺失就补回来"，
// 用户就再也没法靠删文件卸载一个示例插件。
func TestDeletedPluginIsNotRestoredAtStartup(t *testing.T) {
	requireBundle(t)

	dst := t.TempDir()
	if err := pluginbundle.Extract(dst); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	deleted := filepath.Join(dst, wantNames()[0])
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	names := loadNames(t, dst)
	if len(names) != 3 {
		t.Fatalf("删掉一个插件后应装载 3 个，实际 %d 个：%v", len(names), names)
	}
	if _, err := os.Stat(deleted); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("装配期不得自动释放被删的插件（删除即卸载），但 %s 又出现了", deleted)
	}

	// 恢复只走显式 init --plugins：Extract 会把它补回来。
	if err := pluginbundle.Extract(dst); err != nil {
		t.Fatalf("显式释放: %v", err)
	}
	if _, err := os.Stat(deleted); err != nil {
		t.Fatalf("显式释放后 %s 应当回来: %v", deleted, err)
	}
}

// 内嵌副本只作释放源：plugins/bin/ 里已经有一份（哪怕是旧版），
// 释放不得覆盖它。用户手工换过的版本优先。
func TestExistingFileIsNotOverwrittenByBundle(t *testing.T) {
	requireBundle(t)

	dst := t.TempDir()
	if err := pluginbundle.Extract(dst); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	target := filepath.Join(dst, wantNames()[0])
	if err := os.WriteFile(target, []byte("old-build"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := pluginbundle.Extract(dst); err != nil {
		t.Fatalf("第二次 Extract: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-build" {
		t.Fatalf("已存在的插件被内嵌副本覆盖成了 %d 字节，期望保留 old-build", len(got))
	}
}

// loadNames 用生产那条装配路径（pluginproc.Load）扫目录，返回插件名。
func loadNames(t *testing.T, dir string) []string {
	t.Helper()
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          dir,
		ProbeTimeout: 20 * time.Second,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("pluginproc.Load(%s): %v", dir, err)
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Manifest().Name)
	}
	return names
}
