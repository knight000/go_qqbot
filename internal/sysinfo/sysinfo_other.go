//go:build !windows && !linux

package sysinfo

import "fmt"

// 其他平台只保证能编译，指标一律报"不可用"，避免因构建标签漏配而失败。

func cpuTimes() (idle, total uint64, err error) {
	return 0, 0, fmt.Errorf("当前平台不支持 CPU 采集")
}

func readMem() (total, avail uint64, err error) {
	return 0, 0, fmt.Errorf("当前平台不支持内存采集")
}

func processRSS() uint64 { return 0 }

func diskUsage(path string) (total, free uint64, err error) {
	return 0, 0, fmt.Errorf("当前平台不支持磁盘采集")
}
