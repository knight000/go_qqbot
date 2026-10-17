package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 旧库的 admins 有 peer_scope、缺 note / union_openid 列。Migrate 必须先补列、
// 再按 openid 去重搬进全局角色表；整个过程可重复执行。
func TestMigrateLegacyAdminsToGlobalRoles(t *testing.T) {
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
	// 同一个人在群里的 admin 授权 + 单聊认领的 owner：迁移后必须只剩一条 owner。
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO admins(openid, scene, peer_scope, role, granted_by, created_at)
		 VALUES('u1', 'group', 'g1', 'admin', 'owner-1', 100),
		       ('u1', 'c2c', '', 'owner', 'claim', 90),
		       ('u2', 'group', 'g2', 'admin', 'owner-1', 95)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("旧库迁移失败: %v", err)
	}

	var ddl string
	if err := db.r.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'admins'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ddl, "peer_scope") {
		t.Fatalf("迁移后不应再有 peer_scope 列:\n%s", ddl)
	}

	list, err := db.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("同一人应合并为一条，实际 %d 条: %+v", len(list), list)
	}
	byID := map[string]Admin{}
	for _, a := range list {
		byID[a.OpenID] = a
	}
	if byID["u1"].Role != RoleOwner {
		t.Fatalf("同一人多条授权应保留 owner，实际 %+v", byID["u1"])
	}
	if byID["u1"].SourceScene != "c2c" {
		t.Fatalf("来源场景应记录为 c2c，实际 %q", byID["u1"].SourceScene)
	}
	if byID["u2"].Role != RoleAdmin {
		t.Fatalf("u2 应为 admin，实际 %+v", byID["u2"])
	}

	// 迁移后新表可正常写入（note 列存在）
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u3", Role: RoleAdmin, Note: "备注"}); err != nil {
		t.Fatalf("迁移后写入失败: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移应幂等: %v", err)
	}
}

// 角色是全局的：群内添加与私聊添加效果一致，查询不需要场景参数。
func TestAdminRoleIsGlobal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "owner-1", Role: RoleOwner, SourceScene: "group"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "admin-1", Role: RoleAdmin, SourceScene: "c2c"}); err != nil {
		t.Fatal(err)
	}

	if role, ok, err := db.AdminRole(ctx, "owner-1", ""); err != nil || !ok || role != RoleOwner {
		t.Fatalf("owner 应全局有效: role=%q ok=%v err=%v", role, ok, err)
	}
	if role, ok, err := db.AdminRole(ctx, "admin-1", ""); err != nil || !ok || role != RoleAdmin {
		t.Fatalf("私聊添加的 admin 应全局有效: role=%q ok=%v err=%v", role, ok, err)
	}
	if _, ok, _ := db.AdminRole(ctx, "stranger", ""); ok {
		t.Fatal("陌生人不应是管理员")
	}
}

// union_openid 是兜底：openid 对不上时，用它把同一个人认出来。
func TestAdminRoleUnionFallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.LinkIdentity(ctx, "member-A", "union-A", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "member-A", UnionID: "union-A", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}

	// 直接命中 union
	if role, ok, _ := db.AdminRole(ctx, "", "union-A"); !ok || role != RoleOwner {
		t.Fatalf("按 union 应识别为 owner: role=%q ok=%v", role, ok)
	}
	// 另一个 openid + union：经 identities 关联到 member-A
	if role, ok, _ := db.AdminRole(ctx, "c2c-A", "union-A"); !ok || role != RoleOwner {
		t.Fatalf("经 identities 关联应识别为 owner: role=%q ok=%v", role, ok)
	}
	// 没有 union 时不能凭空放行
	if _, ok, _ := db.AdminRole(ctx, "c2c-A", ""); ok {
		t.Fatal("缺少 union_openid 时不应跨 openid 匹配")
	}
}

func TestAdminTargetsResolveC2C(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.LinkIdentity(ctx, "member-A", "union-A", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "member-A", UnionID: "union-A", Role: RoleOwner, Note: "老板",
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
}

func TestDeleteAdminKeepsOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "owner-1", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "admin-1", Role: RoleAdmin}); err != nil {
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
	if _, ok, _ := db.AdminRole(ctx, "admin-1", ""); ok {
		t.Fatal("删除后不应再有管理员权限")
	}
}

// 空字段不覆盖已有值；owner 不会被 upsert 降级。
func TestUpsertAdminKeepsNoteUnionAndOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "u1", UnionID: "union-1", Role: RoleAdmin, Note: "运维", SourceScene: "group",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Note != "运维" || list[0].UnionID != "union-1" ||
		list[0].SourceScene != "group" {
		t.Fatalf("空字段不应覆盖已有值: %+v", list)
	}

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAdmin(ctx, Admin{OpenID: "u1", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if role, _, _ := db.AdminRole(ctx, "u1", ""); role != RoleOwner {
		t.Fatalf("owner 不应被 upsert 降级，实际 %q", role)
	}
}

// 回归：在单聊里 /claim 的 owner，进群后仍是 owner。
// 旧实现把 openid 匹配限定在 scene 内，而群里查询用的是 scene=group。
func TestAdminRoleOwnerFromC2CWorksInGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{
		OpenID: "u1", Role: RoleOwner, GrantedBy: "claim", SourceScene: "c2c",
	}); err != nil {
		t.Fatal(err)
	}
	// 平台未下发 union_openid，只能靠同值 openid；角色查询本身已无场景概念。
	role, ok, err := db.AdminRole(ctx, "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || role != RoleOwner {
		t.Fatalf("单聊认领的 owner 在群里应仍为 owner: role=%q ok=%v", role, ok)
	}
}
