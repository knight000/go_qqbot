package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /admin 属于核心保留指令（不在插件路由里），因为它是权限本身的入口：
// 一旦允许插件接管，插件就能给自己提权。
//
// 权限模型分三级（ARCHITECTURE.md §8.7）：
//
//	user  普通用户 —— 默认角色，只能触发公开指令；
//	admin Bot 管理员 —— 能执行全部管理指令，但**不能增删管理员**；
//	owner Bot 所有者 —— 唯一，认领产生，能增删 admin，自身不可被增删。
//
// 另有一个正交维度：平台给出的群内角色（群主 / 群管理员 / 群成员）。
// 它不是 Bot 角色——群主不会因此获得重启机器人的权限；反过来 Bot 管理员
// 在任何群里都按群管级处理（权限叠加）。
const adminUsage = `用法：
/admin list                 查看管理员
/admin add <openid> [备注]   添加 Bot 管理员（仅所有者）
/admin rm <openid>           删除 Bot 管理员（仅所有者）
/admin whoami               查看自己的角色与标识

角色分三级：普通用户 < Bot 管理员 < Bot 所有者；角色全局有效，群里加与私聊加一致。
群主 / 群管理员是平台身份，不等于 Bot 管理员。
whoami 对所有人开放（只返回你自己的标识）：把 openid 发给所有者才能被添加。`

// adminStore 把 store.DB 的管理员能力适配成 host.AdminBackend。
// 单独一层是为了不让 host 依赖 store，也不让插件摸到数据库。
type adminStore struct{ db *store.DB }

func (a adminStore) AdminRole(ctx context.Context, openid, unionID string) (string, bool, error) {
	return a.db.AdminRole(ctx, openid, unionID)
}

func (a adminStore) AdminTargets(ctx context.Context) ([]host.AdminTarget, error) {
	rows, err := a.db.AdminTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]host.AdminTarget, len(rows))
	for i, r := range rows {
		out[i] = host.AdminTarget{Role: r.Role, C2COpenID: r.C2COpenID, Note: r.Note}
	}
	return out, nil
}

func (c *Core) handleAdmin(ctx context.Context, ev *host.Event) {
	fields := strings.Fields(strings.TrimSpace(ev.Content))
	sub := ""
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}

	p := c.authorize(ctx, ev)
	if !p.IsBotAdmin() {
		// whoami 对所有人开放：要把某人加成 admin，所有者必须先知道对方的
		// openid，而平台不提供 QQ 号。它只回调用者自己的标识，不泄露他人。
		if sub == "whoami" {
			c.adminWhoami(ctx, ev, p)
			return
		}
		c.replyText(ctx, ev, "该指令仅管理员可用。")
		return
	}

	switch sub {
	case "", "help":
		c.replyText(ctx, ev, adminUsage)
	case "list":
		c.adminList(ctx, ev)
	case "whoami":
		c.adminWhoami(ctx, ev, p)
	case "add":
		c.adminAdd(ctx, ev, p, fields)
	case "rm", "remove", "del":
		c.adminRemove(ctx, ev, p, fields)
	default:
		c.replyText(ctx, ev, "未知子命令。\n"+adminUsage)
	}
}

func (c *Core) adminList(ctx context.Context, ev *host.Event) {
	admins, err := c.store.ListAdmins(ctx)
	if err != nil {
		c.log.Error("列出管理员失败", "err", err)
		c.replyText(ctx, ev, "读取管理员列表失败，请查看日志。")
		return
	}
	if len(admins) == 0 {
		c.replyText(ctx, ev, "当前没有管理员（数据异常，检查 admins 表）。")
		return
	}

	const maxLines = 20
	var b strings.Builder
	fmt.Fprintf(&b, "管理员 %d 条（全局角色，所有群与私聊）\n", len(admins))
	for i, a := range admins {
		if i == maxLines {
			fmt.Fprintf(&b, "…另有 %d 条，未显示\n", len(admins)-maxLines)
			break
		}
		fmt.Fprintf(&b, "· %s %s", a.Role, a.OpenID)
		if a.Note != "" {
			fmt.Fprintf(&b, "（%s）", a.Note)
		}
		if src := describeScene(a.SourceScene); src != "" {
			fmt.Fprintf(&b, "｜%s添加", src)
		}
		b.WriteString("\n")
	}
	c.replyText(ctx, ev, strings.TrimRight(b.String(), "\n"))
}

func (c *Core) adminWhoami(ctx context.Context, ev *host.Event, p host.Principal) {
	var b strings.Builder
	fmt.Fprintf(&b, "角色 %s\n", p.RoleLabel())

	// 群内角色来自平台载荷，只在群消息里有。
	// 私聊虽然按 member 作为权限基线，但不是"群内角色"，不在这里展示。
	if p.Scene == host.SceneGroup {
		if gr := p.GroupRoleLabel(); gr != "" {
			fmt.Fprintf(&b, "群内 %s（本群）\n", gr)
		}
	}

	fmt.Fprintf(&b, "openid %s\n", ev.SenderID)
	if ev.UnionID != "" {
		fmt.Fprintf(&b, "union  %s\n", ev.UnionID)
	} else {
		b.WriteString("union  平台未提供\n")
	}
	fmt.Fprintf(&b, "场景 %s", ev.Scene)
	if ev.Scene == host.SceneGroup {
		fmt.Fprintf(&b, "｜群 %s", shortID(ev.PeerID))
	}
	if !p.IsBotAdmin() {
		b.WriteString("\n把 openid 发给 Bot 所有者，即可被添加为管理员。")
	}
	c.replyText(ctx, ev, strings.TrimRight(b.String(), "\n"))
}

// adminAdd 只允许 owner 调用，且新建的角色恒为 admin——
// /admin 不提供任何"再把别人变成 owner"的路径，保证所有者唯一。
func (c *Core) adminAdd(ctx context.Context, ev *host.Event, p host.Principal, fields []string) {
	if !p.IsBotOwner() {
		c.replyText(ctx, ev, "仅 Bot 所有者可以添加或删除管理员。")
		return
	}
	if len(fields) < 3 {
		c.replyText(ctx, ev, "用法：/admin add <openid> [备注]")
		return
	}

	ident := fields[2]
	note := strings.Join(fields[3:], " ")
	if r := []rune(note); len(r) > 40 {
		note = string(r[:40])
	}

	// 允许按 union_openid 添加：能解析到已知身份就用它的 openid，
	// 这样"把某人的 union 发过来"也能落到正确的用户上。
	openid, union := ident, ""
	if idn, ok, err := c.store.ResolveIdentity(ctx, ident, string(ev.Scene), peerScope(ev)); err != nil {
		c.log.Warn("解析待添加身份失败，按 openid 处理", "ident", ident, "err", err)
	} else if ok {
		openid, union = idn.OpenID, idn.UnionID
	}

	existing, err := c.store.ListAdmins(ctx)
	if err != nil {
		c.log.Error("读取管理员列表失败", "err", err)
		c.replyText(ctx, ev, "读取管理员列表失败，请查看日志。")
		return
	}
	for _, a := range existing {
		// 角色全局唯一：同一个人不该有第二条授权（openid 或 union 命中都算）。
		if a.OpenID != openid && (union == "" || a.UnionID != union) {
			continue
		}
		if a.Role == store.RoleOwner {
			c.replyText(ctx, ev, "该用户已是 Bot 所有者，无需添加。")
			return
		}
		c.replyText(ctx, ev, "该用户已经是 admin（全局角色）。")
		return
	}

	row := store.Admin{
		OpenID:      openid,
		UnionID:     union,
		Role:        store.RoleAdmin,
		Note:        note,
		GrantedBy:   ev.SenderID,
		SourceScene: string(ev.Scene),
	}
	if err := c.store.UpsertAdmin(ctx, row); err != nil {
		c.log.Error("写入管理员失败", "openid", openid, "err", err)
		c.replyText(ctx, ev, "写入失败，请查看日志。")
		return
	}
	c.log.Info("已添加管理员", "openid", openid, "role", store.RoleAdmin,
		"source", ev.Scene, "by", ev.SenderID)

	var b strings.Builder
	fmt.Fprintf(&b, "已添加 admin：%s", openid)
	if note != "" {
		fmt.Fprintf(&b, "（%s）", note)
	}
	b.WriteString("\n角色全局有效：所有群与私聊，除增删管理员外权限与所有者相同")
	c.replyText(ctx, ev, b.String())
}

func (c *Core) adminRemove(ctx context.Context, ev *host.Event, p host.Principal, fields []string) {
	if !p.IsBotOwner() {
		c.replyText(ctx, ev, "仅 Bot 所有者可以添加或删除管理员。")
		return
	}
	if len(fields) < 3 {
		c.replyText(ctx, ev, "用法：/admin rm <openid>")
		return
	}

	ident := fields[2]
	removed, owners, err := c.store.DeleteAdmin(ctx, ident)
	if err != nil {
		c.log.Error("删除管理员失败", "ident", ident, "err", err)
		c.replyText(ctx, ev, "删除失败，请查看日志。")
		return
	}

	switch {
	case removed > 0:
		c.log.Info("已删除管理员", "ident", ident, "rows", removed, "by", ev.SenderID)
		c.replyText(ctx, ev, fmt.Sprintf("已删除 %d 条 admin 授权：%s", removed, ident))
	case owners > 0:
		c.replyText(ctx, ev, "该用户是 Bot 所有者，/admin rm 不能删除所有者。")
	default:
		c.replyText(ctx, ev, "未找到该 admin。用 /admin list 查看现有管理员。")
	}
}

// describeScene 把来源场景说成人话，只用于 /admin list 的注脚。
func describeScene(scene string) string {
	switch scene {
	case string(host.SceneGroup):
		return "群聊"
	case string(host.SceneC2C):
		return "私聊"
	}
	return ""
}

// shortID 保留首尾各 6 位，够人眼区分又不把整条 openid 刷满屏。
func shortID(s string) string {
	const n = 6
	r := []rune(s)
	if len(r) <= n*2+1 {
		return s
	}
	return string(r[:n]) + "…" + string(r[len(r)-n:])
}
