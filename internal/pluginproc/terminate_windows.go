//go:build windows

package pluginproc

import "os"

// terminate 在 Windows 上只能 Kill。
//
// Windows 没有可以发给**别的进程**的 SIGTERM：os.Interrupt 也发不过去
// （它对应 Ctrl-C，只能给同控制台进程组，而我们起的子进程不是那组成员）。
// 所以第二、三级在 Windows 上合成一步——反正结果一样是"进程没了"，而
// 第一级（RPC close）才是让插件收尾的地方，它在两个平台上都先跑。
func terminate(proc *os.Process) error {
	return proc.Kill()
}
