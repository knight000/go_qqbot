package core

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// newAdminCore 构造一个只装了存储与发送器的最小 Core，用于跑 /admin 指令。
func newAdminCore(t *testing.T) (*Core, *fakeAPI) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	f := &fakeAPI{}
	c := &Core{
		store:  db,
		log:    slog.New(slog.DiscardHandler),
		sender: NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}),
	}
	return c, f
}

func adminEvent(content, sender, union string, scene host.Scene, peer string) *host.Event {
	typ := host.EventGroupMessage
	if scene == host.SceneC2C {
		typ = host.EventC2CMessage
	}
	return &host.Event{
		ID:       fmt.Sprintf("e-%s-%s", sender, content),
		Type:     typ,
		Scene:    scene,
		PeerID:   peer,
		SenderID: sender,
		UnionID:  union,
		MsgID:    "m-" + sender,
		Content:  content,
	}
}

func lastReply(t *testing.T, f *fakeAPI) string {
	t.Helper()
	if len(f.bodies) == 0 {
		t.Fatal("没有发出任何回复")
	}
	return f.bodies[len(f.bodies)-1].Content
}

func seedAdmin(t *testing.T, c *Core, openid, union, scene, scope, role string) {
	t.Helper()
	if err := c.store.UpsertAdmin(context.Background(), store.Admin{
		OpenID: openid, UnionID: union, Scene: scene, PeerScope: scope, Role: role,
	}); err != nil {
		t.Fatalf("预置管理员: %v", err)
	}
}

func TestAdminRejectsNonAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	ev := adminEvent("/admin list", "stranger", "", host.SceneGroup, "g1")

	if !c.handleReserved(context.Background(), ev) {
		t.Fatal("/admin 应被核心消费")
	}
	if got := lastReply(t, f); !strings.Contains(got, "仅管理员") {
		t.Fatalf("非管理员应被拒绝，实际: %q", got)
	}
}

// admin 与 owner 权限相同，唯一的例外就是不能增删管理员。
func TestAdminCannotMutateAdmins(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", "group", "g1", store.RoleAdmin)
	ctx := context.Background()

	for _, cmd := range []string{"/admin add target-1", "/admin rm admin-1"} {
		ev := adminEvent(cmd, "admin-1", "", host.SceneGroup, "g1")
		c.handleReserved(ctx, ev)
		if got := lastReply(t, f); !strings.Contains(got, "仅 owner") {
			t.Errorf("%s 应由 owner 专属，实际: %q", cmd, got)
		}
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "target-1", "", "group", "g1"); ok {
		t.Fatal("admin 不应能添加管理员")
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "admin-1", "", "group", "g1"); !ok {
		t.Fatal("admin 不应能删除自己")
	}
}

// admin 能执行其它管理指令（这里用 /admin list 代表"相同权限"）。
func TestAdminCanUseOtherCommands(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", "group", "g1", store.RoleAdmin)

	c.handleReserved(context.Background(), adminEvent("/admin list", "admin-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "admin-1") {
		t.Fatalf("admin 应能查看管理员列表，实际: %q", got)
	}
}

func TestOwnerAddsAdminScopedToCurrentGroup(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin add target-1 运维", "owner-1", "", host.SceneGroup, "g1"))
	got := lastReply(t, f)
	if !strings.Contains(got, "已添加 admin") || !strings.Contains(got, "运维") {
		t.Fatalf("添加回复不对: %q", got)
	}

	role, scope, ok, err := c.store.AdminRole(ctx, "target-1", "", "group", "g1")
	if err != nil || !ok || role != store.RoleAdmin {
		t.Fatalf("目标应成为 admin: role=%q ok=%v err=%v", role, ok, err)
	}
	if scope != "g1" {
		t.Fatalf("授权范围应为当前群 g1，实际 %q", scope)
	}
	// 换一个群不应生效（最小权限）
	if _, _, ok, _ := c.store.AdminRole(ctx, "target-1", "", "group", "g2"); ok {
		t.Fatal("在 g1 添加的 admin 不应在 g2 生效")
	}

	// 重复添加给出明确提示，不产生第二条授权
	c.handleReserved(ctx, adminEvent("/admin add target-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已经是 admin") {
		t.Fatalf("重复添加应提示已存在，实际: %q", got)
	}
	if list, _ := c.store.ListAdmins(ctx); len(list) != 2 {
		t.Fatalf("应有 owner + admin 两条授权，实际 %d", len(list))
	}
}

func TestOwnerAddSelfIsRejected(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)

	c.handleReserved(context.Background(),
		adminEvent("/admin add owner-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已是 owner") {
		t.Fatalf("重复添加 owner 应提示，实际: %q", got)
	}
}

func TestOwnerRemovesAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", "group", "g1", store.RoleAdmin)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin rm admin-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已删除 1 条") {
		t.Fatalf("删除回复不对: %q", got)
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "admin-1", "", "group", "g1"); ok {
		t.Fatal("删除后不应再有权限")
	}

	// owner 自身不能被删
	c.handleReserved(ctx, adminEvent("/admin rm owner-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "不能删除 owner") {
		t.Fatalf("删除 owner 应被拒绝，实际: %q", got)
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "owner-1", "", "group", "g1"); !ok {
		t.Fatal("owner 不应被删除")
	}

	// 不存在的目标
	c.handleReserved(ctx, adminEvent("/admin rm nobody", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "未找到") {
		t.Fatalf("未知目标应提示未找到，实际: %q", got)
	}
}

// 在私聊里添加：范围是单聊，且在群里不生效。
func TestOwnerAddsAdminInC2C(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-c2c", "", "c2c", "", store.RoleOwner)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin add helper-c2c 值班", "owner-c2c", "", host.SceneC2C, "owner-c2c"))
	if got := lastReply(t, f); !strings.Contains(got, "私聊") {
		t.Fatalf("应说明范围是私聊，实际: %q", got)
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "helper-c2c", "", "c2c", "helper-c2c"); !ok {
		t.Fatal("私聊添加的 admin 应在私聊生效")
	}
	if _, _, ok, _ := c.store.AdminRole(ctx, "helper-c2c", "", "group", "g1"); ok {
		t.Fatal("私聊添加的 admin 不应在群里生效")
	}
}

// 按 union_openid 添加时，应落到该身份真实所属的场景。
func TestOwnerAddsAdminByUnion(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)
	ctx := context.Background()

	if err := c.store.LinkIdentity(ctx, "member-x", "union-x", "group", "g2"); err != nil {
		t.Fatal(err)
	}
	c.handleReserved(ctx, adminEvent("/admin add union-x 外部", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已添加 admin") {
		t.Fatalf("按 union 添加应成功，实际: %q", got)
	}
	// 身份属于 g2，授权应落在 g2 而不是发指令的 g1
	if _, _, ok, _ := c.store.AdminRole(ctx, "member-x", "union-x", "group", "g2"); !ok {
		t.Fatal("授权应落在该身份所属的 g2")
	}
}

func TestWhoamiOpenForEveryone(t *testing.T) {
	c, f := newAdminCore(t)

	// 私聊里任何人都能查自己的 openid（添加管理员的前提）
	c.handleReserved(context.Background(),
		adminEvent("/admin whoami", "stranger", "union-s", host.SceneC2C, "stranger"))
	got := lastReply(t, f)
	for _, want := range []string{"非管理员", "openid stranger", "union-s"} {
		if !strings.Contains(got, want) {
			t.Errorf("whoami 缺少 %q:\n%s", want, got)
		}
	}

	// 群里同样开放，但只回自己的标识
	c.handleReserved(context.Background(),
		adminEvent("/admin whoami", "member-1", "union-m", host.SceneGroup, "g1"))
	got = lastReply(t, f)
	for _, want := range []string{"openid member-1", "群"} {
		if !strings.Contains(got, want) {
			t.Errorf("群内 whoami 缺少 %q:\n%s", want, got)
		}
	}

	// 其它子命令仍然只对管理员开放
	c.handleReserved(context.Background(),
		adminEvent("/admin list", "member-1", "union-m", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "仅管理员") {
		t.Fatalf("非管理员不应能 list，实际: %q", got)
	}
}

func TestAdminUsageAndWhoami(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "union-1", "group", "", store.RoleOwner)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin", "owner-1", "union-1", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "/admin add") {
		t.Fatalf("无子命令应给出用法，实际: %q", got)
	}

	c.handleReserved(ctx, adminEvent("/admin whoami", "owner-1", "union-1", host.SceneGroup, "g1"))
	got := lastReply(t, f)
	for _, want := range []string{"owner", "openid owner-1", "union-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("whoami 缺少 %q:\n%s", want, got)
		}
	}

	c.handleReserved(ctx, adminEvent("/admin nope", "owner-1", "union-1", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "未知子命令") {
		t.Fatalf("未知子命令应提示，实际: %q", got)
	}
}

func TestAdminListShowsRoleAndScope(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", "group", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", "group", "g1", store.RoleAdmin)

	c.handleReserved(context.Background(), adminEvent("/admin list", "owner-1", "", host.SceneGroup, "g1"))
	got := lastReply(t, f)
	for _, want := range []string{"管理员 2 条", "owner owner-1", "admin admin-1", "全部群"} {
		if !strings.Contains(got, want) {
			t.Errorf("list 缺少 %q:\n%s", want, got)
		}
	}
}
