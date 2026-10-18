package core

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// capturePlugin 是装配测试用的最小插件：Init 时把宿主扣下来，
// 之后所有断言都从"插件能看见什么"出发，不看 Core 的内部字段。
type capturePlugin struct {
	manifest host.Manifest
	host     host.Host
}

func (p *capturePlugin) Manifest() host.Manifest { return p.manifest }

func (p *capturePlugin) Init(_ context.Context, h host.Host) error {
	p.host = h
	return nil
}

func (p *capturePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

// stubRenderer 用可识别的名字与预置返回证明"插件拿到的是注入的那个实例"。
type stubRenderer struct {
	name string
	img  host.Image
	err  error
}

func (r *stubRenderer) Markdown(context.Context, string) (host.Image, error) {
	return r.img, r.err
}

func startWith(t *testing.T, r host.Renderer) *capturePlugin {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "render.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	p := &capturePlugin{manifest: host.Manifest{Name: "capture", APIVersion: host.APIVersion}}
	c, err := New(Options{
		Store:    db,
		Log:      slog.New(slog.DiscardHandler),
		Config:   &config.Config{},
		Plugins:  []host.Plugin{p},
		Renderer: r,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return p
}

// 注入链的终点是插件：Start 之后插件手里的 Host 必须给出注入的那个渲染器。
func TestStartPassesInjectedRendererToPlugins(t *testing.T) {
	want := &stubRenderer{name: "注入的渲染器", img: host.ImageFromBytes([]byte("png"))}
	p := startWith(t, want)

	r := p.host.Renderer()
	got, ok := r.(*stubRenderer)
	if !ok || got.name != want.name {
		t.Fatalf("插件拿到的不是注入的渲染器，实际 %T", r)
	}
	img, err := r.Markdown(context.Background(), "# 标题")
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !img.Valid() || string(img.Data()) != "png" {
		t.Fatalf("应拿到注入实例预置的图片，实际 %+v", img)
	}
}

// 没注入时插件也得能跑：拿到拒绝性替身而不是 nil，错误也说明得清。
func TestStartWithoutRendererGivesRejectingStub(t *testing.T) {
	p := startWith(t, nil)

	r := p.host.Renderer()
	if r == nil {
		t.Fatal("未注入时插件拿到的 Renderer() 不应为 nil")
	}
	_, err := r.Markdown(context.Background(), "纯文本")
	if err == nil {
		t.Fatal("未装配的渲染器应报错，而不是假装成功")
	}
	if !strings.Contains(err.Error(), "渲染能力未装配") {
		t.Fatalf("错误应说明能力未装配，实际: %v", err)
	}
	if errors.Is(err, host.ErrRenderTooLarge) || errors.Is(err, host.ErrRenderTimeout) {
		t.Fatalf("未装配是环境问题，不应命中内容类哨兵: %v", err)
	}
}

// renderReport 是插件作者会写的那几行：成功发图，超尺寸退回纯文本，
// 其余错误也退回纯文本。这里用真实 Start 出来的 Host 驱动它，验证
// 假渲染器的两种预置返回值确实把调用方带进了不同分支。
func renderReport(ctx context.Context, h host.Host, src string) host.Reply {
	img, err := h.Renderer().Markdown(ctx, src)
	switch {
	case err == nil && img.Valid():
		return host.Reply{MsgType: 7, Image: img}
	case errors.Is(err, host.ErrRenderTooLarge):
		return host.Reply{Content: "内容过长，请分段发送"}
	default:
		return host.Reply{Content: src}
	}
}

func TestPluginBranchesOnRendererResult(t *testing.T) {
	ctx := context.Background()

	t.Run("成功走图片", func(t *testing.T) {
		p := startWith(t, &stubRenderer{img: host.ImageFromBytes([]byte("png"))})
		reply := renderReport(ctx, p.host, "# 标题")
		if !reply.Image.Valid() {
			t.Fatal("渲染成功时应回图")
		}
		if reply.Content != "" {
			t.Fatalf("回图时不应同时带文本（会多消耗一次被动配额），实际 %q", reply.Content)
		}
	})

	t.Run("超尺寸退回纯文本", func(t *testing.T) {
		p := startWith(t, &stubRenderer{err: &host.RenderTooLargeError{NeedHeight: 4200}})
		reply := renderReport(ctx, p.host, "# 标题")
		if reply.Image.Valid() {
			t.Fatal("渲染失败时不应回图")
		}
		if !strings.Contains(reply.Content, "过长") {
			t.Fatalf("超尺寸分支应给出拆图提示，实际 %q", reply.Content)
		}
	})

	t.Run("其它错误退回原文", func(t *testing.T) {
		p := startWith(t, &stubRenderer{err: errors.New("字体加载失败")})
		reply := renderReport(ctx, p.host, "**加粗**")
		if reply.Image.Valid() {
			t.Fatal("渲染失败时不应回图")
		}
		if reply.Content != "**加粗**" {
			t.Fatalf("普通错误应退回纯文本原文，实际 %q", reply.Content)
		}
	})

	t.Run("未装配退回原文", func(t *testing.T) {
		p := startWith(t, nil)
		reply := renderReport(ctx, p.host, "**加粗**")
		if reply.Image.Valid() {
			t.Fatal("未装配时不应回图")
		}
		if reply.Content != "**加粗**" {
			t.Fatalf("未装配应退回纯文本原文，实际 %q", reply.Content)
		}
	})
}
