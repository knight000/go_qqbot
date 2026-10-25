package rpc

import (
	"sort"
	"strings"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件是「使用面」的**合并点**（ADR-0007 决策 3 / 5）。
//
// 使用面有两个来源，必须合成一份：
//   - 工具生成：tools/apisurface 把插件 module 对契约成员的静态引用扫出来，
//     写成插件类型上的 APISurface() 方法（产物是纯数据，连 host 都不 import）；
//   - 手写追加：反射调用扫不到，那几处由作者写进 Manifest.Uses（约定放
//     uses_extra.go），这一条路也是「没跑生成器」时的唯一来源。
//
// 合并只能发生在**清单出口**，不能发生在插件的 Manifest() 里：清单有两条
// 出口（`--manifest` 的 stdout 与 plugin.manifest 的应答），两条都得一致，
// 而插件自己只写一处 Manifest()。放在这里，两条出口自动同源。

// apiSurface 是 tools/apisurface 产物实现的接口。
//
// 为什么不在 pkg/host 里声明它：声明了就得为它涨一次契约版本、记一行分级表，
// 而这个方法既不跨进程也不进线协议——它只在本进程内被断言一次。方法名取
// APISurface 而不是 Uses，是为了降低与插件自有方法撞名的概率：一旦撞上，
// 生成的产物会与手写的那个方法重复声明，编译期就红（不会静默取错）。
type apiSurface interface {
	APISurface() []string
}

// PluginManifest 返回插件的清单，Uses 已并入它自报的使用面。
//
// 未生成（插件类型上没有 APISurface）时原样返回，Uses 保持作者写的那一份
// ——空就是空，兼容判定据此退化为「api_version 必须与核心相等」。这个方向
// 不能反：若把「没有生成」当成「不依赖任何成员」，生成器一挂就等于所有插件
// 放行，语义变更会静默穿透（ADR-0007 决策 5）。
func PluginManifest(p host.Plugin) host.Manifest {
	m := p.Manifest()
	s, ok := p.(apiSurface)
	if !ok {
		return m
	}
	m.Uses = mergeUses(m.Uses, s.APISurface())
	return m
}

// mergeUses 取两份使用面的并集：去重、排序。
//
// 排序不是为了好看：这份清单会进 `--manifest` 的 stdout、进启动日志、
// 也可能被插件仓 diff 到。顺序不稳会让「同一个插件两次探测结果不同」这种
// 假信号出现在 diff 与日志对比里。
//
// generated 为空表示产物缺失或作者手写了一个空方法——此时**不排序也不去重**，
// 原样返回 manual，让「没生成」这件事在任何下游都保持可辨认。
func mergeUses(manual, generated []string) []string {
	if len(generated) == 0 {
		return manual
	}
	seen := make(map[string]bool, len(manual)+len(generated))
	out := make([]string, 0, len(manual)+len(generated))
	for _, list := range [][]string{manual, generated} {
		for _, u := range list {
			// TrimSpace 兜住手写时多打的那个空格：成员名是拿来做字符串相交的，
			// " Event.Args" 与 "Event.Args" 不相等，而症状是「明明声明了却被拒载」。
			u = strings.TrimSpace(u)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}
