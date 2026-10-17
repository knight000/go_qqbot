package host

import (
	"strings"
)

// 权限模型（ARCHITECTURE.md §8.7）由两个正交维度组成：
//
//	Bot 角色 Role        本系统认定，来自 admins 表：user < admin < owner；
//	群内角色 GroupRole   平台在事件载荷里直接给出（author.member_role）：
//	                     unknown < member < admin < owner。
//
// 两者**叠加而非替代**：Bot 管理员在任何群里都按群管级处理；平台群主不会因此
// 变成 Bot 管理员。这条边界是刻意的——把群主当 Bot 管理员，等于把"谁在这个群
// 里说话有分量"错误地扩张成了"谁能重启机器人"。

// Role 是 Bot 侧的三级角色，严格递增。
type Role string

const (
	// RoleUser 是默认角色：能触发公开指令。
	RoleUser Role = "user"
	// RoleAdmin 是 Bot 管理员：能执行全部管理指令，但不能增删管理员。
	RoleAdmin Role = "admin"
	// RoleOwner 是 Bot 所有者：唯一且不可被增删，能增删管理员。
	RoleOwner Role = "owner"
)

// Level 返回可比较的等级。未知值按 0 处理（fail-closed）。
func (r Role) Level() int {
	switch r {
	case RoleOwner:
		return 2
	case RoleAdmin:
		return 1
	default:
		return 0
	}
}

// Valid 判断该值是否是已知角色。空串视为 RoleUser。
func (r Role) Valid() bool {
	switch r {
	case "", RoleUser, RoleAdmin, RoleOwner:
		return true
	}
	return false
}

func (r Role) String() string {
	if r == "" {
		return string(RoleUser)
	}
	return string(r)
}

// GroupRole 是平台给出的群内身份。
type GroupRole string

const (
	// GroupRoleUnknown 表示平台没给或没识别出来（私聊、通知事件、字段缺失）。
	GroupRoleUnknown GroupRole = ""
	GroupRoleMember  GroupRole = "member"
	GroupRoleAdmin   GroupRole = "admin"
	GroupRoleOwner   GroupRole = "owner"
)

// Level 返回可比较的等级。未知返回 -1，使"至少是管理员"这类判定一律失败。
func (g GroupRole) Level() int {
	switch g {
	case GroupRoleMember:
		return 0
	case GroupRoleAdmin:
		return 1
	case GroupRoleOwner:
		return 2
	}
	return -1
}

func (g GroupRole) Valid() bool { return g.Level() >= 0 }

// ParseGroupRole 归一化平台字段。平台目前只给 member/admin/owner，
// 这里对大小写与空白宽容，无法识别的一律降级为未知——宁可少给权限。
func ParseGroupRole(s string) GroupRole {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(GroupRoleMember):
		return GroupRoleMember
	case string(GroupRoleAdmin):
		return GroupRoleAdmin
	case string(GroupRoleOwner):
		return GroupRoleOwner
	}
	return GroupRoleUnknown
}

// Principal 是一次事件的调用者身份：Bot 角色 + 群内角色。
//
// 由核心在投递插件**之前**填好（Event.Actor），插件只读。
// 权限判定放在核心的收益是：插件即使忘了写 if，也不会越权——
// 声明式权限在路由阶段就把它拦下了。
type Principal struct {
	OpenID    string
	UnionID   string
	Scene     Scene
	PeerID    string
	Role      Role
	GroupRole GroupRole
}

func (p Principal) Identity() Identity {
	return Identity{OpenID: p.OpenID, UnionID: p.UnionID, Scene: p.Scene, PeerID: p.PeerID}
}

func (p Principal) IsBotAdmin() bool { return p.Role.Level() >= RoleAdmin.Level() }
func (p Principal) IsBotOwner() bool { return p.Role == RoleOwner }

// CanGroupAdmin 表示可否行使"群管级"权限：平台群管/群主，或 Bot 管理员。
func (p Principal) CanGroupAdmin() bool {
	return p.GroupRole.Level() >= GroupRoleAdmin.Level() || p.IsBotAdmin()
}

// CanGroupOwner 表示可否行使"群主级"权限：平台群主，或 Bot 管理员。
//
// 注意这是"权限叠加"，不代表该用户在平台上真是群主。
func (p Principal) CanGroupOwner() bool {
	return p.GroupRole == GroupRoleOwner || p.IsBotAdmin()
}

// RoleLabel 是角色的中文说法，只用于对人输出。
func (p Principal) RoleLabel() string {
	switch p.Role {
	case RoleOwner:
		return "Bot 所有者"
	case RoleAdmin:
		return "Bot 管理员"
	default:
		return "普通用户"
	}
}

// GroupRoleLabel 是群内角色的中文说法；未知返回空串。
func (p Principal) GroupRoleLabel() string {
	switch p.GroupRole {
	case GroupRoleOwner:
		return "群主"
	case GroupRoleAdmin:
		return "群管理员"
	case GroupRoleMember:
		return "群成员"
	}
	return ""
}

// Perm 是权限点集合。多个点用按位或组合，语义是"任一满足即可"（与 nonebot 的
// permission=SUPERUSER | GROUP_ADMIN 一致）。零值等价于 PermUser。
type Perm uint32

const (
	// PermUser 覆盖所有人（含私聊、通知事件）。
	PermUser Perm = 1 << iota
	// PermGroupMember 要求事件来自群聊（群主与群管也满足）。
	PermGroupMember
	// PermGroupAdmin 要求群管级：平台群管/群主，或 Bot 管理员。
	PermGroupAdmin
	// PermGroupOwner 要求群主级：平台群主，或 Bot 管理员。
	PermGroupOwner
	// PermBotAdmin 要求 Bot 管理员及以上。
	PermBotAdmin
	// PermBotOwner 要求 Bot 所有者。
	PermBotOwner
)

// PermAll 是全部已定义权限点的并集，用于校验非法位。
const PermAll = PermUser | PermGroupMember | PermGroupAdmin | PermGroupOwner | PermBotAdmin | PermBotOwner

// Valid 判断是否只包含已定义的位。装配阶段据此拒绝拼错的权限声明。
func (p Perm) Valid() bool { return p&^PermAll == 0 }

// Empty 表示"未声明权限"，即不限制。
func (p Perm) Empty() bool { return p == 0 }

// Admits 判定调用者是否满足任一权限点。零值放行一切。
func (p Perm) Admits(pr Principal) bool {
	if p == 0 || p&PermUser != 0 {
		return true
	}
	if p&PermBotOwner != 0 && pr.IsBotOwner() {
		return true
	}
	if p&PermBotAdmin != 0 && pr.IsBotAdmin() {
		return true
	}
	if pr.Scene != SceneGroup {
		// 群相关的三个权限点只在群聊里成立
		return false
	}
	if p&PermGroupMember != 0 {
		return true
	}
	if p&PermGroupOwner != 0 && pr.CanGroupOwner() {
		return true
	}
	if p&PermGroupAdmin != 0 && pr.CanGroupAdmin() {
		return true
	}
	return false
}

// NeedsGroupRole 表示判定需要群内角色（且群内角色可能来自平台载荷而非本地库）。
func (p Perm) NeedsGroupRole() bool {
	return p&(PermGroupAdmin|PermGroupOwner) != 0
}

// String 输出给人看的组合，如 "bot-admin|group-owner"；空集输出空串。
func (p Perm) String() string {
	if p == 0 {
		return ""
	}
	names := []struct {
		bit  Perm
		name string
	}{
		{PermUser, "user"},
		{PermGroupMember, "group-member"},
		{PermGroupAdmin, "group-admin"},
		{PermGroupOwner, "group-owner"},
		{PermBotAdmin, "bot-admin"},
		{PermBotOwner, "bot-owner"},
	}
	var out []string
	for _, n := range names {
		if p&n.bit != 0 {
			out = append(out, n.name)
		}
	}
	// 未知位也要显式暴露，否则"权限写错"会表现为"谁都能用"
	if rest := p &^ PermAll; rest != 0 {
		out = append(out, "unknown")
	}
	return strings.Join(out, "|")
}
