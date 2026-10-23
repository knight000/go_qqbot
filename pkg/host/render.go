package host

import (
	"context"
	"errors"
	"fmt"
)

// Renderer 把一段受支持的 markdown 文档源画成图片。
//
// 单入口、零样式参数：纯文本就是"没有任何标记的 markdown"。宽度、字号、
// 主题都是固定视觉基线的一部分，插件不可调——这样不同插件出的图才长得一样。
//
// 定义在 host 包而不是渲染实现包里，是因为插件只 import host：
// 哨兵放在实现包，插件的 errors.Is 永远拿不到它，错误分类等于白做。
type Renderer interface {
	Markdown(ctx context.Context, src string) (Image, error)
}

var (
	// ErrRenderTooLarge 表示文档源画出来的高度超过上限。
	ErrRenderTooLarge = errors.New("render: 文档源超出高度上限")
	// ErrRenderTimeout 表示渲染超时或调用方的 ctx 已被取消。
	ErrRenderTimeout = errors.New("render: 渲染超时或已被取消")

	// errRendererUnavailable 是"宿主没装配渲染能力"，属于环境问题，
	// 故意不包装上面两个哨兵：插件据此该退回纯文本，而不是拆图或重试。
	errRendererUnavailable = errors.New("渲染能力未装配：宿主未注入 Renderer，请退回纯文本")
)

// RenderTooLargeError 让插件知道超尺寸时到底需要多高，以决定"拆几条"。
//
// NeedHeight 是所需的像素高度；0 表示未测量（如仅凭源大小直接拒绝）。
type RenderTooLargeError struct {
	NeedHeight int
}

func (e *RenderTooLargeError) Error() string {
	if e.NeedHeight <= 0 {
		return ErrRenderTooLarge.Error()
	}
	return fmt.Sprintf("%s: 需要 %d px 高", ErrRenderTooLarge, e.NeedHeight)
}

func (e *RenderTooLargeError) Unwrap() error { return ErrRenderTooLarge }

// unavailableRenderer 是未注入实现时的替身：拒绝一切渲染请求。
//
// "永不返回 nil"是契约的一部分——插件不该为了环境缺失写判空分支；
// 而报错而不是静默，是因为静默会变成"机器人不理人"。
type unavailableRenderer struct{}

func (unavailableRenderer) Markdown(context.Context, string) (Image, error) {
	return Image{}, errRendererUnavailable
}

// resolveRenderer 把注入值收敛成可直接调用的实现，nil 换成拒绝性替身。
func resolveRenderer(r Renderer) Renderer {
	if r == nil {
		return unavailableRenderer{}
	}
	return r
}
