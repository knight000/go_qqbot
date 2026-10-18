package md

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// 指令判定是插件自己的事：Manifest 里的前缀只是粗筛（裸 HasPrefix 会命中 /md5）。
func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		src  string
		ok   bool
		note string
	}{
		{in: "/md # 标题", src: "# 标题", ok: true},
		{in: "  /md   两侧空格  ", src: "两侧空格", ok: true},
		{in: "/md", src: "", ok: true, note: "只有指令 -> 回用法"},
		{in: "  /md  ", src: "", ok: true},
		{in: "/md\n# 标题", src: "# 标题", ok: true, note: "换行也算分隔"},
		{in: "/md\n    indented code", src: "indented code", ok: true},
		{in: "md # 标题", ok: false, note: "裸写不触发"},
		{in: "md", ok: false},
		{in: "/md5 abc", ok: false, note: "不能吃掉 /md5"},
		{in: "/mdx", ok: false},
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
	reply, err := p.Handle(context.Background(), msg("/md"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil || !strings.Contains(reply.Content, "用法") {
		t.Fatalf("应回用法，实际 %+v", reply)
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

// 反斜杠一律不解释：所见即所渲染。
//
// 这是对一段已删除代码的回归锁。曾经有过一个 unescape，把 `\n`、`\t` 还原成
// 真字符——那是"平台吃掉换行"这个误判的补丁。真因（单独 CR）在网关修掉之后，
// 这个函数只剩副作用：`C:\new` 会被静默变成换行、`\\d` 会被吃掉一半。
func TestHandleKeepsBackslashes(t *testing.T) {
	var got string
	p, _ := newPlugin(t, &stubRenderer{img: host.ImageFromBytes([]byte("png")), got: &got})

	src := "```\npath := `C:\\new\\test.md`\nre := `\\d+\\s`\n```"
	if _, err := p.Handle(context.Background(), msg("/md "+src)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got != src {
		t.Fatalf("正文被改写：\n got=%q\nwant=%q", got, src)
	}
}

// Manifest 写错会在启动装配期直接拒绝加载（正则编不过、Prefix 与 Pattern 同时为空
// 或同时非空）。这条测试把"启动才能发现的错"提前到 go test。
func TestManifestIsLoadable(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if m.APIVersion != host.APIVersion {
		t.Fatalf("APIVersion=%q，核心要求 %q", m.APIVersion, host.APIVersion)
	}
	if len(m.Intents) == 0 {
		t.Fatal("必须订阅事件，否则插件收不到任何消息")
	}
	// 只留 /md 一条：多声明一条 Pattern 命令就等于把裸写形式又放回来了。
	// 同时锁住"Prefix 非空"——两者都空/都非空核心会在装配期直接拒不加载。
	if len(m.Commands) != 1 || m.Commands[0].Prefix != "/md" || m.Commands[0].Pattern != "" {
		t.Fatalf("应只声明 /md 一条前缀命令，实际 %+v", m.Commands)
	}
}

// 不是本指令时返回 (nil, nil)：让出给优先级更低的插件。
// 裸写 md 必须安静放行——这是刻意的取舍，不是漏判。
func TestHandlePassThrough(t *testing.T) {
	p, _ := newPlugin(t, &stubRenderer{img: host.ImageFromBytes([]byte("png"))})
	for _, in := range []string{"hello", "/ping", "/md5 abc", "/mdx", "md5sum x", "md # 标题", "md"} {
		reply, err := p.Handle(context.Background(), msg(in))
		if err != nil || reply != nil {
			t.Fatalf("Handle(%q) 应放行，实际 %+v, %v", in, reply, err)
		}
	}
}
