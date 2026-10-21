//go:build linux

package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// cpuTimes 读 /proc/stat 首行，返回 (idle+iowait, 总时间)，单位 jiffies。
// kernel 不把 iowait 计入 idle，但对使用率而言它确实是"空转"。
func cpuTimes() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("打开 /proc/stat: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		// user nice system idle iowait irq softirq steal
		var v [8]uint64
		for i := 0; i < len(fields) && i < len(v); i++ {
			n, e := strconv.ParseUint(fields[i], 10, 64)
			if e != nil {
				return 0, 0, fmt.Errorf("解析 /proc/stat: %w", e)
			}
			v[i] = n
		}
		idle = v[3] + v[4]
		for _, n := range v {
			total += n
		}
		return idle, total, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("读取 /proc/stat: %w", err)
	}
	return 0, 0, fmt.Errorf("/proc/stat 缺少 cpu 汇总行")
}

// readMem 读 /proc/meminfo 的 MemTotal / MemAvailable（单位 kB）。
func readMem() (total, avail uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("打开 /proc/meminfo: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = parseKB(fields[1])
		case "MemAvailable:":
			avail = parseKB(fields[1])
		}
		if total > 0 && avail > 0 {
			return total, avail, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("读取 /proc/meminfo: %w", err)
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("/proc/meminfo 缺少 MemTotal")
	}
	// 老内核（<3.14）没有 MemAvailable，退回 MemFree+Buffers+Cached
	if avail == 0 {
		avail = total / 10
	}
	return total, avail, nil
}

func parseKB(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * 1024
}

// processMemory 读 /proc/self/status 的 VmRSS 与 VmHWM（峰值常驻）。
// 两个值取自同一个快照，所以峰值一定不小于当前值；分成两次读（statm +
// status）会在两次读取之间自相矛盾。
func processMemory() (rss, peak uint64, err error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, 0, fmt.Errorf("打开 /proc/self/status: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			rss = parseKB(fields[1])
		case "VmHWM:":
			peak = parseKB(fields[1])
		}
		if rss > 0 && peak > 0 {
			return rss, peak, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("读取 /proc/self/status: %w", err)
	}
	if rss == 0 {
		return 0, 0, fmt.Errorf("/proc/self/status 缺少 VmRSS")
	}
	return 0, 0, fmt.Errorf("/proc/self/status 缺少 VmHWM")
}

// diskUsage 用 statfs 取分区容量。Bavail 是普通用户可用块数。
func diskUsage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, nil
}
