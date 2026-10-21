package instance

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// 测试用 appID。appID 锁文件会被 isolateTemp 收进 t.TempDir()，
	// 不会落到真实系统临时目录里，所以固定值不会污染机器。
	testAppID = "102000001"
	// otherAppID 代表另一台 bot：它的锁不能被 testAppID 的占用串行化。
	otherAppID = "102000002"
)

// isolateTemp 把 appID 维度的锁文件指向 dir。锁文件按设计永不删除，
// 测试若落到真实系统临时目录会一路堆垃圾。
func isolateTemp(t *testing.T, dir string) {
	t.Helper()
	old := tempDir
	tempDir = func() string { return dir }
	t.Cleanup(func() { tempDir = old })
}

func dataLockPath(t *testing.T) string {
	t.Helper()
	// 每次调用都基于一个新的 t.TempDir()，所以两次调用得到的是两个不同的
	// 数据目录——"同 appID 不同数据目录"这条用例正是靠它构造的。
	p, err := filepath.Abs(filepath.Join(t.TempDir(), "qqbot.db.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func acquire(t *testing.T, appID, path string, wait time.Duration) *Lock {
	t.Helper()
	lock, err := Acquire(Options{AppID: appID, DataPath: path, Wait: wait})
	if err != nil {
		t.Fatalf("加锁失败（appID=%s path=%s）: %v", appID, path, err)
	}
	return lock
}

func TestAcquireWritesHolder(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	lock := acquire(t, testAppID, path, 0)
	defer lock.Release()

	if lock.Path() != path {
		t.Fatalf("数据目录锁路径不符: %s", lock.Path())
	}
	// appID 锁必须落在系统临时目录，而不是数据目录旁边：它约束的是整台机器上
	// 的同一个 bot，与数据目录在哪无关。
	if dir := filepath.Dir(lock.AppPath()); dir != tempDir() {
		t.Fatalf("appID 锁应位于临时目录 %s，实际 %s", tempDir(), dir)
	}
	if pid := lock.Holder().PID; pid != os.Getpid() {
		t.Fatalf("持有者 pid 应为当前进程: %+v", lock.Holder())
	}
	if exe := lock.Holder().Exe; exe == "" {
		t.Fatalf("应记录可执行文件路径: %+v", lock.Holder())
	}

	// 元数据要能被另一个进程读出来，否则冲突时无法告诉用户是谁占着。
	got := readHolder(lock.AppPath())
	if got.PID != os.Getpid() || got.AppID != testAppID || got.Host == "" {
		t.Fatalf("appID 锁里的持有者信息不符: %+v", got)
	}
}

func TestAppLockFileNameDerivesFromAppID(t *testing.T) {
	isolateTemp(t, t.TempDir())
	lock := acquire(t, testAppID, dataLockPath(t), 0)
	defer lock.Release()

	sum := sha256.Sum256([]byte(testAppID))
	prefix := hex.EncodeToString(sum[:])[:16]
	if name := filepath.Base(lock.AppPath()); !strings.Contains(name, prefix) {
		t.Fatalf("appID 锁文件名应含 sha256(appID) 前 16 位 %s，实际 %s", prefix, name)
	}
}

func TestAcquireRejectsEmptyAppID(t *testing.T) {
	isolateTemp(t, t.TempDir())
	// 空 appID 不能静默退化成"只锁数据目录"——那正是这条缺陷本身。
	if _, err := Acquire(Options{DataPath: dataLockPath(t)}); err == nil {
		t.Fatal("空 appID 应当拒绝加锁")
	}
}

// TestSameAppIDDifferentDataDirIsRejected 是 P2-8 的回归用例：两份配置同一
// app.id、不同 [store].path（测试库与正式库并存，或换了数据目录但旧进程还在）
// 会同时连上网关，同一 bot 两条 WS，一条被 4902 reset by resume。
func TestSameAppIDDifferentDataDirIsRejected(t *testing.T) {
	isolateTemp(t, t.TempDir())
	first := acquire(t, testAppID, dataLockPath(t), 0)
	defer first.Release()

	secondPath := dataLockPath(t)
	second, err := Acquire(Options{AppID: testAppID, DataPath: secondPath, Wait: 0})
	if err == nil {
		_ = second.Release()
		t.Fatal("同一 appID 换一个数据目录也不应能二次加锁")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("应返回 ErrLocked，实际: %v", err)
	}
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("应能断言为 *LockedError: %v", err)
	}
	// 报错必须能定位到"哪个 bot、哪台机、谁占着"，否则用户只能猜。
	if locked.Holder.AppID != testAppID {
		t.Fatalf("冲突信息应指出持有者的 appID: %+v", locked.Holder)
	}
	if locked.Holder.Host == "" {
		t.Fatalf("冲突信息应指出持有者所在主机: %+v", locked.Holder)
	}
	for _, want := range []string{"另一个实例正在运行", testAppID} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案应包含 %q: %v", want, err)
		}
	}
}

func TestSecondAcquireIsRejected(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	first := acquire(t, testAppID, path, 0)
	defer first.Release()

	start := time.Now()
	second, err := Acquire(Options{AppID: testAppID, DataPath: path, Wait: 0})
	if err == nil {
		_ = second.Release()
		t.Fatal("同一 appID + 同一数据目录不应能二次加锁")
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

// TestDifferentAppIDDifferentDataDirBothAcquire 保证 appID 锁没把"另一台 bot"
// 也串行化：不同 appID 就是不同实例，各用各的数据目录时互不相干。
func TestDifferentAppIDDifferentDataDirBothAcquire(t *testing.T) {
	isolateTemp(t, t.TempDir())
	first := acquire(t, testAppID, dataLockPath(t), 0)
	defer first.Release()

	second := acquire(t, otherAppID, dataLockPath(t), 0)
	defer second.Release()

	if first.AppPath() == second.AppPath() {
		t.Fatal("不同 appID 不应落在同一个锁文件上")
	}
}

// TestDifferentAppIDSameDataDirBlockedByDataLock 说明两把锁的分工：appID 锁
// 不管另一个 bot，但数据目录锁仍然挡住"两个 bot 共用一份 SQLite"——那会把
// 消息表与幂等表混在一起，比双开更难查。
func TestDifferentAppIDSameDataDirBlockedByDataLock(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	first := acquire(t, testAppID, path, 0)
	defer first.Release()

	second, err := Acquire(Options{AppID: otherAppID, DataPath: path, Wait: 0})
	if err == nil {
		_ = second.Release()
		t.Fatal("同一数据目录不应能跑两个进程")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("应返回 ErrLocked，实际: %v", err)
	}
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("应能断言为 *LockedError: %v", err)
	}
	// 挡住它的是数据目录锁，报错必须指向那份库，而不是它的 appID。
	if locked.Path != path {
		t.Fatalf("冲突应指向数据目录锁 %s，实际 %s", path, locked.Path)
	}
}

// TestRejectedAcquireReleasesAppLock 验证两阶段获取的清理：数据目录锁失败时
// 已经拿到的 appID 锁必须放手，否则这个 appID 会被一个没起来的进程永久占住。
func TestRejectedAcquireReleasesAppLock(t *testing.T) {
	isolateTemp(t, t.TempDir())
	shared := dataLockPath(t)
	first := acquire(t, testAppID, shared, 0)
	defer first.Release()

	if second, err := Acquire(Options{AppID: otherAppID, DataPath: shared, Wait: 0}); err == nil {
		_ = second.Release()
		t.Fatal("同一数据目录不应能跑两个进程")
	}

	// 换个数据目录，同一个 appID 必须能起来——说明上一次失败没留下锁。
	lock := acquire(t, otherAppID, dataLockPath(t), 0)
	defer lock.Release()
}

// TestAppIDHashCollisionFallsBackToFullHash 把哈希长度压到 1 位，造出必然发生的
// 前缀碰撞：撞车的两个 appID 必须各锁各的文件（否则一个 bot 会被另一个 bot 挡下），
// 而各自"第二次启动"仍然要被挡住（否则同一 bot 就有两条 WS）。
func TestAppIDHashCollisionFallsBackToFullHash(t *testing.T) {
	isolateTemp(t, t.TempDir())
	old := appHashLen
	appHashLen = 1
	t.Cleanup(func() { appHashLen = old })

	firstID, secondID := collidingAppIDs(t)
	first := acquire(t, firstID, dataLockPath(t), 0)
	defer first.Release()
	second := acquire(t, secondID, dataLockPath(t), 0)
	defer second.Release()

	if first.AppPath() == second.AppPath() {
		t.Fatalf("撞车的两个 appID 不应共用锁文件: %s", first.AppPath())
	}
	// 先来的占着前缀文件，后来的退回全哈希文件名。
	if base := filepath.Base(first.AppPath()); !strings.HasPrefix(base, "qqbot-") || len(base) != len("qqbot-1.lock") {
		t.Fatalf("先来的应占前缀文件，实际 %s", base)
	}
	if base := filepath.Base(second.AppPath()); len(base) != len("qqbot-")+sha256.Size*2+len(".lock") {
		t.Fatalf("撞车后应退回全哈希文件名，实际 %s", base)
	}

	// 各自的第二份仍然要被挡住。
	if l, err := Acquire(Options{AppID: firstID, DataPath: dataLockPath(t)}); !errors.Is(err, ErrLocked) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("撞车后同一 appID 仍不应能双开（前缀文件），实际: %v", err)
	}
	if l, err := Acquire(Options{AppID: secondID, DataPath: dataLockPath(t)}); !errors.Is(err, ErrLocked) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("撞车后同一 appID 仍不应能双开（全哈希文件），实际: %v", err)
	}

	// 前缀文件空出来时里面残留的仍是 firstID。secondID 的新进程若把这只空文件
	// 当成自己的锁，就会和还在跑的 second 组成双开——必须仍然被挡住。
	if err := first.Release(); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if l, err := Acquire(Options{AppID: secondID, DataPath: dataLockPath(t)}); !errors.Is(err, ErrLocked) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("前缀文件换手后仍不应能双开，实际: %v", err)
	}
}

// collidingAppIDs 返回两个 sha256 首位（十六进制）相同的 appID。
func collidingAppIDs(t *testing.T) (string, string) {
	t.Helper()
	seen := make(map[string]string)
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("collide-%d", i)
		sum := sha256.Sum256([]byte(id))
		prefix := hex.EncodeToString(sum[:])[:1]
		if other, ok := seen[prefix]; ok {
			return other, id
		}
		seen[prefix] = id
	}
	t.Fatal("64 个候选里没有前缀碰撞，说明哈希算错了")
	return "", ""
}

func TestReleaseAllowsReacquire(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	first := acquire(t, testAppID, path, 0)
	if err := first.Release(); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	// 重复释放不能 panic：defer 与显式释放同时存在是很常见的写法。
	if err := first.Release(); err != nil {
		t.Fatalf("重复释放应无害: %v", err)
	}

	second := acquire(t, testAppID, path, 0)
	defer second.Release()
}

func TestAcquireWaitsForRelease(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	first := acquire(t, testAppID, path, 0)

	// 模拟旧进程收尾：200ms 后才放手，等待方应当拿到锁而不是直接失败。
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = first.Release()
	}()

	start := time.Now()
	second := acquire(t, testAppID, path, 3*time.Second)
	defer second.Release()

	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("应当真的等过一段时间，实际 %s", d)
	}
	if second.Holder().PID != os.Getpid() {
		t.Fatalf("新锁的持有者应是当前进程: %+v", second.Holder())
	}
}

func TestAcquireTimesOutWhileHeld(t *testing.T) {
	isolateTemp(t, t.TempDir())
	path := dataLockPath(t)
	first := acquire(t, testAppID, path, 0)
	defer first.Release()

	start := time.Now()
	_, err := Acquire(Options{AppID: testAppID, DataPath: path, Wait: 300 * time.Millisecond})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("超时后应返回 ErrLocked，实际: %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("应等满等待窗口，实际 %s", d)
	}
}

const (
	helperPathEnv = "QQBOT_LOCK_HELPER_PATH"
	helperTmpEnv  = "QQBOT_LOCK_HELPER_TMPDIR"
)

// TestHelperHoldsLock 只作为子进程运行，用来验证跨进程互斥。
// 直接跑本用例时它会跳过。
func TestHelperHoldsLock(t *testing.T) {
	path := os.Getenv(helperPathEnv)
	if path == "" {
		t.Skip("仅作为跨进程用例的子进程助手运行")
	}
	// 子进程要跟父进程用同一个 appID 锁目录，否则两边锁的是不同文件，
	// 互斥就测不出来了。
	if dir := os.Getenv(helperTmpEnv); dir != "" {
		tempDir = func() string { return dir }
	}
	lock, err := Acquire(Options{AppID: testAppID, DataPath: path})
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
	if os.Getenv(helperPathEnv) != "" {
		t.Skip("父进程用例")
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("该平台无进程模型")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}

	lockDir := t.TempDir()
	isolateTemp(t, lockDir)
	path := dataLockPath(t)
	cmd := exec.Command(exe, "-test.run=^TestHelperHoldsLock$", "-test.v")
	cmd.Env = append(os.Environ(),
		helperPathEnv+"="+path,
		helperTmpEnv+"="+lockDir,
	)
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
	if l, err := Acquire(Options{AppID: testAppID, DataPath: path, Wait: 200 * time.Millisecond}); !errors.Is(err, ErrLocked) {
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

	lock := acquire(t, testAppID, path, 5*time.Second)
	defer lock.Release()
}
