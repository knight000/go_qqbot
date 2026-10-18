package core

import (
	"context"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// 私聊没有群，但调用者仍是普通成员：权限基线是 member，而不是"未知"。
// 这让 PermGroupMember 这类"要求成员及以上"的声明在私聊里照常成立。
func TestPrivateChatRoleDefaultsToMember(t *testing.T) {
	c, _ := newAdminCore(t)
	ev := adminEvent("/ping", "u1", "", host.SceneC2C, "u1")

	c.resolveActor(context.Background(), ev)

	if ev.Actor.Role != host.RoleUser {
		t.Fatalf("陌生人应是普通用户，实际 %q", ev.Actor.Role)
	}
	if ev.Actor.GroupRole != host.GroupRoleMember {
		t.Fatalf("私聊角色基线应为 member，实际 %q", ev.Actor.GroupRole)
	}
	if !host.PermGroupMember.Admits(ev.Actor) {
		t.Fatal("私聊成员应满足 group-member")
	}
	if host.PermGroupAdmin.Admits(ev.Actor) {
		t.Fatal("私聊成员不应满足 group-admin")
	}
}

// 群聊里的角色来自平台载荷，核心不得覆盖（即使它是 unknown）。
func TestGroupRoleFromPayloadIsNotOverwritten(t *testing.T) {
	c, _ := newAdminCore(t)
	ev := adminEvent("/ping", "u1", "", host.SceneGroup, "g1")
	ev.Actor.GroupRole = host.GroupRoleOwner

	c.resolveActor(context.Background(), ev)
	if ev.Actor.GroupRole != host.GroupRoleOwner {
		t.Fatalf("群内角色不应被核心覆盖，实际 %q", ev.Actor.GroupRole)
	}
}
