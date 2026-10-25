// Command fixture 是夹具的入口，只为让扫描器能定位插件类型。
//
// 扫描器靠 `rpcsdk.Serve(<expr>)` 找插件类型与产物落点——这是唯一被真正交给
// 运行时的那个值。夹具照抄真插件的形状（入口在 cmd/<name>，本体在根包），
// 否则「产物该写到哪个包」这条路径没有被验证过。
package main

import (
	"fmt"
	"os"

	"github.com/knight000/go_qqbot/pkg/rpcsdk"
	"github.com/knight000/go_qqbot/tools/apisurface/testdata/fixture"
)

func main() {
	if err := rpcsdk.Serve(&fixture.Plugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}
