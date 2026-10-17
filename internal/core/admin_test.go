package core

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knight000/qqbot/internal/config"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// newAdminCore 构造一个只装了存储与发送器的最小 Core，用于跑 /admin 与 /group 指令。
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
		store:     db,
		cfg:       &config.Config{},
		log:       slog.New(slog.DiscardHandler),
		sender:    NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}),
		startedAt: time.Now(),
		restart:   make(chan RestartRequest, 1),
	}
	return c, f
}

// 事件 ID 与 msg_id 都要唯一：去重表会拦重复事件，被动回复额度按 msg_id 计。
var testEventSeq atomic.Int64

func adminEvent(content, sender, union string, scene host.Scene, peer string) *host.Event {
	n := testEventSeq.Add(1)
	return &host.Event{
		ID:       fmt.Sprintf("e-%s-%s-%d", sender, content, n),
		Kind:     host.KindMessage,
		Name:     "TEST_MESSAGE_CREATE",
		Scene:    scene,
		PeerID:   peer,
		SenderID: sender,
		UnionID:  union,
		MsgID:    fmt.Sprintf("m-%s-%d", sender, n),
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

func seedAdmin(t *testing.T, c *Core, openid, union, role string) {
	t.Helper()
	if err := c.store.UpsertAdmin(context.Background(), store.Admin{
		OpenID: openid, UnionID: union, Role: role, SourceScene: "group",
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
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	ctx := context.Background()

	for _, cmd := range []string{"/admin add target-1", "/admin rm admin-1"} {
		ev := adminEvent(cmd, "admin-1", "", host.SceneGroup, "g1")
		c.handleReserved(ctx, ev)
		if got := lastReply(t, f); !strings.Contains(got, "仅 Bot 所有者") {
			t.Errorf("%s 应由 owner 专属，实际: %q", cmd, got)
		}
	}
	if _, ok, _ := c.store.AdminRole(ctx, "target-1", ""); ok {
		t.Fatal("admin 不应能添加管理员")
	}
	if _, ok, _ := c.store.AdminRole(ctx, "admin-1", ""); !ok {
		t.Fatal("admin 不应能删除自己")
	}
}

// admin 能执行其它管理指令（这里用 /admin list 代表"相同权限"）。
func TestAdminCanUseOtherCommands(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	c.handleReserved(context.Background(), adminEvent("/admin list", "admin-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "admin-1") {
		t.Fatalf("admin 应能查看管理员列表，实际: %q", got)
	}
}

// 角色是全局的：在群里添加的 admin，私聊与其它群都同样生效。
func TestOwnerAddsAdminGlobally(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin add target-1 运维", "owner-1", "", host.SceneGroup, "g1"))
	got := lastReply(t, f)
	if !strings.Contains(got, "已添加 admin") || !strings.Contains(got, "运维") {
		t.Fatalf("添加回复不对: %q", got)
	}
	if !strings.Contains(got, "全局") {
		t.Fatalf("应说明角色全局有效，实际: %q", got)
	}

	role, ok, err := c.store.AdminRole(ctx, "target-1", "")
	if err != nil || !ok || role != store.RoleAdmin {
		t.Fatalf("目标应成为 admin: role=%q ok=%v err=%v", role, ok, err)
	}

	// 重复添加给出明确提示，不产生第二条授权
	c.handleReserved(ctx, adminEvent("/admin add target-1", "owner-1", "", host.SceneC2C, "owner-1"))
	if got := lastReply(t, f); !strings.Contains(got, "已经是 admin") {
		t.Fatalf("重复添加应提示已存在，实际: %q", got)
	}
	if list, _ := c.store.ListAdmins(ctx); len(list) != 2 {
		t.Fatalf("应有 owner + admin 两条授权，实际 %d", len(list))
	}
}

func TestOwnerAddSelfIsRejected(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	c.handleReserved(context.Background(),
		adminEvent("/admin add owner-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已是 Bot 所有者") {
		t.Fatalf("重复添加 owner 应提示，实际: %q", got)
	}
}

func TestOwnerRemovesAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin rm admin-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已删除 1 条") {
		t.Fatalf("删除回复不对: %q", got)
	}
	if _, ok, _ := c.store.AdminRole(ctx, "admin-1", ""); ok {
		t.Fatal("删除后不应再有权限")
	}

	// owner 自身不能被删
	c.handleReserved(ctx, adminEvent("/admin rm owner-1", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "不能删除所有者") {
		t.Fatalf("删除 owner 应被拒绝，实际: %q", got)
	}
	if _, ok, _ := c.store.AdminRole(ctx, "owner-1", ""); !ok {
		t.Fatal("owner 不应被删除")
	}

	// 不存在的目标
	c.handleReserved(ctx, adminEvent("/admin rm nobody", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "未找到") {
		t.Fatalf("未知目标应提示未找到，实际: %q", got)
	}
}

// 在私聊里添加同样产生全局角色：进群也生效。
func TestOwnerAddsAdminInC2C(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-c2c", "", store.RoleOwner)
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/admin add helper-c2c 值班", "owner-c2c", "", host.SceneC2C, "owner-c2c"))
	if got := lastReply(t, f); !strings.Contains(got, "全局") {
		t.Fatalf("应说明角色全局有效，实际: %q", got)
	}
	if _, ok, _ := c.store.AdminRole(ctx, "helper-c2c", ""); !ok {
		t.Fatal("私聊添加的 admin 应在任何场景生效")
	}
}

// 按 union_openid 添加时，应落到该 union 对应的真实 openid 上。
func TestOwnerAddsAdminByUnion(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	if err := c.store.LinkIdentity(ctx, "member-x", "union-x", "group", "g2"); err != nil {
		t.Fatal(err)
	}
	c.handleReserved(ctx, adminEvent("/admin add union-x 外部", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "已添加 admin") {
		t.Fatalf("按 union 添加应成功，实际: %q", got)
	}
	if _, ok, _ := c.store.AdminRole(ctx, "member-x", "union-x"); !ok {
		t.Fatal("授权应落在该 union 对应的 member-x 上")
	}
}

func TestWhoamiOpenForEveryone(t *testing.T) {
	c, f := newAdminCore(t)

	// 私聊里任何人都能查自己的 openid（添加管理员的前提）
	c.handleReserved(context.Background(),
		adminEvent("/admin whoami", "stranger", "union-s", host.SceneC2C, "stranger"))
	got := lastReply(t, f)
	for _, want := range []string{"普通用户", "openid stranger", "union-s"} {
		if !strings.Contains(got, want) {
			t.Errorf("whoami 缺少 %q:\n%s", want, got)
		}
	}
	// 私聊的 member 只是权限基线，不该被说成"群内角色"。
	if strings.Contains(got, "群成员") {
		t.Errorf("私聊 whoami 不应展示群内角色:\n%s", got)
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
	seedAdmin(t, c, "owner-1", "union-1", store.RoleOwner)
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

func TestAdminListShowsGlobalRoles(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	c.handleReserved(context.Background(), adminEvent("/admin list", "owner-1", "", host.SceneGroup, "g1"))
	got := lastReply(t, f)
	for _, want := range []string{"管理员 2 条（全局角色", "owner owner-1", "admin admin-1", "群聊添加"} {
		if !strings.Contains(got, want) {
			t.Errorf("list 缺少 %q:\n%s", want, got)
		}
	}
}

// 回归：在单聊里认领的 owner，进群后必须仍是 owner。
// 这是线上实际出现的问题——C2C 里 /ping 正常，同一个群里却判成非管理员。
func TestOwnerClaimedInC2CWorksInGroup(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/admin list", "owner-1", "", host.SceneGroup, "g1"))
	if got := lastReply(t, f); !strings.Contains(got, "管理员 1 条") {
		t.Fatalf("单聊认领的 owner 应能在群里管理，实际: %q", got)
	}
}

// whoami 要同时展示两个维度：Bot 角色与平台群内角色。
func TestWhoamiShowsGroupRole(t *testing.T) {
	c, f := newAdminCore(t)
	ev := adminEvent("/admin whoami", "member-1", "", host.SceneGroup, "g1")
	ev.Actor.GroupRole = host.GroupRoleAdmin

	c.handleReserved(context.Background(), ev)
	got := lastReply(t, f)
	for _, want := range []string{"普通用户", "群管理员", "openid member-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("whoami 缺少 %q:\n%s", want, got)
		}
	}
}

// 平台群主不是 Bot 管理员：群内身份只用于群管级权限，不能换来管理指令。
func TestGroupOwnerIsNotBotAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	ev := adminEvent("/admin list", "group-owner", "", host.SceneGroup, "g1")
	ev.Actor.GroupRole = host.GroupRoleOwner

	c.handleReserved(context.Background(), ev)
	if got := lastReply(t, f); !strings.Contains(got, "仅管理员可用") {
		t.Fatalf("群主不应获得 Bot 管理权限，实际: %q", got)
	}
}
