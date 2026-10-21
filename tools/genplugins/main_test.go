package main

// P3-19 的守卫：生成器的判据必须是"真的会注册"，而不是"目录里有 package"。
// 后者会让 plugins/foo/plugin/*.go 或共享库目录静默通过，构建全绿、插件永不注册。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePluginFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pluginRoot 造一个只含 plugins/ 的模块根。
func pluginRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPluginDirsAcceptsRegisterAndForwardingRoot(t *testing.T) {
	root := pluginRoot(t)
	base := filepath.Join(root, "plugins")

	// 常规形态：根包自己注册。
	writePluginFile(t, filepath.Join(base, "direct"), "direct.go",
		"package direct\n\nfunc init() { host.Register(&Plugin{}) }\n")

	// 插件仓形态：根包只做 blank import 转发，注册在子包（ARCHITECTURE §3.5）。
	writePluginFile(t, filepath.Join(base, "forward"), "forward.go",
		"package forward\n\nimport _ \"example.com/plugins/forward/plugin\"\n")
	writePluginFile(t, filepath.Join(base, "forward", "plugin"), "plugin.go",
		"package plugin\n\nfunc init() { host.Register(&Plugin{}) }\n")

	got, err := pluginDirs(root)
	if err != nil {
		t.Fatalf("合法形态不该报错: %v", err)
	}
	want := []string{"direct", "forward"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pluginDirs=%v，期望 %v", got, want)
	}
}

// 注册代码只在子包、根包没有 Go 源文件：blank import 根包根本编译不了，
// 插件永远进不了二进制。必须拒绝，而不是静默跳过。
func TestPluginDirsRejectsRegisterOnlyInSubpackage(t *testing.T) {
	root := pluginRoot(t)
	writePluginFile(t, filepath.Join(root, "plugins", "subonly", "plugin"), "plugin.go",
		"package plugin\n\nfunc init() { host.Register(&Plugin{}) }\n")

	_, err := pluginDirs(root)
	if err == nil {
		t.Fatal("根包没有 Go 源文件、注册只在子包时应报错")
	}
	if !strings.Contains(err.Error(), "subonly") {
		t.Fatalf("错误信息要指出目录: %v", err)
	}
}

// 目录里有 package 却从不调用 host.Register：它就是那个"共享库/放错位置"
// 的目录。跳过的代价是插件永不生效且没有任何信号。
func TestPluginDirsRejectsPackageWithoutRegister(t *testing.T) {
	root := pluginRoot(t)
	writePluginFile(t, filepath.Join(root, "plugins", "shared"), "shared.go",
		"package shared\n\n// 共享工具，不是插件。\nvar X = 1\n")

	_, err := pluginDirs(root)
	if err == nil {
		t.Fatal("有 package 但没有 host.Register( 时应报错")
	}
	if !strings.Contains(err.Error(), "shared") || !strings.Contains(err.Error(), "host.Register(") {
		t.Fatalf("错误信息要说清缺什么: %v", err)
	}
}

// 目录里既没有 Go 源码也没有注册调用（文档、资源目录）不是插件，应跳过。
func TestPluginDirsSkipsNonPluginDirs(t *testing.T) {
	root := pluginRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "plugins", "_notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "plugins", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "docs", "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := pluginDirs(root)
	if err != nil {
		t.Fatalf("非插件目录不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不该把非插件目录当成插件: %v", got)
	}
}
