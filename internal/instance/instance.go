// Package instance 保证一份数据目录同时只有一个 bot 进程在跑。
//
// 为什么这是必需的：v2 网关的 session 按 appID 分配，两个进程同时连接会互相顶掉，
// 日志里表现为反复出现 4902 reset by resume；同时两个进程各回一遍消息、各记一遍
// 幂等表，业务上已经是错的。SQLite 的 WAL 虽然支持多进程，但那不是让两个 bot
// 共用一个库的理由。
//
// 实现用操作系统级文件锁（Unix flock / Windows 独占共享模式），而不是"锁文件
// 是否存在"：进程被 kill -9 或任务管理器结束时内核会关闭句柄，锁自动释放，
// 不存在需要人工清理的残留锁。也正因如此，锁文件永不删除——删除会引入
// "删掉别人正在持有的锁"的竞态。
package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Holder 是写进锁文件的持有者信息。它只服务于一件事：拿不到锁时
// 告诉用户"是谁占着"，好在任务管理器或 ps 里精确地结束那个进程。
type Holder struct {
	PID     int       `json:"pid"`
	Exe     string    `json:"exe"`
	Started time.Time `json:"started"`
}

// Lock 是一个已经持有的独占锁。进程退出时无需显式释放（内核会做），
// 但正常退出路径应当调用 Release，让下一个实例不必等锁。
type Lock struct {
	path   string
	file   *os.File
	holder Holder
}

// Path 返回锁文件路径，供启动日志记录。
func (l *Lock) Path() string { return l.path }

// Holder 返回本进程写在锁文件里的信息。
func (l *Lock) Holder() Holder { return l.holder }

// Release 关闭锁文件句柄，把锁交还出去。可重复调用。
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}

// LockedError 说明锁被谁占着。
type LockedError struct {
	Path   string
	Holder Holder
}

func (e *LockedError) Error() string {
	h := e.Holder
	if h.PID == 0 {
		return fmt.Sprintf("另一个实例正在运行：%s 已被占用（读取持有者信息失败）", e.Path)
	}
	exe := h.Exe
	if exe == "" {
		exe = "路径未知"
	}
	return fmt.Sprintf("另一个实例正在运行：pid %d（%s，启动于 %s）持有 %s",
		h.PID, exe, h.Started.Format("2006-01-02 15:04:05"), e.Path)
}

// Unwrap 让 errors.Is(err, ErrLocked) 成立。
func (e *LockedError) Unwrap() error { return ErrLocked }

// Acquire 在 path 上取得独占锁。
//
// wait > 0 时，若锁正被占用则每隔 retryInterval 重试，直到成功或超时。
// 这专为 /reload 而设：Windows 上"先拉起新进程再退出旧进程"、Unix 上
// execve 后新映像重新加锁，都会短暂撞上还没放手的旧锁。
// 第二个实例该用 wait = 0，立刻失败才是正确行为。
//
// 锁被占用时返回的错误满足 errors.Is(err, ErrLocked)，可断言为 *LockedError
// 读取持有者信息。
func Acquire(path string, wait time.Duration) (*Lock, error) {
	// 转成绝对路径：错误消息里出现 "./.temp/data/qqbot.db.lock" 这种相对路径时，
	// 用户还得先猜是相对于谁——报错文案的价值就在于能直接照着去查。
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建锁文件目录: %w", err)
	}

	deadline := time.Now().Add(wait)
	for {
		f, err := lockFile(path)
		if err == nil {
			return adopt(path, f)
		}
		if !errors.Is(err, errBusy) {
			return nil, fmt.Errorf("打开锁文件 %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			return nil, &LockedError{Path: path, Holder: readHolder(path)}
		}
		time.Sleep(retryInterval)
	}
}

// adopt 把刚拿到的句柄包装成 Lock，并写入持有者信息。
func adopt(path string, f *os.File) (*Lock, error) {
	h := Holder{PID: os.Getpid(), Started: time.Now().Truncate(time.Second)}
	if exe, err := os.Executable(); err == nil {
		h.Exe = exe
	}
	if err := writeHolder(f, h); err != nil {
		// 写不进去不影响锁本身的有效性，但会是"未来某次排障时缺一条线索"。
		// 这里仍然失败：拿不到元数据多半意味着磁盘或权限有问题，早报早好。
		_ = f.Close()
		return nil, fmt.Errorf("写入锁文件 %s: %w", path, err)
	}
	return &Lock{path: path, file: f, holder: h}, nil
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
