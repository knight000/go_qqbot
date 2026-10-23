//go:build windows

package pluginproc_test

import "syscall"

// processAlive 报告 pid 是否还活着。
//
// 不走 tasklist：它的输出格式与本地化措辞都不稳定，而"关停后不留孤儿"
// 是本票的硬验收。OpenProcess + GetExitCodeProcess 是 Windows 上判断
// "这个 pid 对应的进程还在不在"的最小可靠手段（取不到句柄＝已经没了）。
func processAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	const stillActive = 259

	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
