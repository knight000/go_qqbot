// Package sysinfo 采集本机与当前进程的资源占用。
//
// 设计约束：
//   - 只用标准库，不引入 cgo（发布物是单二进制交叉编译）；
//   - Windows 走 kernel32/psapi，Linux 走 /proc 与 statfs；
//   - 所有采集都有超时/降级，指标不可用时返回错误而不是 panic，
//     让调用方（如 /ping 插件）把"不可用"如实展示出来。
package sysinfo

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// StartTime 是进程启动时刻，用于计算"机器人连续运行时长"。
// 进程重启即归零，这正是"连续开启"的语义。
var StartTime = time.Now()

// CPU 是整机 CPU 使用率快照。
type CPU struct {
	Percent float64 // 0–100
	Cores   int     // 逻辑核数
}

// Memory 是整机内存快照。
type Memory struct {
	Total   uint64 // 字节
	Used    uint64 // Total - Available
	Percent float64
}

// ProcessMemory 是当前进程的内存占用。
type ProcessMemory struct {
	RSS  uint64 // 当前常驻（working set），字节
	Peak uint64 // 进程生命周期内的峰值常驻，字节
}

// Disk 是某个路径所在分区的容量。
type Disk struct {
	Path  string // 用于定位分区的路径
	Total uint64
	Free  uint64 // 普通用户可用
	Used  uint64
}

// DirUsage 是目录占用统计结果。
type DirUsage struct {
	Path       string
	Bytes      int64
	Files      int
	Partial    bool // 超时或权限受限，结果是下限
	IgnoredGit bool // 跳过了 .git
}

// cpuSampleTTL 是 CPU 使用率的复用窗口：窗口内重复调用直接返回上次结果，
// 避免每次 /ping 都阻塞 200ms。
const cpuSampleTTL = 5 * time.Second

var cpuCache struct {
	mu  sync.Mutex
	pct float64
	at  time.Time
}

// ReadCPU 返回整机 CPU 使用率。首次或缓存过期时做两次采样求差值，
// 采样窗口 200ms，因此最坏多花 200ms。
func ReadCPU(ctx context.Context) (CPU, error) {
	cpuCache.mu.Lock()
	defer cpuCache.mu.Unlock()

	if !cpuCache.at.IsZero() && time.Since(cpuCache.at) < cpuSampleTTL {
		return CPU{Percent: cpuCache.pct, Cores: runtime.NumCPU()}, nil
	}

	idle0, total0, err := cpuTimes()
	if err != nil {
		return CPU{}, err
	}

	t := time.NewTimer(200 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return CPU{}, ctx.Err()
	case <-t.C:
	}

	idle1, total1, err := cpuTimes()
	if err != nil {
		return CPU{}, err
	}

	pct := 0.0
	if dTotal := total1 - total0; dTotal > 0 {
		dIdle := idle1 - idle0
		pct = 100 * float64(dTotal-dIdle) / float64(dTotal)
	}
	pct = clampPercent(pct)

	cpuCache.pct, cpuCache.at = pct, time.Now()
	return CPU{Percent: pct, Cores: runtime.NumCPU()}, nil
}

// ReadMemory 返回整机内存占用。
func ReadMemory() (Memory, error) {
	total, avail, err := readMem()
	if err != nil {
		return Memory{}, err
	}
	if total == 0 {
		return Memory{}, fmt.Errorf("内存总量为 0")
	}
	used := uint64(0)
	if total > avail {
		used = total - avail
	}
	return Memory{
		Total:   total,
		Used:    used,
		Percent: clampPercent(100 * float64(used) / float64(total)),
	}, nil
}

// ReadProcessMemory 返回当前进程的常驻内存与峰值常驻内存。
//
// 与 ReadMemory 分开：整机内存与本进程内存是两个独立的数据源，一个取不到
// 不该把另一个也抹掉。取不到时返回错误而不是 0——"0 字节"和"没采到"在
// /ping 上是两种完全不同的结论（评审 §3.3 P3-25）。
func ReadProcessMemory() (ProcessMemory, error) {
	rss, peak, err := processMemory()
	if err != nil {
		return ProcessMemory{}, err
	}
	return ProcessMemory{RSS: rss, Peak: peak}, nil
}

// ReadDisk 返回 path 所在分区的容量。path 不存在时取其最近的已存在父目录。
func ReadDisk(path string) (Disk, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Disk{}, err
	}
	target := nearestExisting(abs)
	if target == "" {
		return Disk{}, fmt.Errorf("找不到可用的路径前缀: %s", abs)
	}
	total, free, err := diskUsage(target)
	if err != nil {
		return Disk{}, err
	}
	if total == 0 {
		return Disk{}, fmt.Errorf("分区容量为 0: %s", abs)
	}
	used := uint64(0)
	if total > free {
		used = total - free
	}
	return Disk{Path: abs, Total: total, Free: free, Used: used}, nil
}

func nearestExisting(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path || parent == "" {
			return ""
		}
		path = parent
	}
}

// dirBudget 限制目录遍历耗时，避免在大目录上把 /ping 拖到超时。
const dirBudget = 1500 * time.Millisecond

// DirSize 统计目录占用。跳过 .git（非运行时数据且可能极大）、
// 不跟随符号链接/目录联接，遇到权限错误按部分统计处理。
func DirSize(ctx context.Context, root string) (DirUsage, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return DirUsage{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return DirUsage{}, err
	}
	if !info.IsDir() {
		return DirUsage{}, fmt.Errorf("不是目录: %s", abs)
	}

	u := DirUsage{Path: abs}
	deadline := time.Now().Add(dirBudget)
	seen := 0

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 权限不足/文件被删：记部分结果继续，不中断整体统计
			u.Partial = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		seen++
		if seen%256 == 0 {
			if time.Now().After(deadline) || ctx.Err() != nil {
				u.Partial = true
				return filepath.SkipAll
			}
		}

		if d.IsDir() {
			if d.Name() == ".git" {
				u.IgnoredGit = true
				return fs.SkipDir
			}
			return nil
		}
		// 不跟随 link / junction，避免重复计数或跨盘递归
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			u.Partial = true
			return nil
		}
		u.Bytes += fi.Size()
		u.Files++
		return nil
	})
	if walkErr != nil {
		return u, walkErr
	}
	return u, nil
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// HumanBytes 以 1024 进制格式化字节数。
func HumanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if i < 0 {
		i = 0
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// HumanDuration 输出中文可读时长，最多两级精度：
// 3天2小时 / 2小时13分 / 13分20秒 / 20秒。
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	d -= mins * time.Minute
	secs := d / time.Second

	switch {
	case days > 0:
		return fmt.Sprintf("%d天%d小时", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d小时%d分", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%d分%d秒", mins, secs)
	default:
		return fmt.Sprintf("%d秒", secs)
	}
}
