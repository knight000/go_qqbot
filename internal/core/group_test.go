package core

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// probePlugin 记录自己被路由了多少次；群准入测试只关心"插件有没有收到"。
type probePlugin struct {
	manifest host.Manifest
	hits     atomic.Int32
}

func (p *probePlugin) Manifest() host.Manifest               { return p.manifest }
func (p *probePlugin) Init(context.Context, host.Host) error { return nil }
func (p *probePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	p.hits.Add(1)
	return nil, nil
}

// newGateCore 在最小 Core 上补一个订阅全部消息的探针插件，
// 这样能同时观察"闸门是否丢弃"和"插件是否被触发"。
func newGateCore(t *testing.T) (*Core, *fakeAPI, *probePlugin, context.Context) {
	t.Helper()
	c, f := newAdminCore(t)
	p := &probePlugin{manifest: host.Manifest{
		Name:       "probe",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
	}}
	router, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	c.router = router
	c.dispatch = NewDispatcher(router, c.sender, c.log, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, f, p, ctx
}

var eventSeq atomic.Int64

// gateEvent 生成 ID 唯一的事件：去重表会拦下重复 ID，测试里不能复用。
func gateEvent(content, sender, peer string) *host.Event {
	n := eventSeq.Add(1)
	return &host.Event{
		ID:       fmt.Sprintf("ev-%d", n),
		Kind:     host.KindMessage,
		Name:     "GROUP_MESSAGE_CREATE",
		Scene:    host.SceneGroup,
		PeerID:   peer,
		SenderID: sender,
		MsgID:    fmt.Sprintf("m-%d", n),
		Content:  content,
	}
}

func waitHits(t *testing.T, p *probePlugin, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.hits.Load() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("插件触发次数应为 %d，实际 %d", want, p.hits.Load())
}

// 白名单默认开启：未登记的群第一条消息就被丢弃，插件收不到，但群要留下记录。
func TestUnlistedGroupIsDroppedButRecorded(t *testing.T) {
	c, f, p, ctx := newGateCore(t)

	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	time.Sleep(100 * time.Millisecond)

	if p.hits.Load() != 0 {
		t.Fatal("未在白名单的群不应触发插件")
	}
	if len(f.bodies) != 0 {
		t.Fatal("被丢弃的群消息不应产生任何回复")
	}
	g, ok, err := c.store.GetGroup(ctx, "g1")
	if err != nil || !ok {
		t.Fatalf("新群应被登记: ok=%v err=%v", ok, err)
	}
	if g.Mode != store.GroupModeNone {
		t.Fatalf("新群默认不属于任何名单，实际 %q", g.Mode)
	}
}

func TestWhitelistedGroupPasses(t *testing.T) {
	c, _, p, ctx := newGateCore(t)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	waitHits(t, p, 1)
}

func TestBlacklistedGroupIsDropped(t *testing.T) {
	c, _, p, ctx := newGateCore(t)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeDeny); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	time.Sleep(100 * time.Millisecond)
	if p.hits.Load() != 0 {
		t.Fatal("黑名单群的消息不应触发插件")
	}
}

// 两个开关都关掉 = 不设准入，任何群都放行。
func TestBothSwitchesOffPassEverything(t *testing.T) {
	c, _, p, ctx := newGateCore(t)
	off := false
	c.cfg.Group.Whitelist = &off
	c.cfg.Group.Blacklist = &off

	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	waitHits(t, p, 1)
}

// 白名单关、黑名单开：黑名单仍然拦。
func TestBlacklistStillWorksWithWhitelistOff(t *testing.T) {
	c, _, p, ctx := newGateCore(t)
	off := false
	c.cfg.Group.Whitelist = &off
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeDeny); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	time.Sleep(100 * time.Millisecond)
	if p.hits.Load() != 0 {
		t.Fatal("黑名单开关单独开启时也应拦截")
	}
}

// 新群自助接入：Bot 管理员在未授权群里发 /group allow，闸门例外放行。
func TestAdminCanWhitelistCurrentGroupFromInside(t *testing.T) {
	c, f, p, ctx := newGateCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	c.Handle(ctx, gateEvent("/group allow", "owner-1", "g1"))

	if got := lastReply(t, f); !strings.Contains(got, "加入白名单") {
		t.Fatalf("应回复已加入白名单，实际: %q", got)
	}
	g, ok, _ := c.store.GetGroup(ctx, "g1")
	if !ok || g.Mode != store.GroupModeAllow {
		t.Fatalf("当前群应被加入白名单: %+v", g)
	}
	time.Sleep(50 * time.Millisecond)
	if p.hits.Load() != 0 {
		t.Fatal("核心指令不应进入插件路由")
	}
}

// 普通用户在未授权群里发 /group：静默丢弃，连"无权限"都不回。
func TestNonAdminCannotWhitelistFromInside(t *testing.T) {
	c, f, p, ctx := newGateCore(t)

	c.Handle(ctx, gateEvent("/group allow", "stranger", "g1"))
	time.Sleep(100 * time.Millisecond)

	if len(f.bodies) != 0 {
		t.Fatalf("未授权群里的普通用户不应收到回复，实际: %q", lastReply(t, f))
	}
	if p.hits.Load() != 0 {
		t.Fatal("被闸门丢弃的消息不应进入插件")
	}
	g, _, _ := c.store.GetGroup(ctx, "g1")
	if g.Mode != store.GroupModeNone {
		t.Fatalf("普通用户不应改变名单，实际 %q", g.Mode)
	}
}

// 黑名单是硬阻断：连 Bot 管理员也不能在群内自助解除，只能从私聊操作。
func TestBlacklistedGroupCannotSelfUnblock(t *testing.T) {
	c, f, _, ctx := newGateCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeDeny); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("/group clear", "owner-1", "g1"))
	time.Sleep(100 * time.Millisecond)

	if len(f.bodies) != 0 {
		t.Fatalf("黑名单群内不应有任何回复，实际: %q", lastReply(t, f))
	}
	g, _, _ := c.store.GetGroup(ctx, "g1")
	if g.Mode != store.GroupModeDeny {
		t.Fatalf("黑名单不应被群内指令解除，实际 %q", g.Mode)
	}
}

// 已放行的群里，管理员可以改名、拉黑当前群；随后消息立即被丢弃。
func TestGroupCommandsFromInsideAllowedGroup(t *testing.T) {
	c, f, p, ctx := newGateCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("/group name 读书分享会", "owner-1", "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "读书分享会") {
		t.Fatalf("改名回复不对: %q", got)
	}
	g, _, _ := c.store.GetGroup(ctx, "g1")
	if g.Name != "读书分享会" || g.NameSource != "manual" {
		t.Fatalf("群名应被手工写入: %+v", g)
	}

	c.Handle(ctx, gateEvent("/group deny", "owner-1", "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "加入黑名单") {
		t.Fatalf("拉黑回复不对: %q", got)
	}
	g, _, _ = c.store.GetGroup(ctx, "g1")
	if g.Mode != store.GroupModeDeny {
		t.Fatalf("当前群应进入黑名单，实际 %q", g.Mode)
	}

	// 拉黑后，普通消息立刻被丢弃
	c.Handle(ctx, gateEvent("hello", "u1", "g1"))
	time.Sleep(100 * time.Millisecond)
	if p.hits.Load() != 0 {
		t.Fatal("拉黑后的群消息不应触发插件")
	}
}

// 私聊里的管理面：按 id 加名单、改名、查状态与列表。
func TestGroupCommandsFromPrivateChat(t *testing.T) {
	c, f, _, ctx := newGateCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	const g1 = "10F9105D268E0E0AC36FA1240761ABF0"
	const g2 = "3E5D8A1F7B2C9E4D6A0F1B3C5D7E9F2A"

	c.Handle(ctx, adminEvent("/group allow "+g1, "owner-1", "", host.SceneC2C, "owner-1"))
	if got := lastReply(t, f); !strings.Contains(got, g1) {
		t.Fatalf("按 id 加白名单的回复不对: %q", got)
	}
	c.Handle(ctx, adminEvent("/group deny "+g2, "owner-1", "", host.SceneC2C, "owner-1"))
	c.Handle(ctx, adminEvent("/group name "+g1+" 读书分享会", "owner-1", "", host.SceneC2C, "owner-1"))

	c.Handle(ctx, adminEvent("/group list", "owner-1", "", host.SceneC2C, "owner-1"))
	got := lastReply(t, f)
	for _, want := range []string{g1, "读书分享会", g2, "白名单 1", "黑名单 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("list 缺少 %q:\n%s", want, got)
		}
	}

	// 过滤
	c.Handle(ctx, adminEvent("/group list allow", "owner-1", "", host.SceneC2C, "owner-1"))
	got = lastReply(t, f)
	if !strings.Contains(got, g1) || strings.Contains(got, g2) {
		t.Fatalf("allow 过滤结果不对:\n%s", got)
	}

	// 状态总览
	c.Handle(ctx, adminEvent("/group status", "owner-1", "", host.SceneC2C, "owner-1"))
	got = lastReply(t, f)
	for _, want := range []string{"白名单 开", "黑名单 开", "已记录 2 个群"} {
		if !strings.Contains(got, want) {
			t.Errorf("status 缺少 %q:\n%s", want, got)
		}
	}

	// 单群状态
	c.Handle(ctx, adminEvent("/group status "+g1, "owner-1", "", host.SceneC2C, "owner-1"))
	got = lastReply(t, f)
	for _, want := range []string{"名称 读书分享会", "名单 白名单", "准入 放行"} {
		if !strings.Contains(got, want) {
			t.Errorf("单群 status 缺少 %q:\n%s", want, got)
		}
	}
}

// /group 只对 Bot 管理员开放；群主不是 Bot 管理员。
func TestGroupCommandRejectsNonAdmin(t *testing.T) {
	c, f, _, ctx := newGateCore(t)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, gateEvent("/group list", "member-1", "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "仅管理员可用") {
		t.Fatalf("非管理员应被拒绝，实际: %q", got)
	}
}

// 群名手工设置优先于自动抓取：自动抓取不得覆盖管理员改过的名字。
func TestManualGroupNameWinsOverAPI(t *testing.T) {
	c, _, _, ctx := newGateCore(t)

	if err := c.store.SetGroupName(ctx, "g1", "管理员命名", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetGroupName(ctx, "g1", "接口返回的名字", "api"); err != nil {
		t.Fatal(err)
	}
	g, _, _ := c.store.GetGroup(ctx, "g1")
	if g.Name != "管理员命名" || g.NameSource != "manual" {
		t.Fatalf("手工名称不应被自动抓取覆盖: %+v", g)
	}

	// 反向：自动抓取先到，之后手工设置应覆盖
	if err := c.store.SetGroupName(ctx, "g2", "接口名字", "api"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetGroupName(ctx, "g2", "新名字", "manual"); err != nil {
		t.Fatal(err)
	}
	g, _, _ = c.store.GetGroup(ctx, "g2")
	if g.Name != "新名字" || g.NameSource != "manual" {
		t.Fatalf("手工设置应覆盖接口名字: %+v", g)
	}
}
