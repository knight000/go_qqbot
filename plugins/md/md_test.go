package md

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// fakeHost 只填本插件用到的能力：日志与渲染器。
type fakeHost struct {
	log  *slog.Logger
	r    host.Renderer
	last string // 渲染器收到的最后一次源码
}

func (f *fakeHost) Plugin() string              { return "md" }
func (f *fakeHost) Config() host.Params         { return host.Params{} }
func (f *fakeHost) Sender() host.Sender         { return nil }
func (f *fakeHost) KV() host.KV                 { return nil }
func (f *fakeHost) PeerKV(string) host.KV       { return nil }
func (f *fakeHost) Query() host.QueryStore      { return nil }
func (f *fakeHost) HTTP() host.HTTP             { return nil }
func (f *fakeHost) Scheduler() host.Scheduler   { return nil }
func (f *fakeHost) Admins() host.AdminDirectory { return nil }
func (f *fakeHost) Log() *slog.Logger           { return f.log }
func (f *fakeHost) IsAdmin(context.Context, host.Identity) (bool, error) {
	return false, nil
}

// Renderer 复刻核心契约：未注入时给拒绝性替身，永不为 nil。
// 插件据此不写判空分支（判空由 host 的契约测试保证），测试替身也必须守同一条契约，
// 否则测的是一个线上不存在的场景。
func (f *fakeHost) Renderer() host.Renderer {
	if f.r == nil {
		return unavailable{}
	}
	return f.r
}

type unavailable struct{}

func (unavailable) Markdown(context.Context, string) (host.Image, error) {
	return host.Image{}, errors.New("渲染能力未装配：宿主未注入 Renderer，请退回纯文本")
}

// stubRenderer 记录收到的源码，并返回预置结果。
type stubRenderer struct {
	img host.Image
	err error
	got *string
}

func (s *stubRenderer) Markdown(_ context.Context, src string) (host.Image, error) {
	if s.got != nil {
		*s.got = src
	}
	return s.img, s.err
}

func newPlugin(t *testing.T, r host.Renderer) (*Plugin, *fakeHost) {
	t.Helper()
	h := &fakeHost{log: slog.New(slog.NewTextHandler(io.Discard, nil)), r: r}
	p := &Plugin{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p, h
}

func msg(content string) *host.Event {
	return &host.Event{ID: "e1", Kind: host.KindMessage, Scene: host.SceneGroup, Content: content}
}

// 指令判定是插件自己的事：Manifest 里的前缀/正则只是粗筛。
func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		src  string
		ok   bool
		note string
	}{
		{in: "/md # 标题", src: "# 标题", ok: true},
		{in: "md # 标题", src: "# 标题", ok: true},
		{in: "  /md   两侧空格  ", src: "两侧空格", ok: true},
		{in: "/md", src: "", ok: true, note: "只有指令 -> 回用法"},
		{in: "  md  ", src: "", ok: true},
		{in: "/md\n    indented code", src: "indented code", ok: true},
		{in: "mdline1\nline2", src: "mdline1\nline2", ok: false, note: "整段保留换行"},
		{in: "/md5 abc", ok: false, note: "不能吃掉 /md5"},
		{in: "md5sum foo", ok: false},
		{in: "hello", ok: false},
		{in: "", ok: false},
	}
	for _, c := range cases {
		src, ok := parse(c.in)
		if ok != c.ok {
			t.Fatalf("parse(%q) ok=%v, want %v（%s）", c.in, ok, c.ok, c.note)
		}
		if ok && src != c.src {
			t.Fatalf("parse(%q) src=%q, want %q", c.in, src, c.src)
		}
	}
}

// 多行内容必须原样进渲染器：strings.Fields 会把表格和代码块拼坏。
func TestHandleKeepsNewlines(t *testing.T) {
	var got string
	p, _ := newPlugin(t, &stubRenderer{img: host.ImageFromBytes([]byte("png")), got: &got})
	src := "# 标题\n\n| a | b |\n| - | - |\n| 1 | 2 |"
	if _, err := p.Handle(context.Background(), msg("/md "+src)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got != src {
		t.Fatalf("渲染器收到的源码被改写：%q", got)
	}
}

func TestHandleRendersImage(t *testing.T) {
	p, _ := newPlugin(t, &stubRenderer{img: host.ImageFromBytes([]byte("png"))})
	reply, err := p.Handle(context.Background(), msg("/md **加粗**"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil || !reply.Image.Valid() {
		t.Fatal("渲染成功应回图")
	}
	if reply.MsgType != 7 {
		t.Fatalf("图片消息 MsgType 应为 7，实际 %d", reply.MsgType)
	}
	if reply.Content != "" {
		t.Fatalf("回图时不应带文本（会多消耗一次被动配额），实际 %q", reply.Content)
	}
}

// 空内容回用法，且不碰渲染器——传 nil 渲染器也不会 panic。
func TestHandleUsage(t *testing.T) {
	p, _ := newPlugin(t, nil)
	for _, in := range []string{"/md", "md"} {
		reply, err := p.Handle(context.Background(), msg(in))
		if err != nil {
			t.Fatalf("Handle(%q): %v", in, err)
		}
		if reply == nil || !strings.Contains(reply.Content, "用法") {
			t.Fatalf("Handle(%q) 应回用法，实际 %+v", in, reply)
		}
	}
}

func TestHandleErrors(t *testing.T) {
	cases := []struct {
		name  string
		r     host.Renderer
		want  string // 回复正文需包含的片段
		image bool
	}{
		{name: "超尺寸", r: &stubRenderer{err: &host.RenderTooLargeError{NeedHeight: 4200}}, want: "分段"},
		{name: "超时", r: &stubRenderer{err: host.ErrRenderTimeout}, want: "超时"},
		{name: "实现错误", r: &stubRenderer{err: errors.New("字体加载失败")}, want: "渲染失败"},
		{name: "能力未装配", r: nil, want: "渲染失败"},
		{name: "成功但无图", r: &stubRenderer{}, want: "未返回图片"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := newPlugin(t, c.r)
			reply, err := p.Handle(context.Background(), msg("/md x"))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if reply == nil {
				t.Fatal("失败时必须给用户一句反馈，不能静默")
			}
			if reply.Image.Valid() {
				t.Fatal("失败时不应回图")
			}
			if !strings.Contains(reply.Content, c.want) {
				t.Fatalf("回复应包含 %q，实际 %q", c.want, reply.Content)
			}
		})
	}
}

// Manifest 写错会在启动装配期直接拒绝加载（正则编不过、Prefix 与 Pattern 同时为空
// 或同时非空）。这条测试把"启动才能发现的错"提前到 go test。
func TestManifestIsLoadable(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if m.APIVersion != host.APIVersion {
		t.Fatalf("APIVersion=%q，核心要求 %q", m.APIVersion, host.APIVersion)
	}
	if len(m.Commands) == 0 || len(m.Intents) == 0 {
		t.Fatal("必须声明命令与订阅事件，否则插件收不到任何消息")
	}
	for _, c := range m.Commands {
		if (c.Prefix == "") == (c.Pattern == "") {
			t.Fatalf("命令 %q 必须且只能设置 Prefix 或 Pattern 之一", c.Prefix+c.Pattern)
		}
		if c.Pattern != "" {
			if _, err := regexp.Compile(c.Pattern); err != nil {
				t.Fatalf("正则 %q 无法编译: %v", c.Pattern, err)
			}
		}
	}
}

// 不是本指令时返回 (nil, nil)：让出给优先级更低的插件。
func TestHandlePassThrough(t *testing.T) {
	p, _ := newPlugin(t, &stubRenderer{img: host.ImageFromBytes([]byte("png"))})
	for _, in := range []string{"hello", "/ping", "/md5 abc", "md5sum x"} {
		reply, err := p.Handle(context.Background(), msg(in))
		if err != nil || reply != nil {
			t.Fatalf("Handle(%q) 应放行，实际 %+v, %v", in, reply, err)
		}
	}
}
