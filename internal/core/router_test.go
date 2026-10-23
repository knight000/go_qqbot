package core

import (
	"context"
	"testing"

	"github.com/knight000/go_qqbot/pkg/host"
)

type fakePlugin struct{ m host.Manifest }

func (f *fakePlugin) Manifest() host.Manifest { return f.m }
func (f *fakePlugin) Init(context.Context, host.Host) error {
	return nil
}
func (f *fakePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func manifest(name string, priority int, prefix string) host.Manifest {
	return host.Manifest{
		Name:       name,
		APIVersion: host.APIVersion,
		Intents:    []string{"group.message"},
		Commands:   []host.Command{{Prefix: prefix, Priority: priority}},
	}
}

func TestRouterRejectsReservedPrefix(t *testing.T) {
	p := &fakePlugin{m: manifest("evil", 1, "/reset")}
	if _, err := NewRouter([]host.Plugin{p}); err == nil {
		t.Fatal("插件声明核心保留前缀时应拒绝加载")
	}
}

// 保留判定按词边界，不是裸前缀：`/groupinfo`、`/reset2` 与核心指令无关，
// 拿 HasPrefix 去比等于凭空占掉一批名字，插件作者只会看到"莫名其妙的加载失败"。
func TestReservedPrefixUsesWordBoundary(t *testing.T) {
	for _, prefix := range []string{"/groupinfo", "/reset2", "/reloadx", "/claimbot"} {
		p := &fakePlugin{m: manifest("neighbor", 1, prefix)}
		if _, err := NewRouter([]host.Plugin{p}); err != nil {
			t.Errorf("%q 不是保留指令，应能加载: %v", prefix, err)
		}
	}
	// 大小写归一与"带参数写法"仍算同一个指令。
	for _, prefix := range []string{"/group", "/Group", "/reset all"} {
		p := &fakePlugin{m: manifest("evil", 1, prefix)}
		if _, err := NewRouter([]host.Plugin{p}); err == nil {
			t.Errorf("%q 占用了核心保留前缀，应拒绝加载", prefix)
		}
	}
}

// 保留前缀清单必须与 handleReserved 的 case 严格对应：多列一个就等于
// 白占名字（第三方插件想用也会被拒绝加载），少列一个则核心的处理会
// 静默遮蔽插件声明的同名命令。
func TestReservedPrefixesMatchCoreCommands(t *testing.T) {
	want := []string{"/claim", "/admin", "/group", "/reset", "/reload", "/restart", "/upgrade"}
	if len(reservedPrefixes) != len(want) {
		t.Fatalf("保留前缀 = %v, want %v", reservedPrefixes, want)
	}
	for i := range want {
		if reservedPrefixes[i] != want[i] {
			t.Fatalf("保留前缀 = %v, want %v", reservedPrefixes, want)
		}
	}
}

// /grant、/revoke 的功能已由 /admin add|rm 取代，指令本身随之删除，
// 名字也应释放——不允许再以"保留前缀"的名义挡下插件的合法声明。
func TestGrantRevokeNoLongerReserved(t *testing.T) {
	for _, gone := range []string{"/grant", "/revoke"} {
		p := &fakePlugin{m: manifest("legacy", 1, gone)}
		if _, err := NewRouter([]host.Plugin{p}); err != nil {
			t.Fatalf("插件声明 %q 不应被拒绝加载: %v", gone, err)
		}
	}
}

func TestRouterRejectsAPIVersionMismatch(t *testing.T) {
	m := manifest("old", 1, "!old")
	m.APIVersion = "0"
	if _, err := NewRouter([]host.Plugin{&fakePlugin{m: m}}); err == nil {
		t.Fatal("api_version 不匹配时应拒绝加载")
	}
}

func TestRouterPriority(t *testing.T) {
	low := &fakePlugin{m: manifest("low", 10, "!x")}
	high := &fakePlugin{m: manifest("high", 100, "!x")}
	r, err := NewRouter([]host.Plugin{low, high})
	if err != nil {
		t.Fatal(err)
	}

	ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: "!x arg"})
	if len(ms) != 2 {
		t.Fatalf("应命中 2 个插件，实际 %d", len(ms))
	}
	if ms[0].Manifest.Name != "high" {
		t.Fatalf("优先级最高的应排首位，实际 %s", ms[0].Manifest.Name)
	}
}

func TestRouterIntentFilter(t *testing.T) {
	p := &fakePlugin{m: manifest("group-only", 1, "!x")}
	r, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatal(err)
	}
	if ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneC2C, Content: "!x"}); len(ms) != 0 {
		t.Fatal("未订阅的场景不应命中")
	}
}

// 订阅语法写错必须在装配阶段报错：未知事件永不进路由，拼错不会在运行时暴露。
func TestRouterRejectsBadIntent(t *testing.T) {
	for _, bad := range []string{"mesage", "group.mesage", "unknown", "group", "a.b.c", ""} {
		m := manifest("bad", 1, "!x")
		m.Intents = []string{bad}
		if _, err := NewRouter([]host.Plugin{&fakePlugin{m: m}}); err == nil {
			t.Fatalf("intent %q 应被拒绝加载", bad)
		}
	}
}

// 只写大类（不带场景）表示订阅该大类的所有场景。
func TestRouterIntentAnyScene(t *testing.T) {
	m := manifest("all-messages", 1, "!x")
	m.Intents = []string{"message"}
	r, err := NewRouter([]host.Plugin{&fakePlugin{m: m}})
	if err != nil {
		t.Fatal(err)
	}
	for _, scene := range []host.Scene{host.SceneGroup, host.SceneC2C} {
		if ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: scene, Content: "!x"}); len(ms) != 1 {
			t.Fatalf("订阅 message 应命中 %s 场景", scene)
		}
	}
	if ms := r.Match(&host.Event{Kind: host.KindNotice, Scene: host.SceneGroup, Name: "GROUP_ADD_ROBOT"}); len(ms) != 0 {
		t.Fatal("订阅 message 不应命中通知事件")
	}
}

// 命令词边界与参数切分都在路由层：命中后 Match 带回命令写法与已 TrimSpace 的参数，
// 插件不再自己 CutPrefix（否则每个插件都要重演一遍 /md5 误命中的兜底）。
func TestRouterMatchPrefixArgs(t *testing.T) {
	cases := []struct {
		in      string
		prefix  string
		loose   bool
		command string
		args    string
		hit     bool
		note    string
	}{
		{in: "!echo hi", prefix: "!echo", command: "!echo", args: "hi", hit: true},
		{in: "!echo", prefix: "!echo", command: "!echo", args: "", hit: true, note: "只有指令 -> 参数为空串"},
		{in: "  !echo  ", prefix: "!echo", command: "!echo", args: "", hit: true},
		{in: "!echo\nhi", prefix: "!echo", command: "!echo", args: "hi", hit: true, note: "换行也算分隔"},
		{in: "  !echo   两侧空格  ", prefix: "!echo", command: "!echo", args: "两侧空格", hit: true},
		{in: "!echo5 hi", prefix: "!echo", hit: false, note: "默认要求词边界，不能吃掉 !echo5"},
		{in: "!echox", prefix: "!echo", hit: false},
		{in: "hello", prefix: "!echo", hit: false},
		// 票 #12 的验收形状：/md、/md 内容、/md\n内容 命中且 Args 一致，/md5 不命中。
		{in: "/md", prefix: "/md", command: "/md", args: "", hit: true},
		{in: "/md 内容", prefix: "/md", command: "/md", args: "内容", hit: true},
		{in: "/md\n内容", prefix: "/md", command: "/md", args: "内容", hit: true},
		{in: "/md5 abc", prefix: "/md", hit: false, note: "未声明 Loose 时 /md5 不算 /md"},
		// 显式放宽才退回裸前缀语义，参数从紧邻处开始。
		{in: "!echo5 hi", prefix: "!echo", loose: true, command: "!echo", args: "5 hi", hit: true},
	}
	for _, c := range cases {
		m := manifest("p", 10, c.prefix)
		m.Commands[0].Loose = c.loose
		r, err := NewRouter([]host.Plugin{&fakePlugin{m: m}})
		if err != nil {
			t.Fatal(err)
		}
		ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: c.in})
		if hit := len(ms) > 0; hit != c.hit {
			t.Fatalf("Match(%q) 命中=%v, want %v（%s）", c.in, hit, c.hit, c.note)
		}
		if !c.hit {
			continue
		}
		if ms[0].Command != c.command {
			t.Fatalf("Match(%q) Command=%q, want %q", c.in, ms[0].Command, c.command)
		}
		if ms[0].Args != c.args {
			t.Fatalf("Match(%q) Args=%q, want %q", c.in, ms[0].Args, c.args)
		}
	}
}

// 正则命令回传正则源码，Args 取匹配区间之后的剩余（不是整段内容）。
func TestRouterMatchRegexArgs(t *testing.T) {
	pattern := `^!roll\s+`
	m := host.Manifest{
		Name:       "roller",
		APIVersion: host.APIVersion,
		Intents:    []string{"group.message"},
		Commands:   []host.Command{{Pattern: pattern, Priority: 10}},
	}
	r, err := NewRouter([]host.Plugin{&fakePlugin{m: m}})
	if err != nil {
		t.Fatal(err)
	}

	ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: "!roll   3d6 +2"})
	if len(ms) != 1 {
		t.Fatalf("正则应命中，实际 %d 条", len(ms))
	}
	if ms[0].Command != pattern {
		t.Fatalf("Command 应为正则源码 %q，实际 %q", pattern, ms[0].Command)
	}
	if ms[0].Args != "3d6 +2" {
		t.Fatalf("Args 应取匹配区间之后，实际 %q", ms[0].Args)
	}

	if ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: "roll 3d6"}); len(ms) != 0 {
		t.Fatal("不匹配的正则不应命中")
	}
}

// 未声明命令 = 订阅该 intent 的全部事件：没有命中的命令，两个字段都应是空串，
// 而不是把整段正文塞进 Args。
func TestRouterMatchWithoutCommandLeavesArgsEmpty(t *testing.T) {
	m := host.Manifest{Name: "all", APIVersion: host.APIVersion, Intents: []string{"group.message"}}
	r, err := NewRouter([]host.Plugin{&fakePlugin{m: m}})
	if err != nil {
		t.Fatal(err)
	}
	ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: "随便说点什么"})
	if len(ms) != 1 {
		t.Fatalf("应命中 1 条，实际 %d", len(ms))
	}
	if ms[0].Command != "" || ms[0].Args != "" {
		t.Fatalf("未命中命令时 Command/Args 应为空，实际 %q / %q", ms[0].Command, ms[0].Args)
	}
}

// Lookup 是会话投递的取人入口：会话行里只存插件名，重载/卸载之后
// 名字可能已经不在路由表里，返回 nil 就是"悬挂会话"的判定依据。
func TestRouterLookup(t *testing.T) {
	p := &fakePlugin{m: manifest("md", 10, "/md")}
	router, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatal(err)
	}

	rt := router.Lookup("md")
	if rt == nil || rt.plugin != host.Plugin(p) || rt.manifest.Name != "md" {
		t.Fatalf("应取到 md 的路由项: %+v", rt)
	}
	if router.Lookup("ghost") != nil {
		t.Fatal("未加载的插件应返回 nil")
	}
}
