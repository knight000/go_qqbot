package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	got, err := parseTarget("linux/arm64")
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if got.os != "linux" || got.arch != "arm64" {
		t.Fatalf("解析成 %+v，期望 linux/arm64", got)
	}

	// 目标平台是 CI 的输入，写错必须立刻报错：默默按本机编会产出
	// "文件名是 linux_arm64、内容是 windows 二进制"的发布件。
	for _, bad := range []string{"", "linux", "linux/amd64/extra", "/amd64", "linux/"} {
		if _, err := parseTarget(bad); err == nil {
			t.Errorf("parseTarget(%q) 应当报错", bad)
		}
	}
}

func TestParseArgsDefaults(t *testing.T) {
	for _, name := range []string{"plugins", "core", "all"} {
		p, err := parseArgs([]string{name})
		if err != nil {
			t.Fatalf("parseArgs(%q): %v", name, err)
		}
		if p.target.os != runtime.GOOS || p.target.arch != runtime.GOARCH {
			t.Errorf("%s 的默认目标 = %s，期望本机 %s/%s",
				name, p.target, runtime.GOOS, runtime.GOARCH)
		}
		if p.out != "" {
			t.Errorf("%s 的默认 -o 应为空（用平台默认路径），实际 %q", name, p.out)
		}
	}

	if _, err := parseArgs(nil); err == nil {
		t.Error("缺子命令应当报错")
	}
	if _, err := parseArgs([]string{"release"}); err == nil {
		t.Error("未知子命令应当报错")
	}
}

// 参数可以跟在子命令后面（#43 的 CI 会这么写）。
func TestParseArgsFlagsAfterSubcommand(t *testing.T) {
	p, err := parseArgs([]string{"plugins", "-target", "linux/arm64", "-o", "out/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if p.cmd != cmdPlugins {
		t.Fatalf("子命令 = %v，期望 plugins", p.cmd)
	}
	if p.target.os != "linux" || p.target.arch != "arm64" {
		t.Fatalf("目标 = %s", p.target)
	}
	if p.out != "out/bin" {
		t.Fatalf("-o = %q", p.out)
	}
}

// 插件必须落进 internal/pluginbundle/dist/<goos>_<goarch>/：go:embed 只认
// 包目录内的路径，写别处核心就编不过（ADR-0005 决策 11）。
func TestPluginDir(t *testing.T) {
	root := t.TempDir()
	tgt := target{os: "linux", arch: "arm64"}
	want := filepath.Join(root, "internal", "pluginbundle", "dist", "linux_arm64")

	p := plan{cmd: cmdPlugins, target: tgt}
	if got := p.pluginDir(root); got != want {
		t.Fatalf("默认插件目录 = %s，期望 %s", got, want)
	}

	p.out = filepath.Join(root, "stage")
	if got := p.pluginDir(root); got != p.out {
		t.Fatalf("plugins -o 应覆盖插件目录，实际 %s", got)
	}

	// all 的 -o 是核心产物路径，插件仍写 embed 目录：否则核心编不过。
	all := plan{cmd: cmdAll, target: tgt, out: filepath.Join(root, "qqbot")}
	if got := all.pluginDir(root); got != want {
		t.Fatalf("all 的插件目录 = %s，期望仍是 %s", got, want)
	}
}

func TestCorePath(t *testing.T) {
	root := t.TempDir()
	p := plan{cmd: cmdCore, target: target{os: "windows", arch: "amd64"}}
	if got, want := p.corePath(root), filepath.Join(root, "bot.exe"); got != want {
		t.Fatalf("Windows 核心产物 = %s，期望 %s", got, want)
	}

	p.target = target{os: "linux", arch: "amd64"}
	if got, want := p.corePath(root), filepath.Join(root, "bot"); got != want {
		t.Fatalf("Linux 核心产物 = %s，期望 %s", got, want)
	}

	// -o 是文件路径
	p.out = filepath.Join(root, "dist", "qqbot")
	if got := p.corePath(root); got != p.out {
		t.Fatalf("-o 文件路径被改写成了 %s", got)
	}

	// -o 是已存在的目录：补上默认文件名，别把它当成一个文件去写。
	dir := filepath.Join(root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p.out = dir
	if got, want := p.corePath(root), filepath.Join(dir, "bot"); got != want {
		t.Fatalf("-o 目录时核心产物 = %s，期望 %s", got, want)
	}
}

// 插件清单来自目录：每个 plugins/<name>/cmd/<name>/main.go 就是一个产物。
func TestPluginNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "plugins/echo/cmd/echo/main.go", "package main\n")
	writeFile(t, root, "plugins/md/cmd/md/main.go", "package main\n")
	// 只有文档的目录不是插件，忽略。
	if err := os.MkdirAll(filepath.Join(root, "plugins", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 编辑器/隐藏目录忽略。
	if err := os.MkdirAll(filepath.Join(root, "plugins", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	names, err := pluginNames(root)
	if err != nil {
		t.Fatalf("pluginNames: %v", err)
	}
	if strings.Join(names, ",") != "echo,md" {
		t.Fatalf("插件清单 = %v，期望 [echo md]", names)
	}
}

// 目录里有 Go 源码却没有入口 → 报错。旧的生成式装配就是栽在这类
// "构建全绿、指令没反应"的静默缺失上；构建工具必须比它更早、更响地失败。
func TestPluginNamesRejectsPackageWithoutEntrypoint(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "plugins/orphan/orphan.go", "package orphan\n")

	_, err := pluginNames(root)
	if err == nil {
		t.Fatal("有 Go 源码却没有 cmd/<name>/main.go 时应当报错")
	}
	if !strings.Contains(err.Error(), "cmd/orphan/main.go") {
		t.Fatalf("错误串应指出缺哪个入口，实际: %v", err)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
