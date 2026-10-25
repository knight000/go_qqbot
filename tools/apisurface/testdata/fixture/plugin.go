// Package fixture 是 tools/apisurface 的扫描夹具，不是插件。
//
// 它存在的唯一目的是让「扫描器会不会静默漏扫」这件事可被断言：这里刻意写了
// 泛型、嵌入接口、类型别名三种写法，每种都引用契约成员，而成员名必须出现在
// 产物里。它自己不需要能跑起来——扫描只需要类型检查通过。
//
// 产物 uses_gen.go 是**入仓的**：用例拿「重新生成的结果」与它逐字比对，
// 生成器一改行为（多扫一条、少扫一条、顺序变了）就会红。
package fixture

import (
	"context"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// Plugin 是夹具插件本体，形状与真插件一致（三件套 + 一个可选的任务入口）。
type Plugin struct {
	h host.Host
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:       "fixture",
		Version:    "0.0.0",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
		Commands:   []host.Command{{Prefix: "/fixture", Priority: 100}},
	}
}

func (p *Plugin) Init(_ context.Context, h host.Host) error {
	p.h = h
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	// 成员级引用：Event.Args / Event.Scene / Event.Actor。
	if ev.Scene != host.SceneGroup {
		return &host.Reply{Content: "只在群里响应"}, nil
	}
	// Params.Duration：配置读取是插件最常用的一类引用，也是「符号级声明会
	// 误伤、成员级才够用」的典型（torn 就用到它）。
	interval := p.h.Config().Duration("interval", time.Minute)
	return &host.Reply{Content: ev.Args + interval.String()}, nil
}

// RunJob 引用 JobSpec 的三个字段：任务入口的参数是契约类型。
func (p *Plugin) RunJob(_ context.Context, spec host.JobSpec) error {
	if spec.Cron == "" || spec.Timeout <= 0 {
		return nil
	}
	return p.h.Scheduler().Register(spec)
}

// —— 扫描器的三个已知难点，各一例 ——
//
// 每一例都刻意是某个成员名的**唯一**来源：这样「产物里有没有这条」就等于
// 「那种写法有没有被扫到」，不必靠人读产物猜。

// pair 让泛型实参位置上出现契约类型。
type pair[A, B any] struct {
	first  A
	second B
}

// first 是泛型函数：调用处的类型实参同样是对契约的引用。
func first[T any](xs []T) (T, bool) {
	if len(xs) == 0 {
		var zero T
		return zero, false
	}
	return xs[0], true
}

// genericUse 是「泛型」那一例：Command.Pattern 只能来自这里——它读的是泛型
// 函数返回值的字段，类型实参在调用处才落地。AdminTarget 也只在这里出现
// （泛型结构体的类型实参位置）。
func genericUse() {
	cmds := []host.Command{{Prefix: "/a"}}
	if c, ok := first(cmds); ok {
		_ = c.Pattern
	}
	_ = pair[host.AdminTarget, string]{}
}

// surface 是「插件自己声明、内嵌契约接口」的典型写法：插件常把 Host 收窄成
// 自己需要的那几个方法。通过它调用方法时，成员名必须记成**声明处**
// （Host.Renderer），而不是 surface.Renderer——后者在契约包里不存在。
type surface interface {
	host.Host
}

// embeddedUse 是「嵌入接口」那一例：Host.Renderer / Host.KV 只在这里出现。
func embeddedUse(s surface) {
	_ = s.Renderer()
	_ = s.KV()
}

// ev 是类型别名：别名用法必须还原到 host.Event，记 Event.SenderID 而不是
// ev.SenderID。Event.SenderID 只在这里出现，所以它缺了就是别名没被展开。
type ev = host.Event

func aliasUse(e ev) string {
	return e.SenderID
}
