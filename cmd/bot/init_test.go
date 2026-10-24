package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/pluginbundle"
	"github.com/knight000/go_qqbot/internal/pluginproc"
)

// argSep 用来把参数列表塞进一个环境变量。不用 NUL：Windows 的环境块里
// 不允许出现 NUL，exec 会直接报 "environment variable contains NUL"。
const argSep = "\x1f"

// 这一组用例要的是**真实进程**：init 的价值全在"文件系统上多了什么、
// 退出码是什么、下次启动读不读得到"，在进程内调函数看不出这些。
//
// 用测试二进制自己当 qqbot：TestMain 看到 QQBOT_TEST_RUN_MAIN 就直接走
// main()，于是 runBot 跑到的就是编译好的那个程序（含它内嵌的出厂配置与插件），
// 退出码、stderr、工作目录全部按真实进程的语义。
func TestMain(m *testing.M) {
	if os.Getenv("QQBOT_TEST_RUN_MAIN") == "1" {
		args := []string{"qqbot"}
		if raw := os.Getenv("QQBOT_TEST_ARGS"); raw != "" {
			args = append(args, strings.Split(raw, argSep)...)
		}
		os.Args = args
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runBot 把测试二进制当 qqbot 跑一次，返回 stdout / stderr / 退出码。
//
// dir 是子进程的工作目录：默认 [store].path 是相对路径，数据目录落在哪里
// 取决于它，所以每个用例都给自己一个干净目录。
func runBot(t *testing.T, dir string, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), extraEnv...)
	cmd.Env = append(cmd.Env,
		"QQBOT_TEST_RUN_MAIN=1",
		"QQBOT_TEST_ARGS="+strings.Join(args, argSep))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut

	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("运行 qqbot %v: %v", args, err)
	}
	return out.String(), errOut.String(), code
}

// childEnv 去掉会互相干扰的变量：QQBOT_TEST_* 只由本文件设置，QQ_APP_* 必须
// 由每个用例显式决定（开发机上残留的真实凭据会让"app.id 为空"永远不触发）。
func childEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "QQBOT_TEST_") || strings.HasPrefix(name, "QQ_APP_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// requireBundle 在干净克隆（dist 里只有 .gitkeep）上跳过：那是一种合法的
// 构建状态，不是失败。空副本本身的行为由 pluginbundle 的单测守住。
func requireBundle(t *testing.T) {
	t.Helper()
	if len(pluginbundle.Names()) == 0 {
		t.Skip("本产物没有出厂插件（没跑 go run ./tools/build plugins）：跳过 init 的插件释放验证")
	}
}

// pluginFile 返回出厂插件在本平台的文件名（Windows 带 .exe）。
func pluginFile(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// 空目录 init：配置、数据目录、四个插件都要出现，且配置就是内嵌的那份。
func TestInitInEmptyDirCreatesEverything(t *testing.T) {
	requireBundle(t)
	work := t.TempDir()
	cfgPath := filepath.Join(work, "config", "config.local.toml")

	stdout, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath)
	if code != 0 {
		t.Fatalf("init 退出码 = %d，期望 0；stderr: %s", code, stderr)
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	if !bytes.Equal(got, config.ExampleTOML()) {
		t.Fatalf("写出去的配置不是内嵌的示例（%d 字节 vs %d 字节）", len(got), len(config.ExampleTOML()))
	}
	// 示例里 app.id 必须留空：填好了就没人会去填自己的 AppID。
	var parsed config.Config
	if _, err := toml.Decode(string(got), &parsed); err != nil {
		t.Fatalf("写出去的配置不是合法 TOML: %v", err)
	}
	if strings.TrimSpace(parsed.App.ID) != "" || strings.TrimSpace(parsed.App.Secret) != "" {
		t.Errorf("示例配置不该带凭据，实际 id=%q secret=%q", parsed.App.ID, parsed.App.Secret)
	}

	// 默认 [store].path = ./data/qqbot.db，相对子进程的工作目录。
	datadir := filepath.Join(work, "data")
	if info, err := os.Stat(datadir); err != nil || !info.IsDir() {
		t.Fatalf("数据目录 %s 未创建: %v", datadir, err)
	}
	// 数据目录不能落在 .temp 下：那是仓库内开发期的临时目录名，会被清理脚本
	// 连数据库带插件一起删掉。
	if _, err := os.Stat(filepath.Join(work, ".temp")); err == nil {
		t.Error("默认数据目录不该再创建 .temp/")
	}
	binDir := filepath.Join(datadir, "plugins", "bin")
	for _, name := range []string{"adminping", "bootnotice", "echo", "md"} {
		if _, err := os.Stat(filepath.Join(binDir, pluginFile(name))); err != nil {
			t.Errorf("插件 %s 未释放: %v", name, err)
		}
	}

	// 输出里的数据目录就是用户唯一能看到的"东西落在哪"，不能是 .temp\data。
	if !strings.Contains(stdout, "数据目录：data") {
		t.Errorf("init 输出应报告数据目录 data，实际：\n%s", stdout)
	}

	// 下一步必须说清楚去哪儿填凭据，否则用户拿着空配置启动只会看到校验错误。
	for _, want := range []string{"[app].id", "[app].secret", cfgPath} {
		if !strings.Contains(stdout, want) {
			t.Errorf("init 输出缺少 %q：\n%s", want, stdout)
		}
	}
}

// 已有配置再 init：报错、文件不动；--force 才覆盖。
func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	requireBundle(t)
	work := t.TempDir()
	cfgPath := filepath.Join(work, "config.local.toml")
	if _, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath); code != 0 {
		t.Fatalf("第一次 init 失败: %s", stderr)
	}

	// 用户改过的配置：既看内容也看 mtime，两者都不能被动。
	const edited = "# 用户改过的配置\n[app]\nid = \"123\"\nsecret = \"s\"\n"
	if err := os.WriteFile(cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(cfgPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath)
	if code == 0 {
		t.Fatal("配置已存在时 init 必须失败，不能静默覆盖")
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("报错要给出 --force 这条出路，实际: %s", stderr)
	}
	if got, _ := os.ReadFile(cfgPath); string(got) != edited {
		t.Errorf("未加 --force 时配置被改了:\n%s", got)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Errorf("未加 --force 时配置 mtime 变了: %v，期望 %v", info.ModTime(), stamp)
	}

	if _, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath, "--force"); code != 0 {
		t.Fatalf("--force 应当覆盖成功，实际退出码 %d: %s", code, stderr)
	}
	if got, _ := os.ReadFile(cfgPath); !bytes.Equal(got, config.ExampleTOML()) {
		t.Error("--force 之后配置没有恢复成示例内容")
	}
}

// init --plugins：只补回被删的那个插件，配置与其余插件的 mtime 一律不动。
func TestInitPluginsRestoresOnlyDeleted(t *testing.T) {
	requireBundle(t)
	work := t.TempDir()
	cfgPath := filepath.Join(work, "config.local.toml")
	if _, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}

	binDir := filepath.Join(work, "data", "plugins", "bin")
	cfgStamp := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	binStamp := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(cfgPath, cfgStamp, cfgStamp); err != nil {
		t.Fatal(err)
	}
	for _, name := range pluginbundle.Names() {
		if err := os.Chtimes(filepath.Join(binDir, name), binStamp, binStamp); err != nil {
			t.Fatal(err)
		}
	}
	deleted := filepath.Join(binDir, pluginFile("echo"))
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := runBot(t, work, nil, "init", "--plugins", "-c", cfgPath); code != 0 {
		t.Fatalf("init --plugins 失败: %s", stderr)
	}

	if _, err := os.Stat(deleted); err != nil {
		t.Fatalf("被删的 echo 应当补回来: %v", err)
	}
	for _, name := range pluginbundle.Names() {
		if name == pluginFile("echo") {
			continue
		}
		info, err := os.Stat(filepath.Join(binDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(binStamp) {
			t.Errorf("其余插件 %s 不该被重写（mtime %v，期望 %v）", name, info.ModTime(), binStamp)
		}
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(cfgStamp) {
		t.Errorf("--plugins 不该碰配置（mtime %v，期望 %v）", info.ModTime(), cfgStamp)
	}
}

// --plugins 在配置不存在时也要给一句人话：它靠 [store].path 定位数据目录，
// 没有配置就无从下手。
func TestInitPluginsWithoutConfigExplains(t *testing.T) {
	work := t.TempDir()
	cfgPath := filepath.Join(work, "missing.toml")
	_, stderr, code := runBot(t, work, nil, "init", "--plugins", "-c", cfgPath)
	if code == 0 {
		t.Fatal("配置不存在时 --plugins 必须失败")
	}
	if !strings.Contains(stderr, "不存在") {
		t.Errorf("报错要说明配置不存在，实际: %s", stderr)
	}
}

// 验收第 4 条：app.id 为空时，启动错误指向 `qqbot init` 与 [app].id。
// 走真实进程的启动路径（config.Load 是 run() 的第一件事，不需要网络）。
func TestStartupWithEmptyAppIDPointsToInit(t *testing.T) {
	work := t.TempDir()
	cfgPath := filepath.Join(work, "config.local.toml")
	if err := os.WriteFile(cfgPath, config.ExampleTOML(), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runBot(t, work, []string{"QQ_APP_ID=", "QQ_APP_SECRET="}, "-c", cfgPath)
	if code == 0 {
		t.Fatal("app.id 为空时启动必须失败")
	}
	for _, want := range []string{"qqbot init", "[app].id"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("启动错误缺少 %q：\n%s", want, stderr)
		}
	}
}

// init 产出的配置能过校验、能定位到刚释放的四个插件——这是"init 之后能启动"
// 的装配层验证（不连网关：那需要网络与 token，与配置正确性无关）。
func TestGeneratedConfigLoadsAndDiscoversPlugins(t *testing.T) {
	requireBundle(t)
	work := t.TempDir()
	cfgPath := filepath.Join(work, "config.local.toml")
	if _, stderr, code := runBot(t, work, nil, "init", "-c", cfgPath); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}

	t.Setenv("QQ_APP_ID", "102000001")
	t.Setenv("QQ_APP_SECRET", "secret-for-test")
	// 配置里的 [store].path 是相对路径，相对的是"进程的工作目录"——
	// 子进程在 work 下跑 init，这里也要站在 work 上看同一份相对路径。
	t.Chdir(work)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("init 产出的配置过不了校验: %v", err)
	}

	binDir := filepath.Join(filepath.Dir(cfg.Store.Path), "plugins", "bin")
	plugins, err := pluginproc.Load(context.Background(), pluginproc.Options{
		Dir:          binDir,
		ProbeTimeout: 20 * time.Second,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("装载刚释放的插件: %v", err)
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Manifest().Name)
	}
	sort.Strings(names)
	if want := []string{"adminping", "bootnotice", "echo", "md"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("装载到 %v，期望 %v", names, want)
	}
}
