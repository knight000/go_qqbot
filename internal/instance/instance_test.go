package instance

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func lockPath(t *testing.T) string {
	t.Helper()
	// Acquire 会把路径转成绝对路径，这里先对齐，免得断言被 Clean 的差异绊倒。
	p, err := filepath.Abs(filepath.Join(t.TempDir(), "qqbot.db.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAcquireWritesHolder(t *testing.T) {
	path := lockPath(t)
	lock, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	defer lock.Release()

	if lock.Path() != path {
		t.Fatalf("路径不符: %s", lock.Path())
	}
	if lock.Holder().PID != os.Getpid() {
		t.Fatalf("持有者 pid 应为当前进程: %+v", lock.Holder())
	}
	if lock.Holder().Exe == "" {
		t.Fatalf("应记录可执行文件路径: %+v", lock.Holder())
	}

	// 元数据要能被另一个进程读出来，否则冲突时无法告诉用户是谁占着。
	got := readHolder(path)
	if got.PID != os.Getpid() || got.Exe != lock.Holder().Exe {
		t.Fatalf("回读的持有者信息不符: %+v", got)
	}
}

func TestSecondAcquireIsRejected(t *testing.T) {
	path := lockPath(t)
	first, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	defer first.Release()

	start := time.Now()
	second, err := Acquire(path, 0)
	if err == nil {
		_ = second.Release()
		t.Fatal("同一路径不应能二次加锁")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("应返回 ErrLocked，实际: %v", err)
	}
	// wait=0 必须立刻失败：第二个实例等下去没有任何意义。
	if d := time.Since(start); d > time.Second {
		t.Fatalf("wait=0 时不应等待，实际耗时 %s", d)
	}

	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("应能断言为 *LockedError: %v", err)
	}
	if locked.Holder.PID != os.Getpid() {
		t.Fatalf("冲突信息应指出持有者 pid: %+v", locked.Holder)
	}
	if !strings.Contains(err.Error(), "另一个实例正在运行") {
		t.Fatalf("错误文案不可操作: %v", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	path := lockPath(t)
	first, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	// 重复释放不能 panic：defer 与显式释放同时存在是很常见的写法。
	if err := first.Release(); err != nil {
		t.Fatalf("重复释放应无害: %v", err)
	}

	second, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("释放后应能重新加锁: %v", err)
	}
	defer second.Release()
}

func TestAcquireWaitsForRelease(t *testing.T) {
	path := lockPath(t)
	first, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}

	// 模拟旧进程收尾：200ms 后才放手，等待方应当拿到锁而不是直接失败。
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = first.Release()
	}()

	start := time.Now()
	second, err := Acquire(path, 3*time.Second)
	if err != nil {
		t.Fatalf("等待模式下应最终拿到锁: %v", err)
	}
	defer second.Release()

	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("应当真的等过一段时间，实际 %s", d)
	}
	if second.Holder().PID != os.Getpid() {
		t.Fatalf("新锁的持有者应是当前进程: %+v", second.Holder())
	}
}

func TestAcquireTimesOutWhileHeld(t *testing.T) {
	path := lockPath(t)
	first, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	defer first.Release()

	start := time.Now()
	_, err = Acquire(path, 300*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("超时后应返回 ErrLocked，实际: %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("应等满等待窗口，实际 %s", d)
	}
}

const helperEnv = "QQBOT_LOCK_HELPER_PATH"

// TestHelperHoldsLock 只作为子进程运行，用来验证跨进程互斥。
// 直接跑本用例时它会跳过。
func TestHelperHoldsLock(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		t.Skip("仅作为跨进程用例的子进程助手运行")
	}
	lock, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("子进程加锁失败: %v", err)
	}
	defer lock.Release()
	// 这行是父进程的同步点：看到它才说明锁已经在别的进程手里。
	t.Log("LOCKED")
	time.Sleep(30 * time.Second)
}

// TestCrossProcessLock 是这整个包存在的理由：两个独立进程真的互斥，
// 且其中一个被强杀后锁随即自动释放（不需要任何清理脚本）。
func TestCrossProcessLock(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Skip("父进程用例")
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("该平台无进程模型")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}

	path := lockPath(t)
	cmd := exec.Command(exe, "-test.run=^TestHelperHoldsLock$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	locked := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "LOCKED") {
				close(locked)
				return
			}
		}
	}()

	select {
	case <-locked:
	case <-time.After(15 * time.Second):
		t.Fatal("子进程没有在 15s 内报出已持锁")
	}

	// 子进程持锁期间，本进程必须被挡住——这是 4902 的唯一防线。
	if l, err := Acquire(path, 200*time.Millisecond); !errors.Is(err, ErrLocked) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("另一进程持锁时应失败，实际: %v", err)
	}

	// 强杀：不给子进程任何清理机会。锁必须由内核收回。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("结束子进程: %v", err)
	}
	_ = cmd.Wait()

	lock, err := Acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("持有者退出后锁应自动释放: %v", err)
	}
	defer lock.Release()
}
