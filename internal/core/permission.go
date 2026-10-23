package core

import (
	"context"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 权限的填充与判定都放在核心，插件只读 Event.Actor。分两条来源：
//
//	群内角色 GroupRole —— 网关从平台消息载荷的 author.member_role 解出；
//	Bot 角色  Role      —— 这里查 admins 表补上。
//
// 这样插件不需要、也无法自己拼身份；声明式权限（Manifest/Command.Permission）
// 在路由阶段就生效，插件里漏写判断不会导致越权。

// resolveActor 填充 ev.Actor 的 Bot 角色。可重复调用（幂等）。
//
// 查询失败按 fail-closed 处理：角色保持 user 并记 Error——宁可管理员这次用不了，
// 也不能因为库故障把管理员权限发给不确定的人。
func (c *Core) resolveActor(ctx context.Context, ev *host.Event) {
	if ev.Actor.Role != "" && ev.Actor.OpenID == ev.SenderID {
		return // 已解析过
	}

	ev.Actor.OpenID = ev.SenderID
	ev.Actor.UnionID = ev.UnionID
	ev.Actor.Scene = ev.Scene
	ev.Actor.PeerID = ev.PeerID
	ev.Actor.Role = host.RoleUser
	// GroupRole 由网关填，核心没有比平台更权威的来源，绝不覆盖群聊里的值。
	// 但私聊没有群角色：调用者仍是"普通成员"，因此默认等价于 member——
	// 这让"要求成员及以上"的权限点在私聊里同样成立，而不是因为场景不同失效。
	if ev.Scene == host.SceneC2C && ev.Actor.GroupRole == host.GroupRoleUnknown {
		ev.Actor.GroupRole = host.GroupRoleMember
	}

	if ev.SenderID == "" || c.store == nil {
		return
	}
	// Bot 角色是全局的：openid 跨场景同值，不需要 scene / peer 参与判定。
	role, ok, err := c.store.AdminRole(ctx, ev.SenderID, ev.UnionID)
	if err != nil {
		c.log.Error("查询角色失败，本次按普通用户处理", "event", ev.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	if r := host.Role(role); r.Valid() && r != host.RoleUser {
		ev.Actor.Role = r
		return
	}
	c.log.Warn("admins 表里出现未知或非法角色，按普通用户处理",
		"sender", ev.SenderID, "role", role)
}

// authorize 返回调用者的权限视图，供核心保留指令使用（插件走声明式）。
func (c *Core) authorize(ctx context.Context, ev *host.Event) host.Principal {
	c.resolveActor(ctx, ev)
	return ev.Actor
}
