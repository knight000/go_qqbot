package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/pluginbundle"
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

// 插件产物目录里必须带一份版本清单：核心启动期靠它判断"内嵌的比磁盘新"
// （#52）。清单是覆盖写，旧的必须被清掉——否则删掉的插件会永远留在清单里，
// 启动期每次去补一个已经不存在的产物。
func TestWriteVersionsRewritesManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pluginbundle.VersionsFile), []byte("gone=9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeVersions(dir, map[string]string{"md": "0.3.0", "echo": "0.1.0"}); err != nil {
		t.Fatalf("writeVersions: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, pluginbundle.VersionsFile))
	if err != nil {
		t.Fatal(err)
	}
	want := "# 由 go run ./tools/build plugins 生成：插件名=版本\n" +
		"echo=0.1.0\n" +
		"md=0.3.0\n"
	if string(data) != want {
		t.Fatalf("清单内容 =\n%q\n期望\n%q", data, want)
	}
}

// 四个出厂插件的版本必须都能从 Manifest() 读到：清单里的版本号不是手写的，
// 改了插件代码忘了涨版本号时，这里至少保证不会写进空值。
func TestPluginVersionsCoverBundled(t *testing.T) {
	vs := pluginVersions()
	for _, name := range []string{"adminping", "bootnotice", "echo", "md"} {
		if strings.TrimSpace(vs[name]) == "" {
			t.Errorf("插件 %s 没有版本号，无法写入 %s", name, pluginbundle.VersionsFile)
		}
	}
}
