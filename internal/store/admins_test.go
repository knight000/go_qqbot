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
	// 群内委托只约束"别的群"：同一个人在私聊里仍是 admin
	// （openid 跨场景同值，私聊没有 peer 概念）。
	if role, _, ok, _ := db.AdminRole(ctx, "admin-1", "", "c2c", "admin-1"); !ok || role != RoleAdmin {
		t.Fatalf("admin 应在私聊同样生效: role=%q ok=%v", role, ok)
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

	// 私聊加的 admin 是全局角色：群内也生效
	// （peer_scope 为空 = 全局；这与"群内委托只在那个群生效"是同一条规则的两面）
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
	if _, _, ok, _ := db.AdminRole(ctx, "member-D", "union-D", "group", "g1"); !ok {
		t.Fatal("全局 admin 应在群内生效")
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
	// openid 跨场景同值：没建立单聊映射也能直接私聊。
	// 这是"群认领的 owner 收不到启动通知"的修复点。
	if targets[0].C2COpenID != "member-A" {
		t.Fatalf("应按同值 openid 直接私聊，实际 %q", targets[0].C2COpenID)
	}

	// 私聊过之后，identities 里的映射更权威，优先使用它
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

// 回归：在单聊里 /claim 的 owner，进群后必须仍是 owner。
// 根因是 AdminRole 曾把 openid 匹配限定在 scene 内，而群/单聊的 openid 是同值。
func TestAdminRoleOwnerFromC2CWorksInGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Scene: "c2c", Role: RoleOwner, GrantedBy: "claim"}); err != nil {
		t.Fatal(err)
	}
	// 平台未下发 union_openid，只能靠同值 openid
	role, _, ok, err := db.AdminRole(ctx, "u1", "", "group", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || role != RoleOwner {
		t.Fatalf("单聊认领的 owner 在群里应仍为 owner: role=%q ok=%v", role, ok)
	}
	// 群里认领、私聊使用同样成立
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u2", Scene: "group", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if role, _, ok, _ := db.AdminRole(ctx, "u2", "", "c2c", "u2"); !ok || role != RoleOwner {
		t.Fatalf("群内认领的 owner 在私聊应仍为 owner: role=%q ok=%v", role, ok)
	}
}

// 群内委托（peer_scope 非空）不得越界到别的群。
func TestAdminRoleGroupDelegationIsScoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "a1", Scene: "group", PeerScope: "g1", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.AdminRole(ctx, "a1", "", "group", "g1"); !ok {
		t.Fatal("应在授权群生效")
	}
	if _, _, ok, _ := db.AdminRole(ctx, "a1", "", "group", "g2"); ok {
		t.Fatal("不应在未授权群生效")
	}
}
