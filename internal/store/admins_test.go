package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 旧库没有 note / union_openid 列，Migrate 必须能补上（SQLite 的 ADD COLUMN 无 IF NOT EXISTS）。
func TestMigrateAddsAdminColumnsToOldDB(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.w.ExecContext(ctx, `CREATE TABLE admins (
		openid     TEXT NOT NULL,
		scene      TEXT NOT NULL,
		peer_scope TEXT NOT NULL DEFAULT '',
		role       TEXT NOT NULL,
		granted_by TEXT,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (openid, scene, peer_scope)
	)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("旧库迁移失败: %v", err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Scene: "group", Role: RoleOwner, Note: "备注"}); err != nil {
		t.Fatalf("迁移后写入失败: %v", err)
	}
	list, err := db.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Note != "备注" {
		t.Fatalf("迁移后新列应可用: %+v", list)
	}

	// 幂等：再跑一次不应报 duplicate column
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移应幂等: %v", err)
	}
}

func TestAdminRoleScopeIsolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "owner-1", Scene: "group", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "admin-1", Scene: "group", PeerScope: "g1", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}

	// owner 在任意群都有效（peer_scope 为空）
	if role, _, ok, _ := db.AdminRole(ctx, "owner-1", "", "group", "g2"); !ok || role != RoleOwner {
		t.Fatalf("owner 在其它群应为 owner，实际 role=%q ok=%v", role, ok)
	}
	// admin 只在授权群有效
	if role, _, ok, _ := db.AdminRole(ctx, "admin-1", "", "group", "g1"); !ok || role != RoleAdmin {
		t.Fatalf("admin 在授权群应为 admin，实际 role=%q ok=%v", role, ok)
	}
	if _, _, ok, _ := db.AdminRole(ctx, "admin-1", "", "group", "g2"); ok {
		t.Fatal("admin 不应在未授权群生效")
	}
	// 群授权不跨越到单聊
	if _, _, ok, _ := db.AdminRole(ctx, "admin-1", "", "c2c", "admin-1"); ok {
		t.Fatal("群内 admin 不应自动获得单聊权限")
	}
	// 陌生人
	if _, _, ok, _ := db.AdminRole(ctx, "stranger", "", "group", "g1"); ok {
		t.Fatal("陌生人不应是管理员")
	}
}

// 群认领的 owner 在私聊里也应是 owner：靠 union_openid 把两种身份连起来。
func TestAdminRoleUnionLinkAcrossScenes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 在群里认领：member_openid + union
	if err := db.LinkIdentity(ctx, "member-A", "union-A", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "member-A", UnionID: "union-A", Scene: "group", Role: RoleOwner,
	}); err != nil {
		t.Fatal(err)
	}

	// 私聊前：单聊场景下无法识别
	if _, _, ok, _ := db.AdminRole(ctx, "c2c-A", "", "c2c", "c2c-A"); ok {
		t.Fatal("尚未建立单聊身份映射时不应识别为管理员")
	}

	// 在私聊里说过一句话：平台回传 union_openid
	if err := db.LinkIdentity(ctx, "c2c-A", "union-A", "c2c", ""); err != nil {
		t.Fatal(err)
	}
	role, _, ok, err := db.AdminRole(ctx, "c2c-A", "union-A", "c2c", "c2c-A")
	if err != nil || !ok || role != RoleOwner {
		t.Fatalf("群认领的 owner 应在私聊生效: role=%q ok=%v err=%v", role, ok, err)
	}

	// 没有 union 时（部分事件版本）不能凭空放行
	if _, _, ok, _ := db.AdminRole(ctx, "c2c-A", "", "c2c", "c2c-A"); ok {
		t.Fatal("缺少 union_openid 时不应跨场景匹配")
	}

	// 群内受限的 admin：私聊可用（peer_scope 只约束群），去别的群仍不行
	if err := db.LinkIdentity(ctx, "member-B", "union-B", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkIdentity(ctx, "c2c-B", "union-B", "c2c", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "member-B", UnionID: "union-B", Scene: "group", PeerScope: "g1", Role: RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	if role, _, ok, _ := db.AdminRole(ctx, "c2c-B", "union-B", "c2c", "c2c-B"); !ok || role != RoleAdmin {
		t.Fatalf("群内 admin 应能在私聊使用，role=%q ok=%v", role, ok)
	}
	if _, _, ok, _ := db.AdminRole(ctx, "member-B", "union-B", "group", "g2"); ok {
		t.Fatal("g1 的 admin 不应在 g2 生效")
	}

	// 私聊加的 admin 不扩散到群：即便 union 能认出同一个人
	if err := db.LinkIdentity(ctx, "c2c-D", "union-D", "c2c", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkIdentity(ctx, "member-D", "union-D", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "c2c-D", UnionID: "union-D", Scene: "c2c", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.AdminRole(ctx, "c2c-D", "union-D", "c2c", "c2c-D"); !ok {
		t.Fatal("私聊 admin 应在私聊生效")
	}
	if _, _, ok, _ := db.AdminRole(ctx, "member-D", "union-D", "group", "g1"); ok {
		t.Fatal("私聊加的 admin 不应在群里生效")
	}
}

func TestAdminTargetsResolveC2C(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.LinkIdentity(ctx, "member-A", "union-A", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "member-A", UnionID: "union-A", Scene: "group", Role: RoleOwner, Note: "老板",
	}); err != nil {
		t.Fatal(err)
	}

	targets, err := db.AdminTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("应有 1 个收件人，实际 %d", len(targets))
	}
	if targets[0].C2COpenID != "" {
		t.Fatalf("未建立单聊身份时不应有私聊 openid，实际 %q", targets[0].C2COpenID)
	}

	// 私聊过之后应能解析出单聊 openid
	if err := db.LinkIdentity(ctx, "c2c-A", "union-A", "c2c", ""); err != nil {
		t.Fatal(err)
	}
	targets, err = db.AdminTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].C2COpenID != "c2c-A" {
		t.Fatalf("应解析出单聊 openid c2c-A，实际 %+v", targets)
	}
	if targets[0].Role != RoleOwner || targets[0].Note != "老板" {
		t.Fatalf("收件人信息不对: %+v", targets[0])
	}

	// 同一个人又加了一条单聊授权：去重后仍是一条，角色取 owner
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "c2c-A", UnionID: "union-A", Scene: "c2c", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	targets, err = db.AdminTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("同一人应合并为一条，实际 %+v", targets)
	}
	if targets[0].Role != RoleOwner {
		t.Fatalf("合并后角色应取 owner，实际 %q", targets[0].Role)
	}
}

func TestDeleteAdminKeepsOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "owner-1", Scene: "group", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "admin-1", Scene: "group", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}

	// owner 不能被 /admin rm 删掉
	removed, owners, err := db.DeleteAdmin(ctx, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || owners != 1 {
		t.Fatalf("owner 应被拒绝删除: removed=%d owners=%d", removed, owners)
	}
	if n, _ := db.CountAdmins(ctx); n != 2 {
		t.Fatalf("owner 未被删除时应仍有 2 条，实际 %d", n)
	}

	removed, owners, err = db.DeleteAdmin(ctx, "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || owners != 0 {
		t.Fatalf("admin 应被删除: removed=%d owners=%d", removed, owners)
	}
	if _, _, ok, _ := db.AdminRole(ctx, "admin-1", "", "group", "g1"); ok {
		t.Fatal("删除后不应再有管理员权限")
	}
}

func TestUpsertAdminKeepsNoteAndUnion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "u1", UnionID: "union-1", Scene: "group", Role: RoleAdmin, Note: "运维",
	}); err != nil {
		t.Fatal(err)
	}
	// 再次写入但省略备注：不应把已有备注与 union 清空
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Scene: "group", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Note != "运维" || list[0].UnionID != "union-1" {
		t.Fatalf("空字段不应覆盖已有值: %+v", list)
	}
}
