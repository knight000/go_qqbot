// Command adminping 是 adminping 插件的独立产物入口。
//
// 产物形态是「一个插件一个可执行文件」（ADR-0005）：核心按
// <datadir>/plugins/bin/<name> 发现它、用 --manifest 探测清单，然后 spawn。
package main

import (
	"fmt"
	"os"

	"github.com/knight000/go_qqbot/pkg/rpcsdk"
	"github.com/knight000/go_qqbot/plugins/adminping"
)

func main() {
	if err := rpcsdk.Serve(&adminping.Plugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "adminping:", err)
		os.Exit(1)
	}
}
