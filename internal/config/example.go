package config

import (
	"bytes"
	_ "embed"
)

// config.example.toml 就在本包目录里，而不是仓库根的 config/：
// //go:embed 不能引用包目录之外的文件，而发布出去的单文件 exe 没有
// 仓库目录可读——`qqbot init` 要写的配置只能从二进制里取（#42）。
//
//go:embed config.example.toml
var exampleTOML []byte

// ExampleTOML 返回 `qqbot init` 写出去的配置样例。
//
// 全仓只有这一份示例（旧位置 config/config.example.toml 已删除），
// 测试校验的也是这份字节，所以"测试通过"就等于"init 写出去的配置能启动"。
// 返回副本：调用方不该能改到包内的内嵌数据。
func ExampleTOML() []byte {
	return bytes.Clone(exampleTOML)
}
