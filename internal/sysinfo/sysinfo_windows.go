//go:build windows

package sysinfo

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 只用 kernel32/psapi，不引入 x/sys，保证单个二进制、无 cgo。
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	psapi    = syscall.NewLazyDLL("psapi.dll")

	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// filetime 是两个 DWORD，按小端拼成 100ns 计数。
type filetime struct {
	Low  uint32
	High uint32
}

func (f filetime) u64() uint64 { return uint64(f.Low) | uint64(f.High)<<32 }

// cpuTimes 返回 (空闲时间, 总时间)。GetSystemTimes 的 kernel 时间已含 idle，
// 所以总时间 = kernel + user。
func cpuTimes() (idle, total uint64, err error) {
	var i, k, u filetime
	r, _, e := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&i)),
		uintptr(unsafe.Pointer(&k)),
		uintptr(unsafe.Pointer(&u)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetSystemTimes: %v", e)
	}
	return i.u64(), k.u64() + u.u64(), nil
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readMem() (total, avail uint64, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx: %v", e)
	}
	return m.TotalPhys, m.AvailPhys, nil
}

// processMemoryCountersEx 对应 PROCESS_MEMORY_COUNTERS_EX。
// 两个 uint32 之后才是 SIZE_T，Go 的 8 字节对齐与 C 结构体一致。
type processMemoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// processMemory 用 GetProcessMemoryInfo 取当前进程的 WorkingSetSize 与
// PeakWorkingSetSize。两者来自同一次调用，峰值一定不小于当前值。
func processMemory() (rss, peak uint64, err error) {
	h, _, _ := procGetCurrentProcess.Call()
	var c processMemoryCountersEx
	c.Cb = uint32(unsafe.Sizeof(c))
	r, _, e := procGetProcessMemoryInfo.Call(
		h,
		uintptr(unsafe.Pointer(&c)),
		uintptr(c.Cb),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetProcessMemoryInfo: %v", e)
	}
	return uint64(c.WorkingSetSize), uint64(c.PeakWorkingSetSize), nil
}

// diskUsage 取路径所在分区的总量与可用量（配额感知）。
func diskUsage(path string) (total, free uint64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var availToCaller, totalBytes, totalFree uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&availToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceExW %s: %v", path, e)
	}
	_ = totalFree
	return totalBytes, availToCaller, nil
}
