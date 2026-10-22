// Package instance 保证这台机器上一个 bot 只有一个进程在跑，一份数据目录也只被
// 一个进程打开。
//
// 为什么这是必需的：v2 网关的 session 按 appID 分配，两个进程同时连接会互相顶掉，
// 日志里表现为反复出现 4902 reset by resume；同时两个进程各回一遍消息、各记一遍
// 幂等表，业务上已经是错的。SQLite 的 WAL 虽然支持多进程，但那不是让两个 bot
// 共用一份库的理由——消息表与幂等表会混在一起，排查时无从下手。
//
// 所以锁分两个维度，按宽到窄的顺序取：
//
//  1. appID 维度：%TMP%/qqbot-<sha256(appID)[:16]>.lock。约束的是"这台机器上的
//     同一个 bot"。同一 appID 配两份数据目录（测试库与正式库并存，或换了目录但
//     旧进程还在）时，靠它挡住第二条 WS。锁文件必须放在系统临时目录：它跟数据
//     目录在哪无关，放在库旁边就又会退化成"换个目录就能双开"。
//  2. 数据目录维度：<store.path>.lock。约束的是一份 SQLite。
//
// 先 appID 后目录，是为了让最常见的双开（同一 bot、换个目录）在第一步就被挡住，
// 报错指向 appID 而不是一份陌生的库路径。
//
// 实现用操作系统级文件锁（Unix flock / Windows 独占共享模式），而不是"锁文件
// 是否存在"：进程被 kill -9 或任务管理器结束时内核会关闭句柄，锁自动释放，
// 不存在需要人工清理的残留锁。也正因如此，锁文件永不删除——删除会引入
// "删掉别人正在持有的锁"的竞态。
package instance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrLocked 表示锁被另一个仍存活的进程持有。用 errors.Is 判定。
var ErrLocked = errors.New("锁已被占用")

// errBusy 是平台层"这一刻拿不到锁"的内部信号，只用于决定是否重试。
var errBusy = errors.New("busy")

const (
	// retryInterval 是等待期间的重试间隔。150ms 意味着 /reload 这种
	// "旧进程刚放手"的场景最多多花这么点时间，用户感知不到。
	retryInterval = 150 * time.Millisecond
	// holderLimit 限制读回元数据的大小，避免把垃圾文件整个读进内存。
	holderLimit = 4 << 10
)

// appHashLen 是 appID 锁文件名里哈希的长度（十六进制字符数）。16 位即 64 bit，
// 撞车概率可以忽略；真撞上了由 acquireApp 退回全哈希，见那里。
// 变量而非常量只为让测试把长度压到 1 位，好在可枚举的范围里造出一次碰撞。
var appHashLen = 16

// tempDir 是 appID 锁文件所在目录的默认值。用变量而非常量，只为让测试把
// 锁文件收进 t.TempDir()：锁文件按设计永不删除，测试不该往真实临时目录里堆垃圾。
var tempDir = os.TempDir

// LockDirEnv 指定 appID 锁文件所在目录，空则用系统临时目录。
//
// 为什么不能只依赖 os.TempDir()：部署单元设了 PrivateTmp=true（见
// deploy/qqbot.service），/tmp 对每个 unit 是**私有命名空间**。同一 appID
// 的第二份部署（另一个 unit，或运维手工起的进程）会看到另一个 /tmp，
// 两把"同一把"锁互不可见，双开照样发生。所以 unit 里用
// RuntimeDirectory=qqbot + QQBOT_LOCK_DIR=/run/qqbot 把锁放到共享位置。
const LockDirEnv = "QQBOT_LOCK_DIR"

// lockDir 返回 appID 锁目录。设了 LockDirEnv 就只用它，不回退——静默回退
// 等于把 PrivateTmp 那个坑原地埋回去。
func lockDir() string {
	if d := strings.TrimSpace(os.Getenv(LockDirEnv)); d != "" {
		// 部署单元由 RuntimeDirectory 建好目录；手工设置环境变量时兜一层，
		// 建不出来就留给后续加锁报错，不要偷偷换成临时目录。
		_ = os.MkdirAll(d, 0o700)
		return d
	}
	return tempDir()
}

// Options 描述一次加锁。
type Options struct {
	// AppID 是网关 session 的归属，同一 appID 全机只允许一个进程。必填：
	// 为空时直接报错，静默退化成"只锁数据目录"正是被修掉的那个缺陷。
	AppID string
	// DataPath 是数据目录维度的锁文件路径，通常是 <store.path>.lock。
	DataPath string
	// Wait 是每把锁各自的等待上限（两把锁各有一份预算，不是共享）。
	Wait time.Duration
}

// Holder 是写进锁文件的持有者信息。它只服务于一件事：拿不到锁时
// 告诉用户"是谁占着、哪个 bot、哪台机"，好在任务管理器或 ps 里精确地结束
// 那个进程，而不是猜。
type Holder struct {
	AppID   string    `json:"app_id"`
	PID     int       `json:"pid"`
	Exe     string    `json:"exe"`
	Host    string    `json:"host"`
	Started time.Time `json:"started"`
}

// fileLock 是一把已经持有的单文件锁。
type fileLock struct {
	path string
	file *os.File
	// holder 是本进程写进去的信息；prev 是写之前文件里残留的上一位持有者。
	// prev 只有一个用途：让 appID 锁看出"这个文件上次是别的 appID 在用"，
	// 也就是哈希前缀撞车，见 acquireApp。
	holder Holder
	prev   Holder
}

// release 关闭句柄把锁交还出去。可重复调用。
func (f *fileLock) release() error {
	if f == nil || f.file == nil {
		return nil
	}
	file := f.file
	f.file = nil
	return file.Close()
}

// Lock 是本次启动持有的两把锁。
type Lock struct {
	app *fileLock
	dir *fileLock
}

// Path 返回数据目录锁文件路径，供启动日志记录。
func (l *Lock) Path() string { return l.dir.path }

// AppPath 返回 appID 锁文件路径，供启动日志记录。
func (l *Lock) AppPath() string { return l.app.path }

// Holder 返回本进程写在锁文件里的信息。
func (l *Lock) Holder() Holder { return l.dir.holder }

// Release 依次放开两把锁。可重复调用。
//
// 反序释放（先目录后 appID）：appID 锁最后放手，新进程要等它空出来才能确认
// "这台机器上的 bot 确实没人跑"，早放一刻没有好处。
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	err := l.dir.release()
	return errors.Join(err, l.app.release())
}

// Scope 说明是哪一把锁挡住的。两种冲突该做的事完全不同：appID 冲突要去结束
// 那个进程，数据目录冲突要改 [store].path——所以错误文案必须能区分。
type Scope string

const (
	// ScopeAppID 是"同一台机器上同一个 bot 已经有一个进程在跑"。
	ScopeAppID Scope = "appID"
	// ScopeDataDir 是"这份数据目录已经被另一个进程打开"。
	ScopeDataDir Scope = "数据目录"
)

// LockedError 说明锁被谁占着。
type LockedError struct {
	Scope  Scope
	Path   string
	Holder Holder
}

func (e *LockedError) Error() string {
	h := e.Holder
	if h.PID == 0 {
		return fmt.Sprintf("另一个实例正在运行：%s已被占用（读取 %s 的持有者信息失败）",
			e.Scope.subject(), e.Path)
	}
	switch e.Scope {
	case ScopeAppID:
		return fmt.Sprintf("另一个实例正在运行：appID %s 已被占用——%s持有锁 %s",
			orUnknown(h.AppID, "未知"), describeHolder(h, ""), e.Path)
	case ScopeDataDir:
		return fmt.Sprintf("另一个实例正在运行：数据目录已被占用——%s持有锁 %s；"+
			"两个 bot 不要共用同一份库，请改 [store].path",
			describeHolder(h, "appID "+orUnknown(h.AppID, "未知")), e.Path)
	}
	return fmt.Sprintf("另一个实例正在运行：%s 已被占用——%s持有锁 %s",
		e.Scope, describeHolder(h, ""), e.Path)
}

// subject 给"读取持有者信息失败"那句填主语。
func (s Scope) subject() string {
	switch s {
	case ScopeAppID:
		return "该 appID "
	case ScopeDataDir:
		return "该数据目录"
	}
	return string(s) + " "
}

// describeHolder 把持有者拼成一句话里能用的短语。extra 插在 exe 之前，
// 让"数据目录冲突"能在同一个括号里点出占用者的 appID。
func describeHolder(h Holder, extra string) string {
	parts := make([]string, 0, 4)
	if extra != "" {
		parts = append(parts, extra)
	}
	parts = append(parts,
		orUnknown(h.Exe, "路径未知"),
		"启动于 "+h.Started.Format("2006-01-02 15:04:05"),
		"主机 "+orUnknown(h.Host, "未知"))
	return fmt.Sprintf("pid %d（%s）", h.PID, strings.Join(parts, "，"))
}

func orUnknown(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Unwrap 让 errors.Is(err, ErrLocked) 成立。
func (e *LockedError) Unwrap() error { return ErrLocked }

// Acquire 先取 appID 维度的锁，再取数据目录维度的锁。
//
// Wait > 0 时，若锁正被占用则每隔 retryInterval 重试，直到成功或超时。
// 这专为 /reload 而设：Windows 上"先拉起新进程再退出旧进程"、Unix 上
// execve 后新映像重新加锁，都会短暂撞上还没放手的旧锁。
// 第二个实例该用 Wait = 0，立刻失败才是正确行为。
//
// 锁被占用时返回的错误满足 errors.Is(err, ErrLocked)，可断言为 *LockedError
// 读取持有者信息。
func Acquire(opts Options) (*Lock, error) {
	if strings.TrimSpace(opts.AppID) == "" {
		return nil, errors.New("单实例锁需要 appID：缺了它就只能按数据目录划分，同一 bot 换个目录仍会双开")
	}

	app, err := acquireApp(opts)
	if err != nil {
		return nil, err
	}
	dir, err := acquireFile(opts.DataPath, ScopeDataDir, opts)
	if err != nil {
		// 第二把没拿到就必须放手第一把，否则这个 appID 会被一个根本没起来的
		// 进程占住，直到它退出。
		_ = app.release()
		return nil, err
	}
	return &Lock{app: app, dir: dir}, nil
}

// acquireApp 取 appID 维度的锁。
//
// 锁文件名用哈希前缀，理论上有两个 appID 落到同一个文件（前缀碰撞）的可能。
// 撞车时**不能**让它们共用一把锁：那样一个 bot 的进程会被另一个 bot 挡下，
// 而真正该保证的"同一 appID 只有一个进程"反而失效。做法是把锁文件里的 appID
// 当唯一真相：
//
//   - 拿到前缀文件，但里面残留的持有者 appID 不是自己 → 这个文件归别人，
//     还给别人，改锁全哈希文件（首次运行文件是空的，不会误判）；
//   - 前缀文件被别人占着，且占着的 appID 不是自己 → 同样是撞车，改锁全哈希；
//   - 占着的 appID 就是自己 → 真的双开，报错。
//
// 三条合起来保证：同一个 appID 无论走哪个文件，全机只有一个进程能拿到它。
func acquireApp(opts Options) (*fileLock, error) {
	appID := opts.AppID
	primary, full := appLockPaths(appID)
	lock, err := acquireFile(primary, ScopeAppID, opts)
	if err == nil {
		if lock.prev.AppID != "" && lock.prev.AppID != appID {
			// 前缀文件空出来了，但上次用它的不是自己：一旦占着不放，撞车的两个
			// appID 会在"文件空出来"的时刻轮流把它当成自己的锁，同一 bot 就有
			// 两条 WS 了。放手，去锁自己的全哈希文件。
			_ = lock.release()
			return acquireFile(full, ScopeAppID, opts)
		}
		return lock, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, err
	}
	var locked *LockedError
	if errors.As(err, &locked) && locked.Holder.AppID != "" && locked.Holder.AppID != appID {
		return acquireFile(full, ScopeAppID, opts)
	}
	return nil, err
}

// appLockPaths 返回 appID 锁的两个候选文件路径。primary 用哈希前缀，便于人工
// 一眼认出"这是同一把锁"；full 只在前缀碰撞时启用。
func appLockPaths(appID string) (primary, full string) {
	sum := sha256.Sum256([]byte(appID))
	h := hex.EncodeToString(sum[:])
	dir := lockDir()
	return filepath.Join(dir, "qqbot-"+h[:appHashLen]+".lock"),
		filepath.Join(dir, "qqbot-"+h+".lock")
}

// acquireFile 在 path 上取独占锁并在拿到后写入持有者信息。
// scope 只用于冲突时把"哪一把锁挡住的"写进错误文案。
func acquireFile(path string, scope Scope, opts Options) (*fileLock, error) {
	// 转成绝对路径：错误消息里出现 "./.temp/data/qqbot.db.lock" 这种相对路径时，
	// 用户还得先猜是相对于谁——报错文案的价值就在于能直接照着去查。
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建锁文件目录: %w", err)
	}

	deadline := time.Now().Add(opts.Wait)
	for {
		f, err := lockFile(path)
		if err == nil {
			return adopt(path, f, opts.AppID)
		}
		if !errors.Is(err, errBusy) {
			return nil, fmt.Errorf("打开锁文件 %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			return nil, &LockedError{Scope: scope, Path: path, Holder: readHolder(path)}
		}
		time.Sleep(retryInterval)
	}
}

// adopt 把刚拿到的句柄包装成 fileLock，并写入持有者信息。
func adopt(path string, f *os.File, appID string) (*fileLock, error) {
	// 先读再写：读到的是上一位持有者留下的内容，只有存不存在自己之外的 appID
	// 这一个用途。
	prev := readHolder(path)
	h := Holder{AppID: appID, PID: os.Getpid(), Started: time.Now().Truncate(time.Second)}
	if exe, err := os.Executable(); err == nil {
		h.Exe = exe
	}
	if host, err := os.Hostname(); err == nil {
		h.Host = host
	}
	if err := writeHolder(f, h); err != nil {
		// 写不进去不影响锁本身的有效性，但会是"未来某次排障时缺一条线索"。
		// 这里仍然失败：拿不到元数据多半意味着磁盘或权限有问题，早报早好。
		_ = f.Close()
		return nil, fmt.Errorf("写入锁文件 %s: %w", path, err)
	}
	return &fileLock{path: path, file: f, holder: h, prev: prev}, nil
}

func writeHolder(f *os.File, h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(b, 0)
	return err
}

// readHolder 尽力读回持有者信息：读不到只降级错误文案，不影响判定。
func readHolder(path string) Holder {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > holderLimit {
		return Holder{}
	}
	var h Holder
	if err := json.Unmarshal(bytes.TrimSpace(b), &h); err != nil {
		return Holder{}
	}
	return h
}
