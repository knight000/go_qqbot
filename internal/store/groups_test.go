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

	total, allowed, denied, err := db.CountGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || allowed != 1 || denied != 1 {
		t.Fatalf("统计错误: total=%d allow=%d deny=%d", total, allowed, denied)
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
