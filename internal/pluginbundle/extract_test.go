package pluginbundle

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeBundle 模拟一份内嵌副本。内容与真实产物无关：这里观察的是释放语义
// （写哪些、跳过哪些、权限是什么），不是某个插件的行为。
func fakeBundle() fstest.MapFS {
	return fstest.MapFS{
		// go:embed all: 会把 .gitkeep 也嵌进来——它是占位文件，不是插件。
		".gitkeep": {Data: nil},
		"echo":     {Data: []byte("echo-v2")},
		"md":       {Data: []byte("md-v2")},
	}
}

func TestExtractWritesMissingPlugins(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "plugins", "bin")
	if err := extractFS(fakeBundle(), dst); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for name, want := range map[string]string{"echo": "echo-v2", "md": "md-v2"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s 内容 = %q，期望 %q", name, got, want)
		}
	}

	// 占位文件不得被当成插件释放出去：它出现在 plugins/bin/ 里会让
	// pluginproc 去探测一个空文件，日志里多一条"探测失败"。
	if _, err := os.Stat(filepath.Join(dst, ".gitkeep")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("占位文件不该被释放，得到 err=%v", err)
	}

	// 插件必须可执行：Windows 不看权限位，Unix 上没有 +x 就是"探测失败"。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dst, "echo"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("插件权限 %v 不可执行", info.Mode().Perm())
		}
	}
}

// 已存在的文件一律跳过：内嵌副本只作「释放源」，不能覆盖磁盘上的版本
// （ADR-0005 决策 11/12）。这是「换成旧版后仍跑旧版」的机制保证。
func TestExtractDoesNotOverwriteExisting(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "echo"), []byte("echo-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := extractFS(fakeBundle(), dst); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "echo-v1" {
		t.Errorf("已存在的 echo 被覆盖成 %q，期望保留 echo-v1", got)
	}
	// 缺的那份照常释放——跳过是逐个文件判断，不是"目录非空就整轮放弃"。
	if _, err := os.Stat(filepath.Join(dst, "md")); err != nil {
		t.Errorf("缺失的 md 应当被释放: %v", err)
	}
}

// 「删除即卸载，恢复靠显式 init --plugins」：第二次释放只补缺失的，
// 不碰已有的。这就是 ADR-0005 决策 12 把自愈改成显式命令后的全部机制。
func TestExtractRestoresOnlyDeleted(t *testing.T) {
	dst := t.TempDir()
	if err := extractFS(fakeBundle(), dst); err != nil {
		t.Fatalf("第一次 Extract: %v", err)
	}
	if err := os.Remove(filepath.Join(dst, "md")); err != nil {
		t.Fatal(err)
	}
	// 手工改掉仍在的那份，确认它不会被第二次释放动到。
	if err := os.WriteFile(filepath.Join(dst, "echo"), []byte("echo-local"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := extractFS(fakeBundle(), dst); err != nil {
		t.Fatalf("第二次 Extract: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "md")); string(got) != "md-v2" {
		t.Errorf("被删的 md 应恢复成 %q，实际 %q", "md-v2", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "echo")); string(got) != "echo-local" {
		t.Errorf("仍存在的 echo 不该被动，实际 %q", got)
	}
}

// 出厂副本里一个插件都没有（干净克隆的典型现场：dist/<平台>/ 只有 .gitkeep）
// 时，释放必须失败而不是"成功释放 0 个"。
//
// 静默成功会让用户以为插件已经就位，下一次启动只看到插件清单少了几项，
// 而 ADR-0005 决策 12 把恢复的唯一入口定在 `init --plugins`——那个入口
// 说"成功"却不补任何东西，用户就再也没有线索了。
func TestExtractEmptyBundleIsAnError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "plugins", "bin")
	err := extractFS(fstest.MapFS{".gitkeep": {Data: nil}}, dst)
	if err == nil {
		t.Fatal("空出厂副本必须报错，不能静默释放 0 个插件")
	}
	for _, want := range []string{"出厂插件", "go run ./tools/build plugins", "发布版"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误串 %q 缺少 %q：用户要能从错误里看出下一步怎么办", err, want)
		}
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("释放失败时不该留下空目录 %s", dst)
	}
}

// 名字清单按字典序、且只含真正的插件：装配期日志要能两次启动逐字比对，
// 而 .gitkeep / _ 前缀（Go 忽略的目录约定）都不是插件。
func TestNamesSortedAndFiltered(t *testing.T) {
	src := fstest.MapFS{
		"md":       {Data: []byte("x")},
		"echo":     {Data: []byte("x")},
		".gitkeep": {Data: nil},
		"_draft":   {Data: []byte("x")},
		"nested":   {Data: []byte("x")},
	}
	// 目录条目：ReadDir 会把它标成 IsDir。
	src["sub/inner"] = &fstest.MapFile{Data: []byte("x")}

	got, err := namesOf(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "echo" || got[1] != "md" || got[2] != "nested" {
		t.Fatalf("namesOf = %v，期望 [echo md nested]（字典序、不含点/下划线/目录）", got)
	}
}

func TestExtractRespectsPermOption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不用 Unix 权限位")
	}
	dst := t.TempDir()
	if err := extractFS(fakeBundle(), dst, WithPerm(0o700)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("权限 = %v，期望 0700", got)
	}
}
