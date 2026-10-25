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
// 它做四件事：应答 `--manifest`、把 stdin/stdout 接成 rpc.Conn、注册
// plugin.* 的方法、把 Host 实现成 rpc 客户端交给 Init。协议本身（帧、方法表、
// 错误码、清单线格式）在 pkg/rpc；本包只负责「标准输入输出上的那一条连接」
// 这个约定。
//
// 一条必须知道的约束：**stdout 是协议通道**。插件的任何调试输出都要走
// stderr（`Host.Log()` 与默认的 slog 都是 stderr）；往 stdout 打一行字，
// 核心侧的帧读取立刻失步，表现为插件启动即失败。`--manifest` 是这条通道的
// 唯一例外——那一瞬间 stdout 只装一段 JSON，没有连接。
package rpcsdk

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// ManifestFlag 是装配期探测用的命令行开关（ADR-0005 决策 5）。
//
// 探测是「核心起 `bin --manifest`、插件打一段 JSON、退出 0」，所以它必须由
// 插件产物自己应答——核心不认识任何插件类型，替它猜清单不在选项里。
const ManifestFlag = "--manifest"

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
//
// 参数里带 `--manifest` 时不进入连接：往 stdout 打一份清单就返回（调用方
// 的 main 随之正常退出）。这使得「同一个产物既是插件、也是自己的探测对象」，
// 清单只有一个实现（plugin.Manifest），不存在"探测到的"与"实际注册的"
// 两份不一致的可能。
func Serve(p host.Plugin) error {
	if wantsManifest(os.Args[1:]) {
		// 探测这条路不改全局日志：核心只把 stderr 当错误串的尾巴读。
		return writeManifest(p, os.Stdout)
	}
	// 插件日志一律走 stderr 上的 JSON（#48）：核心的 pluginproc 按 level/msg/attrs
	// 重放，等级才不会丢。标准库默认的 TextHandler 会输出 `2009/11/10 23:00:00
	// INFO msg k=v`，核心只能整行当一条 Info 打出来——插件的 WARN/ERROR 在
	// `[log].level = "warn"` 下会被全部过滤掉，现场表现是「插件在报错，日志里
	// 一条都没有」。stderr 仍是日志通道、线协议没变，所以不涨 api_version。
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
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

func wantsManifest(args []string) bool {
	for _, a := range args {
		if a == ManifestFlag {
			return true
		}
	}
	return false
}

// writeManifest 打出一行 JSON。编码用 rpc.EncodeManifest——与 plugin.manifest
// 的应答是同一个函数，字段名不可能漂移（#39 踩过一次）。
//
// 清单取 rpc.PluginManifest 而不是 p.Manifest()：使用面由 tools/apisurface
// 生成在插件类型上（APISurface），要在这里并进 Uses 才能穿过 --manifest
// ——核心的兼容判定读的就是这一份（#56）。
//
// 名字为空的插件在这里就失败：一个没有名字的清单装进去只会得到一条
// "谁都不认识"的路由项，早失败比晚失败便宜。
func writeManifest(p host.Plugin, w io.Writer) error {
	if p == nil {
		return errors.New("rpcsdk: 插件不能为 nil")
	}
	m := rpc.PluginManifest(p)
	if m.Name == "" {
		return errors.New("rpcsdk: 插件 Manifest.Name 不能为空")
	}
	b, err := rpc.EncodeManifest(m, rpc.OptionalMethods(p))
	if err != nil {
		return fmt.Errorf("rpcsdk: 编码清单: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("rpcsdk: 写清单: %w", err)
	}
	return nil
}
