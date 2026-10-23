// Package rpcsdk 是插件二进制的唯一入口。
//
// 插件作者只需要写：
//
//	func main() {
//		if err := rpcsdk.Serve(&echo{}); err != nil {
//			fmt.Fprintln(os.Stderr, "echo:", err)
//			os.Exit(1)   // 非零退出由核心按 1/2/4/8/16 分钟退避重启（ADR-0005 决策 8）
//		}
//	}
//
// 它做三件事：把 stdin/stdout 接成 rpc.Conn、注册 plugin.* 的方法、
// 把 Host 实现成 rpc 客户端交给 Init。协议本身（帧、方法表、错误码）在
// pkg/rpc；本包只负责「标准输入输出上的那一条连接」这个约定。
//
// 一条必须知道的约束：**stdout 是协议通道**。插件的任何调试输出都要走
// stderr（`Host.Log()` 与默认的 slog 都是 stderr）；往 stdout 打一行字，
// 核心侧的帧读取立刻失步，表现为插件启动即失败。
package rpcsdk

import (
	"errors"
	"io"
	"os"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// Options 是 Serve 的可选项。零值即默认行为，绝大多数插件不需要碰它。
type Options struct {
	// Registry 是错误码表。零值等价于 rpc.NewRegistry()（只含 pkg/host 的哨兵）。
	//
	// 需要它的场景很窄但确实存在：插件自己定义了哨兵，经核心转手又回到
	// 插件（例如一个由核心回调触发的失败）。这时插件要在**实现自定义段**
	// 登记自己的码，且号码必须与核心约定的一致——同一个概念在两端有两个
	// 号码，比不分类更难查。
	Registry *rpc.Registry
}

// Serve 在 stdin/stdout 上跑插件，阻塞到对端关闭。
//
// 返回 nil 表示核心正常收摊（读到帧边界上的 EOF）；返回错误表示协议或
// 读写出问题，调用方应非零退出。
func Serve(p host.Plugin) error {
	return ServeIO(p, Options{}, os.Stdin, os.Stdout)
}

// ServeIO 是 Serve 的显式管道版本，供测试与嵌入式场景使用。
func ServeIO(p host.Plugin, o Options, r io.Reader, w io.Writer) error {
	if p == nil {
		return errors.New("rpcsdk: 插件不能为 nil")
	}
	c := rpc.NewConn(r, w, rpc.Options{Registry: o.Registry})
	if err := rpc.ServePlugin(c, p); err != nil {
		return err
	}
	return c.Run()
}
