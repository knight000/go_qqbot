// Command devrun 在开发工作区托管 qqbot：把 /reload 从「换进程映像」升级为
// 「重新生成装配 → 编译 → 替换二进制 → 拉起」。
//
// 为什么需要它：Go 的插件是编译期注册的，重新执行同一份二进制不会让新放入
// plugins/ 的插件生效，只有重新编译才行。而 Windows 没有 execve，qqbot 自己
// 重启只能 spawn 后退出，shell 立刻拿回提示符——终端体验是断的。让一个不死的
// 父进程持有终端，两个问题一起解决。
//
// 用法（在工作区根目录）：
//
//	go run ./tools/devrun -c config/config.local.toml
//
// 除 -h/--help 外，所有参数原样传给 qqbot。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	// supervisorEnv 告诉 qqbot：/reload 不要自己换映像，用退出码交给父进程。
	supervisorEnv = "QQBOT_SUPERVISOR"
	// restartEnv 让新进程知道自己不是冷启动（bootnotice 据此抑制崩溃告警）。
	restartEnv = "QQBOT_RESTARTED"
	// exitRestart 是 qqbot 与 devrun 之间的重启协议，取值 sysexits 的 EX_TEMPFAIL。
	exitRestart = 75
	// shutdownWait 是收到 Ctrl-C 后等 qqbot 落盘 session 的上限。
	shutdownWait = 20 * time.Second
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-h" || a == "--help" {
			usage()
			return
		}
	}
	os.Exit(run(args))
}

func usage() {
	fmt.Fprint(os.Stderr, `用法：go run ./tools/devrun [qqbot 的参数...]

在工作区根目录托管 qqbot：
  - 启动前执行 go generate ./cmd/bot 与 go build ./cmd/bot
  - /reload 时重新生成、编译、替换二进制并拉起，终端不释放
  - 编译失败时打印错误并沿用上一次的二进制

例：
  go run ./tools/devrun -c config/config.local.toml
  go run ./tools/devrun -debug
`)
}

func run(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[devrun] 取当前目录:", err)
		return 1
	}
	// 编译必须在模块根执行；启动 qqbot 时回到用户的 cwd，
	// 否则 -c 里的相对路径会解析到别处。
	root, err := moduleRoot(cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[devrun]", err)
		return 1
	}

	bin := filepath.Join(root, "bot")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	restarted := false
	for {
		if err := build(root, bin); err != nil {
			fmt.Fprintln(os.Stderr, "[devrun] 构建失败:", err)
			if _, statErr := os.Stat(bin); statErr != nil {
				fmt.Fprintln(os.Stderr, "[devrun] 没有可用的旧二进制，停止")
				return 1
			}
			fmt.Fprintln(os.Stderr, "[devrun] 沿用上一次构建继续运行")
		}

		code := supervise(bin, cwd, args, restarted)
		if code != exitRestart {
			return code
		}
		restarted = true
		fmt.Fprintln(os.Stderr, "[devrun] 收到重载请求，重新生成并编译")
	}
}

// build 生成装配并编译到 bin。先写临时名再替换，任何一步失败都不会破坏
// 正在使用的旧二进制。
func build(root, bin string) error {
	gen := exec.Command("go", "generate", "./cmd/bot")
	gen.Dir = root
	gen.Stdout = os.Stderr
	gen.Stderr = os.Stderr
	if err := gen.Run(); err != nil {
		return fmt.Errorf("go generate: %w", err)
	}

	tmp := bin + ".new"
	compile := exec.Command("go", "build", "-o", tmp, "./cmd/bot")
	compile.Dir = root
	compile.Stdout = os.Stderr
	compile.Stderr = os.Stderr
	if err := compile.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("go build: %w", err)
	}

	if err := replace(tmp, bin); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换二进制: %w", err)
	}
	return nil
}

// replace 把新构建放到 bin。
//
// Unix 的 rename 直接覆盖即可，运行中的旧 inode 不受影响。Windows 不允许
// 覆盖正在运行的 exe，但允许改名：先把旧的挪成 .old，再放进新的。残留的
// .old 在下一个重载周期尝试清理，删不掉说明仍被占用，留着不影响启动。
func replace(tmp, bin string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(tmp, bin)
	}
	old := bin + ".old"
	os.Remove(old)
	if _, err := os.Stat(bin); err == nil {
		if err := os.Rename(bin, old); err != nil {
			// 挪不动就试直接覆盖；Go 自己的 build 在 Windows 上也走这条路径。
			return os.Rename(tmp, bin)
		}
	}
	return os.Rename(tmp, bin)
}

// supervise 跑一轮 qqbot 并等待它退出，返回退出码。
func supervise(bin, dir string, args []string, restarted bool) int {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = childEnv(restarted)

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "[devrun] 启动失败:", err)
		return 1
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Ctrl-C 会被控制台广播给 qqbot 自己，它知道怎么优雅关闭。本进程只需
	// 等它关完再走——提前退出会把终端还给 shell，日志就散了。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case err := <-done:
		return exitCode(err)
	case <-sig:
		select {
		case err := <-done:
			return exitCode(err)
		case <-time.After(shutdownWait):
			fmt.Fprintln(os.Stderr, "[devrun] 等待超时，强制结束")
			_ = cmd.Process.Kill()
			<-done
			return 1
		}
	}
}

// childEnv 复制环境并写入托管标识：清掉可能从外层继承的同名项，
// 避免手工调试时的残留值影响判断。
func childEnv(restarted bool) []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, supervisorEnv+"=") || strings.HasPrefix(kv, restartEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, supervisorEnv+"=1")
	if restarted {
		out = append(out, restartEnv+"=1")
	}
	return out
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return code
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[devrun] 等待子进程:", err)
	return 1
}

// moduleRoot 从 dir 向上找第一个 go.mod。
func moduleRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("找不到 go.mod：请在核心仓工作区内运行")
		}
		dir = parent
	}
}
