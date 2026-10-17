package host

import "testing"

// 权限判定是安全边界，这里把矩阵逐格钉住：任何一格变化都必须是有意为之。

func TestRoleOrdering(t *testing.T) {
	if !(RoleUser.Level() < RoleAdmin.Level() && RoleAdmin.Level() < RoleOwner.Level()) {
		t.Fatal("角色必须严格递增：user < admin < owner")
	}
	if !RoleUser.Valid() || !RoleAdmin.Valid() || !RoleOwner.Valid() || !Role("").Valid() {
		t.Fatal("已知角色与空串应合法")
	}
	if Role("superuser").Valid() {
		t.Fatal("未知角色不应合法")
	}
	if Role("").String() != "user" {
		t.Fatalf("零值角色应按 user 处理，实际 %q", Role("").String())
	}
}

func TestParseGroupRole(t *testing.T) {
	cases := map[string]GroupRole{
		"member":    GroupRoleMember,
		" ADMIN ":   GroupRoleAdmin,
		"Owner":     GroupRoleOwner,
		"":          GroupRoleUnknown,
		"moderator": GroupRoleUnknown,
		"群主":        GroupRoleUnknown,
	}
	for in, want := range cases {
		if got := ParseGroupRole(in); got != want {
			t.Errorf("ParseGroupRole(%q) = %q, want %q", in, got, want)
		}
	}
}

// 未知群内角色必须按"低于成员"处理：平台没给字段时不能凭空满足群管权限。
func TestUnknownGroupRoleFailsClosed(t *testing.T) {
	if GroupRoleUnknown.Level() != -1 {
		t.Fatalf("未知群角色的 Level 应为 -1，实际 %d", GroupRoleUnknown.Level())
	}
	p := Principal{Scene: SceneGroup, Role: RoleUser, GroupRole: GroupRoleUnknown}
	if PermGroupAdmin.Admits(p) {
		t.Fatal("未知群角色不应满足 group-admin")
	}
	if PermGroupOwner.Admits(p) {
		t.Fatal("未知群角色不应满足 group-owner")
	}
	if !PermGroupMember.Admits(p) {
		t.Fatal("未知群角色的群成员仍应满足 group-member（只要求场景是群）")
	}
}

func TestPermAdmitsMatrix(t *testing.T) {
	group := func(role Role, gr GroupRole) Principal {
		return Principal{Scene: SceneGroup, Role: role, GroupRole: gr}
	}
	c2c := func(role Role) Principal {
		return Principal{Scene: SceneC2C, Role: role}
	}

	cases := []struct {
		name string
		perm Perm
		p    Principal
		want bool
	}{
		// 零值 = 不限制
		{"零值放行普通用户", 0, c2c(RoleUser), true},
		{"零值放行群成员", 0, group(RoleUser, GroupRoleMember), true},

		{"PermUser 放行私聊", PermUser, c2c(RoleUser), true},
		{"PermGroupMember 拒绝私聊", PermGroupMember, c2c(RoleUser), false},
		{"PermGroupMember 放行群成员", PermGroupMember, group(RoleUser, GroupRoleMember), true},
		{"PermGroupMember 放行群管", PermGroupMember, group(RoleUser, GroupRoleAdmin), true},

		{"group-admin 放行平台群管", PermGroupAdmin, group(RoleUser, GroupRoleAdmin), true},
		{"group-admin 放行平台群主", PermGroupAdmin, group(RoleUser, GroupRoleOwner), true},
		{"group-admin 拒绝群成员", PermGroupAdmin, group(RoleUser, GroupRoleMember), false},
		{"group-admin 拒绝私聊", PermGroupAdmin, c2c(RoleUser), false},
		{"group-admin 放行 Bot 管理员（叠加）", PermGroupAdmin, group(RoleAdmin, GroupRoleMember), true},
		{"group-admin 拒绝私聊里的 Bot 管理员（群权限不适用于私聊）", PermGroupAdmin, c2c(RoleAdmin), false},

		{"group-owner 放行平台群主", PermGroupOwner, group(RoleUser, GroupRoleOwner), true},
		{"group-owner 拒绝平台群管", PermGroupOwner, group(RoleUser, GroupRoleAdmin), false},
		{"group-owner 放行 Bot 管理员（叠加）", PermGroupOwner, group(RoleAdmin, GroupRoleMember), true},

		{"bot-admin 放行 admin", PermBotAdmin, group(RoleAdmin, GroupRoleMember), true},
		{"bot-admin 放行 owner", PermBotAdmin, c2c(RoleOwner), true},
		{"bot-admin 拒绝普通用户", PermBotAdmin, group(RoleUser, GroupRoleOwner), false},
		{"bot-admin 拒绝平台群主", PermBotAdmin, group(RoleUser, GroupRoleOwner), false},

		{"bot-owner 放行 owner", PermBotOwner, c2c(RoleOwner), true},
		{"bot-owner 拒绝 admin", PermBotOwner, c2c(RoleAdmin), false},

		// 组合语义是"任一满足"
		{"组合：群主或 Bot 管理员", PermBotAdmin | PermGroupOwner, group(RoleUser, GroupRoleOwner), true},
		{"组合：群主或 Bot 管理员（都不满足）", PermBotAdmin | PermGroupOwner, group(RoleUser, GroupRoleAdmin), false},
		{"组合：普通用户或群管", PermUser | PermGroupAdmin, c2c(RoleUser), true},
	}

	for _, c := range cases {
		if got := c.perm.Admits(c.p); got != c.want {
			t.Errorf("%s: %s.Admits(%+v) = %v, want %v", c.name, c.perm, c.p, got, c.want)
		}
	}
}

func TestPermValidAndString(t *testing.T) {
	if !PermBotAdmin.Valid() || !(PermBotAdmin | PermGroupOwner).Valid() {
		t.Fatal("已定义权限位应合法")
	}
	if Perm(1 << 20).Valid() {
		t.Fatal("未定义位不应合法")
	}
	if got := PermBotAdmin.String(); got != "bot-admin" {
		t.Errorf("String() = %q", got)
	}
	if got := (PermBotAdmin | PermGroupOwner).String(); got != "group-owner|bot-admin" {
		t.Errorf("组合 String() = %q", got)
	}
	if Perm(1<<20).String() != "unknown" {
		t.Errorf("未知位应显式暴露，实际 %q", Perm(1<<20).String())
	}
	if PermBotAdmin.Empty() || !Perm(0).Empty() {
		t.Fatal("Empty 语义错误")
	}
}

func TestPermNeedsGroupRole(t *testing.T) {
	if !PermGroupAdmin.NeedsGroupRole() || !PermGroupOwner.NeedsGroupRole() {
		t.Fatal("群管/群主权限应标记需要群内角色")
	}
	if PermBotAdmin.NeedsGroupRole() || PermUser.NeedsGroupRole() {
		t.Fatal("Bot 级权限不应依赖群内角色")
	}
}

func TestPrincipalLabels(t *testing.T) {
	p := Principal{Role: RoleOwner, GroupRole: GroupRoleOwner}
	if p.RoleLabel() != "Bot 所有者" || p.GroupRoleLabel() != "群主" {
		t.Fatalf("标签错误: %q / %q", p.RoleLabel(), p.GroupRoleLabel())
	}
	if (Principal{Role: RoleUser, GroupRole: GroupRoleUnknown}).GroupRoleLabel() != "" {
		t.Fatal("未知群角色不应有文案")
	}
}
