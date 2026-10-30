package instance

import (
	"context"

	"github.com/knight000/go_qqbot/pkg/host"
)

// botRenderer 给共享渲染器包上一层「每 Bot 同时最多 1 个渲染任务」的闸门。
//
// 为什么要这一层，而不是改渲染包：全局并发 2 是内存实测调出来的进程级上限
// （1080×4000 的 RGBA 约 17.3 MB，无节制并发会撞 MemoryMax），它守的是**内存**；
// 而「一个租户最多占几格」是租户间的公平问题，属于装配层的知识——渲染包不该
// 知道 Bot 的存在（ADR-0008 决策 13）。
//
// 不做每 Bot 独立 worker 池：内存会随租户数线性增长，与 256 MB 上限冲突。
// 所以只有一个共享渲染器 + N 个容量为 1 的闸门，全局仍然最多 2 张同时在画。
type botRenderer struct {
	inner host.Renderer
	// slots 容量为 1：同一个 Bot 的第二个渲染请求在这里等，而不是去抢全局那 2 格。
	slots chan struct{}
}

func newBotRenderer(inner host.Renderer) *botRenderer {
	return &botRenderer{inner: inner, slots: make(chan struct{}, 1)}
}

// renderGateFor 为某个 Bot 造闸门；未注入渲染器时保持 nil。
//
// nil 不能包：host 会把 nil 换成"拒绝一切"的替身，插件据此退回纯文本；
// 包一层非 nil 的空壳等于把"没装配"变成运行期 nil 解引用。
func renderGateFor(r host.Renderer) host.Renderer {
	if r == nil {
		return nil
	}
	return newBotRenderer(r)
}

// Markdown 先过本 Bot 的闸门，再交给共享渲染器排全局那 2 格。
//
// 等闸门时被取消要返回 render 的哨兵错误而不是 ctx.Err()：插件按
// ErrRenderTimeout 判定"这张图没出来"，换个错误类型它就只能当未知故障。
func (r *botRenderer) Markdown(ctx context.Context, src string) (host.Image, error) {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return host.Image{}, host.ErrRenderTimeout
	}
	defer func() { <-r.slots }()
	return r.inner.Markdown(ctx, src)
}
