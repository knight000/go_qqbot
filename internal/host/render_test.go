package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 未装配渲染器时，插件必须拿到一个可调用但拒绝服务的对象：
// 判空分支不该由每个插件重复写，而"环境没装配"也不能被误判成"内容超尺寸"。
func TestRendererWithoutInjectionIsRejecting(t *testing.T) {
	h := NewService(Options{Name: "p"})

	r := h.Renderer()
	if r == nil {
		t.Fatal("未注入时 Renderer() 不应返回 nil")
	}

	img, err := r.Markdown(context.Background(), "# 标题")
	if err == nil {
		t.Fatal("未注入时 Markdown 应返回错误")
	}
	if img.Valid() {
		t.Fatal("失败时不应返回有效图片")
	}
	if !strings.Contains(err.Error(), "渲染能力未装配") {
		t.Fatalf("错误说明应指出能力未装配，实际: %v", err)
	}
	if errors.Is(err, ErrRenderTooLarge) || errors.Is(err, ErrRenderTimeout) {
		t.Fatalf("未装配不是内容问题，不应命中渲染哨兵: %v", err)
	}
}

// fakeRenderer 用一个可识别的名字证明"拿到的就是我们注入的那个"，
// 不靠指针相等——实现侧完全可以在中间包一层。
type fakeRenderer struct {
	name string
	img  Image
	err  error
}

func (f *fakeRenderer) Markdown(context.Context, string) (Image, error) { return f.img, f.err }

func TestRendererReturnsInjectedImplementation(t *testing.T) {
	fake := &fakeRenderer{name: "单位测试专用", img: ImageFromBytes([]byte("png"))}
	h := NewService(Options{Name: "p", Renderer: fake})

	got := h.Renderer()
	if got == nil {
		t.Fatal("注入后 Renderer() 不应返回 nil")
	}
	if fr, ok := got.(*fakeRenderer); !ok || fr.name != "单位测试专用" {
		t.Fatalf("Renderer() 应原样返回注入的实现，实际 %T", got)
	}
	img, err := got.Markdown(context.Background(), "任意内容")
	if err != nil {
		t.Fatalf("fake 不应报错: %v", err)
	}
	if !img.Valid() {
		t.Fatal("应拿到 fake 预置的图片")
	}
	if strings.TrimSpace(string(img.Data())) != "png" {
		t.Fatalf("拿到的不是 fake 预置的字节: %q", img.Data())
	}
}

// 超尺寸错误必须能被 errors.Is 归类、被 errors.As 取出所需高度—
// 插件据此判断是"拆两条图"还是"放弃"。
func TestRenderTooLargeErrorClassifies(t *testing.T) {
	err := fmt.Errorf("渲染失败: %w", &RenderTooLargeError{NeedHeight: 4200})

	if !errors.Is(err, ErrRenderTooLarge) {
		t.Fatal("应 errors.Is 到 ErrRenderTooLarge")
	}
	if errors.Is(err, ErrRenderTimeout) {
		t.Fatal("超尺寸不应同时命中超时哨兵")
	}
	var tooLarge *RenderTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatal("应 errors.As 到 *RenderTooLargeError")
	}
	if tooLarge.NeedHeight != 4200 {
		t.Fatalf("NeedHeight = %d, want 4200", tooLarge.NeedHeight)
	}

	// 0 表示"未测量"，不是"高度为 0"。
	unmeasured := &RenderTooLargeError{}
	if !errors.Is(unmeasured, ErrRenderTooLarge) || !strings.Contains(unmeasured.Error(), "上限") {
		t.Fatalf("未测量的超尺寸错误也应可归类: %v", unmeasured.Error())
	}
}
