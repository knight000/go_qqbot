package main

import (
	"errors"
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

// 第二步改名失败（旧的已经叫 .old、新的还没就位）必须把 .old 改回 bin：
// 否则一次无人值守的 /reload 就让二进制消失、进程下线，而能跑的那份躺在旁边。
func TestReplaceRollsBackWhenSecondRenameFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("两步替换只发生在 Windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bot.exe")
	tmp := bin + ".new"
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	rename := func(oldpath, newpath string) error {
		if oldpath == tmp && newpath == bin {
			return errors.New("注入：新二进制无法就位")
		}
		return os.Rename(oldpath, newpath)
	}
	err := replaceWith(tmp, bin, rename)
	if err == nil {
		t.Fatal("第二步改名失败时应当返回错误")
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("回滚后 bin 应当存在: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("回滚后 bin 内容 %q，期望旧内容 old", got)
	}
	if _, err := os.Stat(bin + ".old"); !os.IsNotExist(err) {
		t.Fatalf("回滚后 .old 应已被消耗，得到 err=%v", err)
	}
}

// 替换中断后留下的现场：bin 缺失、.old 还在。启动前必须先接回来。
func TestRestoreBinaryFromOld(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bot.exe")
	old := bin + ".old"
	if err := os.WriteFile(old, []byte("survivor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restoreBinary(bin); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("恢复后 bin 应当存在: %v", err)
	}
	if string(got) != "survivor" {
		t.Fatalf("恢复后 bin 内容 %q，期望 survivor", got)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf(".old 应已被改名消耗，得到 err=%v", err)
	}

	// bin 正常时绝不能用备份覆盖它。
	if err := os.WriteFile(bin, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restoreBinary(bin); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(bin); string(got) != "current" {
		t.Fatalf("bin 已存在时不该被 .old 覆盖，实际内容 %q", got)
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
