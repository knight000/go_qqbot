// Command echo 是 echo 插件的独立产物入口。
//
// 产物形态是「一个插件一个可执行文件」（ADR-0005）：核心按
// <datadir>/plugins/bin/<name> 发现它、用 --manifest 探测清单，然后 spawn。
// 所以这里只做一件事——把包里的 Plugin 交给 rpcsdk.Serve。
package main

import (
	"fmt"
	"os"

	"github.com/knight000/go_qqbot/pkg/rpcsdk"
	"github.com/knight000/go_qqbot/plugins/echo"
)

func main() {
	if err := rpcsdk.Serve(&echo.Plugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "echo:", err)
		os.Exit(1)
	}
}
