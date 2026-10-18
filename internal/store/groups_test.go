package store

import (
	"context"
	"testing"
)

// 首次见到群要返回 isNew=true（群名自动抓取只在这种情况下触发一次），
// 第二次只更新活跃时间。
func TestObserveGroupTracksNewAndExisting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	g, isNew, err := db.ObserveGroup(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if !isNew || g.OpenID != "g1" {
		t.Fatalf("首次应新建登记项: isNew=%v g=%+v", isNew, g)
	}
	if g.FirstSeenAt.IsZero() || g.LastSeenAt.IsZero() {
		t.Fatalf("时间字段不应为零值: %+v", g)
	}

	_, isNew, err = db.ObserveGroup(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("第二次观察不应报告为新群")
	}

	// 空 openid 是调用方 bug，不应静默写进库
	if _, _, err := db.ObserveGroup(ctx, ""); err == nil {
		t.Fatal("空 openid 应报错")
	}
}

func TestGroupModeAndName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.SetGroupMode(ctx, "g1", GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupMode(ctx, "g2", GroupModeDeny); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupMode(ctx, "g1", "bogus"); err == nil {
		t.Fatal("非法模式应报错")
	}

	g, ok, err := db.GetGroup(ctx, "g1")
	if err != nil || !ok || g.Mode != GroupModeAllow {
		t.Fatalf("g1 应为白名单: ok=%v g=%+v err=%v", ok, g, err)
	}
	if _, ok, _ := db.GetGroup(ctx, "missing"); ok {
		t.Fatal("未登记的群应返回 ok=false")
	}

	// 手工名称优先于接口抓取
	if err := db.SetGroupName(ctx, "g1", "读书分享会", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupName(ctx, "g1", "接口名字", "api"); err != nil {
		t.Fatal(err)
	}
	g, _, _ = db.GetGroup(ctx, "g1")
	if g.Name != "读书分享会" || g.NameSource != "manual" {
		t.Fatalf("手工名称不应被覆盖: %+v", g)
	}

	st, err := db.CountGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Allowed != 1 || st.Denied != 1 || st.Left != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	// 列表顺序：白名单 → 黑名单 → 未设置
	if err := db.SetGroupMode(ctx, "g3", GroupModeNone); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].OpenID != "g1" || list[1].OpenID != "g2" || list[2].OpenID != "g3" {
		t.Fatalf("列表排序错误: %+v", list)
	}
}

// 退群只打标记、不删记录：名单与名称都还要留着，
// 否则重新把机器人拉回群时得再授权一次，而"这个群以前叫什么"也无从查起。
func TestMarkGroupLeftKeepsRecord(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.SetGroupMode(ctx, "g1", GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupName(ctx, "g1", "读书分享会", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGroupLeft(ctx, "g1"); err != nil {
		t.Fatal(err)
	}

	g, ok, err := db.GetGroup(ctx, "g1")
	if err != nil || !ok {
		t.Fatalf("退群后记录不应消失: ok=%v err=%v", ok, err)
	}
	if !g.Left() || g.LeftAt.IsZero() {
		t.Fatalf("应标记为已退出: %+v", g)
	}
	if g.Mode != GroupModeAllow || g.Name != "读书分享会" {
		t.Fatalf("退群不应清掉名单与名称: %+v", g)
	}

	inUse, err := db.ListGroupsInUse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inUse) != 0 {
		t.Fatalf("已退出的群不应进入在群列表: %+v", inUse)
	}
	all, err := db.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListGroups 应保留已退出的群: %+v", all)
	}

	st, err := db.CountGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Left != 1 || st.InUse() != 0 {
		t.Fatalf("统计应把已退出单独计数: %+v", st)
	}

	// 重新入群撤销标记，名单依然在
	if err := db.MarkGroupPresent(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	g, _, _ = db.GetGroup(ctx, "g1")
	if g.Left() {
		t.Fatalf("重新入群后不应再是已退出: %+v", g)
	}
	if g.Mode != GroupModeAllow {
		t.Fatalf("重新入群不应动名单: %+v", g)
	}
	if inUse, _ = db.ListGroupsInUse(ctx); len(inUse) != 1 {
		t.Fatalf("重新入群后应出现在在群列表: %+v", inUse)
	}
}

// 收到群消息说明机器人在群里——这比任何事件通知都可信。
// 断线期间丢掉的 GROUP_ADD_ROBOT 就靠这条自愈，否则该群永远算"已退出"，
// 表现为 /group refresh 永远跳过它。
func TestObserveGroupClearsLeftMark(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, _, err := db.ObserveGroup(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGroupLeft(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if _, isNew, err := db.ObserveGroup(ctx, "g1"); err != nil || isNew {
		t.Fatalf("再次观察不应算新群: isNew=%v err=%v", isNew, err)
	}
	g, _, _ := db.GetGroup(ctx, "g1")
	if g.Left() {
		t.Fatalf("收到消息后应清掉退出标记: %+v", g)
	}
}

// 退群事件对未登记的群也要建档：平台可能在机器人第一次说话前就把它踢了，
// 这条记录是唯一能证明"它确实进过这个群"的东西。
func TestMarkGroupLeftCreatesUnknownGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.MarkGroupLeft(ctx, "g9"); err != nil {
		t.Fatal(err)
	}
	g, ok, err := db.GetGroup(ctx, "g9")
	if err != nil || !ok {
		t.Fatalf("未登记的群退群也应建档: ok=%v err=%v", ok, err)
	}
	if !g.Left() || g.Mode != GroupModeNone {
		t.Fatalf("建档后应为已退出且未设置名单: %+v", g)
	}
	if err := db.MarkGroupLeft(ctx, ""); err == nil {
		t.Fatal("空 openid 应报错")
	}
}
