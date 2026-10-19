package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestChildEnvReplacesInherited(t *testing.T) {
	t.Setenv(supervisorEnv, "0")
	t.Setenv(restartEnv, "0")

	env := childEnv(true)
	if got := countPrefix(env, supervisorEnv+"="); got != 1 {
		t.Fatalf("supervisorEnv 出现 %d 次，期望 1", got)
	}
	if got := countPrefix(env, restartEnv+"="); got != 1 {
		t.Fatalf("restartEnv 出现 %d 次，期望 1", got)
	}
	if !hasKV(env, supervisorEnv+"=1") || !hasKV(env, restartEnv+"=1") {
		t.Fatalf("托管标识取值不对: %v", filter(env, supervisorEnv, restartEnv))
	}

	// 冷启动那一轮不能带 QQBOT_RESTARTED，否则 bootnotice 会把首次启动
	// 当成重载，跳过它本该做的存活确认。
	cold := childEnv(false)
	if got := countPrefix(cold, restartEnv+"="); got != 0 {
		t.Fatalf("冷启动带了 restartEnv: %v", filter(cold, restartEnv))
	}
	if got := countPrefix(cold, supervisorEnv+"="); got != 1 {
		t.Fatalf("冷启动缺 supervisorEnv: %v", filter(cold, supervisorEnv))
	}
}

func TestExitCode(t *testing.T) {
	if os.Getenv("DEVRUN_TEST_HELPER") == "1" {
		os.Exit(exitRestart)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestExitCode")
	cmd.Env = append(os.Environ(), "DEVRUN_TEST_HELPER=1")
	if got := exitCode(cmd.Run()); got != exitRestart {
		t.Fatalf("退出码 %d，期望 %d", got, exitRestart)
	}
	if got := exitCode(nil); got != 0 {
		t.Fatalf("nil 应映射为 0，得到 %d", got)
	}
}

func TestModuleRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := moduleRoot(deep)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("找到 %s，期望 %s", got, root)
	}

	// 没有 go.mod 的目录树必须报错，而不是一直往上爬到盘根。
	if _, err := moduleRoot(t.TempDir()); err == nil {
		t.Fatal("没有 go.mod 时应报错")
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bot")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := bin + ".new"
	if err := os.WriteFile(tmp, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replace(tmp, bin); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("内容 %q，期望 new", got)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("临时文件应已被改名消耗掉，得到 err=%v", err)
	}
}

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

func hasKV(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func filter(env []string, prefixes ...string) []string {
	var out []string
	for _, kv := range env {
		for _, p := range prefixes {
			if strings.HasPrefix(kv, p+"=") {
				out = append(out, kv)
			}
		}
	}
	return out
}
