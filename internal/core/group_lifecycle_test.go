package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// newLifecycleCore 在最小 Core 上装一个能返回群名的 fakeAPI，
// 并把主动消息的总开关打开——群生命周期通知走的是主动私聊。
func newLifecycleCore(t *testing.T) (*Core, *fakeAPI, context.Context) {
	t.Helper()
	c, f := newAdminCore(t)
	f.names = map[string]string{}
	f.errs = map[string]error{}
	c.api = f
	// DailyPerPeer=0 表示不限：测试里要断言"发了 2 条"，不能被日预算截断。
	c.sender = NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{Enabled: true}, c.store, time.Local)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, f, ctx
}

// lifecycleEvent 构造入群/退群通知。ID 唯一：去重表会拦重复事件。
func lifecycleEvent(notice host.Notice, groupID, operator string) *host.Event {
	n := testEventSeq.Add(1)
	return &host.Event{
		ID:        fmt.Sprintf("lc-%d", n),
		Kind:      host.KindNotice,
		Name:      "TEST_" + string(notice),
		Notice:    notice,
		Scene:     host.SceneGroup,
		PeerID:    groupID,
		SenderID:  operator,
		Timestamp: time.Now(),
	}
}

func waitCalls(t *testing.T, f *fakeAPI, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.callCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("应发出 %d 条消息，实际 %d", want, f.callCount())
}

func waitNotRefreshing(t *testing.T, c *Core) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !c.refreshing.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("刷新任务未在 5s 内结束")
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %q:\n%s", w, got)
		}
	}
}

// 入群事件要在闸门之外处理：新群此刻必然未放行，走闸门就被丢了。
// 这条用例的群从头到尾没进过白名单——能收到通知本身就证明闸门被正确绕过。
func TestBotJoinedGroupRegistersAndNotifiesOwner(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	f.names["g-new"] = "读书分享会"
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	c.Handle(ctx, lifecycleEvent(host.NoticeBotJoinedGroup, "g-new", "op-1"))
	waitCalls(t, f, 1)

	g, ok, err := c.store.GetGroup(ctx, "g-new")
	if err != nil || !ok {
		t.Fatalf("入群应立即建档: ok=%v err=%v", ok, err)
	}
	if g.Left() {
		t.Fatalf("刚入群不应带退出标记: %+v", g)
	}
	if g.Name != "读书分享会" || g.NameSource != "api" {
		t.Fatalf("群名应由接口写入: %+v", g)
	}

	if n := f.callCount(); n != 1 {
		t.Fatalf("默认只通知所有者一条，实际 %d 条", n)
	}
	mustContain(t, lastReply(t, f),
		"g-new", "读书分享会", "op-1", "/group allow g-new", "消息会被丢弃")
}

// 退群：只打标记，名单与名称都留着；重新拉回群时不需要再授权一次。
func TestBotLeftGroupMarksExited(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetGroupName(ctx, "g1", "读书分享会", "manual"); err != nil {
		t.Fatal(err)
	}
	// 退群要按 R1 语义清掉该群的会话状态，但绝不能波及别的群。
	for _, peer := range []string{"g1", "g2"} {
		if err := c.store.KVSet(ctx, "echo", peer, "state", []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
	}

	c.Handle(ctx, lifecycleEvent(host.NoticeBotLeftGroup, "g1", "op-1"))
	waitCalls(t, f, 1)

	g, ok, _ := c.store.GetGroup(ctx, "g1")
	if !ok {
		t.Fatal("退群不应删除记录")
	}
	if !g.Left() {
		t.Fatalf("应标记为已退出: %+v", g)
	}
	if g.Mode != store.GroupModeAllow || g.Name != "读书分享会" {
		t.Fatalf("退群不应清掉名单与名称: %+v", g)
	}
	if _, ok, _ := c.store.KVGet(ctx, "echo", "g1", "state"); ok {
		t.Fatal("退群应清掉该群的会话状态")
	}
	if _, ok, _ := c.store.KVGet(ctx, "echo", "g2", "state"); !ok {
		t.Fatal("退群不应影响别的群")
	}
	mustContain(t, lastReply(t, f), "已退出", "读书分享会", "名单仍保留")
}

// 黑名单群被重新拉进来时仍要处理通知：管理员必须知道"有人又把它拉进来了"。
// 如果走闸门，黑名单是硬阻断，这条信息就永远收不到。
func TestBotRejoinedBlacklistedGroupStillNotifies(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeDeny); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, lifecycleEvent(host.NoticeBotJoinedGroup, "g1", "op-1"))
	waitCalls(t, f, 1)

	if g, _, _ := c.store.GetGroup(ctx, "g1"); g.Left() {
		t.Fatalf("重新入群应撤销退出标记: %+v", g)
	}
	mustContain(t, lastReply(t, f), "黑名单")
}

// 平台重投同一条事件时不能发两遍私聊。
func TestGroupLifecycleIsDeduplicated(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	ev := lifecycleEvent(host.NoticeBotJoinedGroup, "g1", "op-1")
	c.Handle(ctx, ev)
	c.Handle(ctx, ev)
	waitCalls(t, f, 1)

	time.Sleep(100 * time.Millisecond)
	if n := f.callCount(); n != 1 {
		t.Fatalf("重复投递只应通知一次，实际 %d 条", n)
	}
}

// 收件人默认只有所有者；[group].notify=admin 时抄送管理员；off 时完全静默。
func TestGroupNotifyRecipients(t *testing.T) {
	t.Run("admin", func(t *testing.T) {
		c, f, ctx := newLifecycleCore(t)
		c.cfg.Group.Notify = "admin"
		seedAdmin(t, c, "owner-1", "", store.RoleOwner)
		seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

		c.Handle(ctx, lifecycleEvent(host.NoticeBotJoinedGroup, "g1", "op-1"))
		waitCalls(t, f, 2)
	})

	t.Run("off", func(t *testing.T) {
		c, f, ctx := newLifecycleCore(t)
		c.cfg.Group.Notify = "off"
		seedAdmin(t, c, "owner-1", "", store.RoleOwner)

		c.Handle(ctx, lifecycleEvent(host.NoticeBotJoinedGroup, "g1", "op-1"))
		time.Sleep(150 * time.Millisecond)

		if n := f.callCount(); n != 0 {
			t.Fatalf("关闭通知后不应发任何消息，实际 %d 条", n)
		}
		// 通知关了，登记不能跟着丢
		if g, ok, _ := c.store.GetGroup(ctx, "g1"); !ok || g.Left() {
			t.Fatalf("关闭通知不应影响登记: ok=%v g=%+v", ok, g)
		}
	})
}

// 批量刷新：只刷在群的、不动手工名、结果私聊汇报给发起人。
func TestGroupRefreshUpdatesNamesAndSkipsManualAndLeft(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	if err := c.store.SetGroupName(ctx, "g1", "旧名字", "api"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetGroupName(ctx, "g2", "手工名", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetGroupName(ctx, "g3", "已退出的群", "api"); err != nil {
		t.Fatal(err)
	}
	if err := c.store.MarkGroupLeft(ctx, "g3"); err != nil {
		t.Fatal(err)
	}
	f.names["g1"] = "新名字"
	f.names["g2"] = "接口想改的名字"
	f.names["g3"] = "已退出的群也不该被取"

	c.Handle(ctx, adminEvent("/group refresh", "owner-1", "", host.SceneC2C, "owner-1"))
	// 回执是同步发出的第 1 条；汇报来自后台协程，不能拿 lastReply 断言它。
	mustContain(t, f.bodyAt(0).Content, "开始刷新 2 个群")

	waitNotRefreshing(t, c)

	if g, _, _ := c.store.GetGroup(ctx, "g1"); g.Name != "新名字" || g.NameSource != "api" {
		t.Fatalf("在群的自动名应被刷新: %+v", g)
	}
	if g, _, _ := c.store.GetGroup(ctx, "g2"); g.Name != "手工名" || g.NameSource != "manual" {
		t.Fatalf("手工名不应被自动刷新覆盖: %+v", g)
	}
	if g, _, _ := c.store.GetGroup(ctx, "g3"); g.Name != "已退出的群" {
		t.Fatalf("已退出的群不应参与刷新: %+v", g)
	}
	// g1 一次；g2 手工跳过；g3 已退出不查
	if n := f.infoCallCount(); n != 1 {
		t.Fatalf("应只调用 1 次群信息接口，实际 %d 次", n)
	}

	// 汇报是第二条消息（第一条是回执）
	if n := f.callCount(); n != 2 {
		t.Fatalf("应有回执 + 汇报两条消息，实际 %d 条", n)
	}
	mustContain(t, f.bodyAt(1).Content, "群名称刷新完成", "已更新 1", "手工名跳过 1")
}

// 接口整体无权限（11253）时要说清楚原因，而不是让用户以为刷新成功了。
func TestGroupRefreshReportsForbidden(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupName(ctx, "g1", "旧名字", "api"); err != nil {
		t.Fatal(err)
	}
	f.errs["g1"] = errors.New("平台返回 11253: no permission")

	c.Handle(ctx, adminEvent("/group refresh", "owner-1", "", host.SceneC2C, "owner-1"))
	waitNotRefreshing(t, c)

	mustContain(t, f.bodyAt(f.callCount()-1).Content, "刷新完成", "11253", "手工设置")
	if g, _, _ := c.store.GetGroup(ctx, "g1"); g.Name != "旧名字" {
		t.Fatalf("取不到名字时不应改写: %+v", g)
	}
}

// 同一时刻只允许一个刷新任务，否则两批请求会把 30 QPM 的配额互相打满。
func TestGroupRefreshRejectsConcurrentRun(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	if err := c.store.SetGroupName(ctx, "g1", "旧名字", "api"); err != nil {
		t.Fatal(err)
	}
	c.refreshing.Store(true)

	c.Handle(ctx, adminEvent("/group refresh", "owner-1", "", host.SceneC2C, "owner-1"))
	mustContain(t, lastReply(t, f), "已有刷新任务")
	if n := f.infoCallCount(); n != 0 {
		t.Fatalf("被拒绝的刷新不应调接口，实际 %d 次", n)
	}
}

// 列表与状态要能区分"在群"与"已退出"，否则用户会困惑
// 为什么某个群不出现在 refresh 的统计里。
func TestGroupListAndStatusShowExited(t *testing.T) {
	c, f, ctx := newLifecycleCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	// 私聊里按 id 操作要求完整 32 位十六进制，短名字会被当成"没给 id"。
	const g1 = "10F9105D268E0E0AC36FA1240761ABF0"
	if err := c.store.SetGroupMode(ctx, g1, store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := c.store.MarkGroupLeft(ctx, g1); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, adminEvent("/group list", "owner-1", "", host.SceneC2C, "owner-1"))
	mustContain(t, lastReply(t, f), "在群 0", "已退出 1")

	c.Handle(ctx, adminEvent("/group status "+g1, "owner-1", "", host.SceneC2C, "owner-1"))
	mustContain(t, lastReply(t, f), "状态 已退出")

	c.Handle(ctx, adminEvent("/group status", "owner-1", "", host.SceneC2C, "owner-1"))
	mustContain(t, lastReply(t, f), "在群 0", "已退出 1")
}
