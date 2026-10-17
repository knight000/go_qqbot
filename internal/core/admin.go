package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// /admin 属于核心保留指令（不在插件路由里），因为它是权限本身的入口：
// 一旦允许插件接管，插件就能给自己提权。
//
// 权限模型：
//   - owner：认领产生，能增删 admin，其余能力与 admin 相同；
//   - admin：辅助管理员，能执行全部管理指令，但**不能增删管理员**。
//
// 注意：平台不提供 QQ 号。群成员是 member_openid、单聊是 user_openid，
// 两者不通用，所以 /admin add 收的是 openid（或 union_openid），不是 QQ 号。
const adminUsage = `用法：
/admin list                 查看管理员
/admin add <openid> [备注]   添加 admin（仅 owner）
/admin rm <openid>           删除 admin（仅 owner，owner 本身不可删）
/admin whoami               查看自己的身份与角色

whoami 对所有人开放（只返回你自己的标识）：把 openid 发给 owner 才能被添加为 admin。`

// adminStore 把 store.DB 的管理员能力适配成 host.AdminBackend。
// 单独一层是为了不让 host 依赖 store，也不让插件摸到数据库。
type adminStore struct{ db *store.DB }

func (a adminStore) AdminRole(ctx context.Context, openid, unionID, scene, peer string) (string, string, bool, error) {
	return a.db.AdminRole(ctx, openid, unionID, scene, peer)
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

	role, scope, ok, err := c.store.AdminRole(ctx, ev.SenderID, ev.UnionID, string(ev.Scene), ev.PeerID)
	if err != nil {
		c.log.Error("查询管理员身份失败", "event", ev.ID, "err", err)
		c.replyText(ctx, ev, "权限查询失败，请稍后重试。")
		return
	}
	if !ok {
		// whoami 对所有人开放：要把某人加成 admin，owner 必须先知道对方的
		// openid，而平台不提供 QQ 号。它只回调用者自己的标识，不泄露他人。
		if sub == "whoami" {
			c.adminWhoami(ctx, ev, "", "")
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
		c.adminWhoami(ctx, ev, role, scope)
	case "add":
		c.adminAdd(ctx, ev, role, fields)
	case "rm", "remove", "del":
		c.adminRemove(ctx, ev, role, fields)
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
	fmt.Fprintf(&b, "管理员 %d 条\n", len(admins))
	for i, a := range admins {
		if i == maxLines {
			fmt.Fprintf(&b, "…另有 %d 条，未显示\n", len(admins)-maxLines)
			break
		}
		fmt.Fprintf(&b, "· %s %s", a.Role, a.OpenID)
		if a.Note != "" {
			fmt.Fprintf(&b, "（%s）", a.Note)
		}
		fmt.Fprintf(&b, "｜%s\n", describeAdmin(a.Scene, a.PeerScope))
	}
	c.replyText(ctx, ev, strings.TrimRight(b.String(), "\n"))
}

func (c *Core) adminWhoami(ctx context.Context, ev *host.Event, role, scope string) {
	var b strings.Builder
	if role == "" {
		b.WriteString("角色 非管理员\n")
	} else {
		fmt.Fprintf(&b, "角色 %s｜%s\n", role, describeAdmin(string(ev.Scene), scope))
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
	if role == "" {
		b.WriteString("\n把 openid 发给 owner，即可被添加为 admin。")
	}
	c.replyText(ctx, ev, strings.TrimRight(b.String(), "\n"))
}

// adminAdd 只允许 owner 调用，且新建的角色恒为 admin——
// /admin 不提供任何"再把别人变成 owner"的路径，避免 owner 被无限复制。
func (c *Core) adminAdd(ctx context.Context, ev *host.Event, role string, fields []string) {
	if role != store.RoleOwner {
		c.replyText(ctx, ev, "仅 owner 可以添加或删除管理员。")
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

	// 默认按"在当前场景手写 openid"处理；能解析到已知身份就用已知的
	// 场景/范围，这样按 union_openid 添加也能落到正确的那个场景。
	scene, scope := string(ev.Scene), peerScope(ev)
	openid, union := ident, ""
	if idn, ok, err := c.store.ResolveIdentity(ctx, ident, scene, scope); err != nil {
		c.log.Warn("解析待添加身份失败，按 openid 处理", "ident", ident, "err", err)
	} else if ok {
		openid, union, scene, scope = idn.OpenID, idn.UnionID, idn.Scene, idn.PeerScope
	}

	existing, err := c.store.ListAdmins(ctx)
	if err != nil {
		c.log.Error("读取管理员列表失败", "err", err)
		c.replyText(ctx, ev, "读取管理员列表失败，请查看日志。")
		return
	}
	for _, a := range existing {
		if a.Scene != scene {
			continue
		}
		if a.OpenID != openid && (union == "" || a.UnionID != union) {
			continue
		}
		// 已有授权覆盖本次范围（全局，或同一个 peer）才算重复；
		// 在 A 群加过的人可以再被加到 B 群。
		if a.PeerScope != "" && a.PeerScope != scope {
			continue
		}
		if a.Role == store.RoleOwner {
			c.replyText(ctx, ev, "该用户已是 owner，无需添加。")
			return
		}
		c.replyText(ctx, ev, fmt.Sprintf("该用户已经是 admin（%s）。", describeAdmin(a.Scene, a.PeerScope)))
		return
	}

	row := store.Admin{
		OpenID:    openid,
		UnionID:   union,
		Scene:     scene,
		PeerScope: scope,
		Role:      store.RoleAdmin,
		Note:      note,
		GrantedBy: ev.SenderID,
	}
	if err := c.store.UpsertAdmin(ctx, row); err != nil {
		c.log.Error("写入管理员失败", "openid", openid, "err", err)
		c.replyText(ctx, ev, "写入失败，请查看日志。")
		return
	}
	c.log.Info("已添加管理员", "openid", openid, "role", store.RoleAdmin,
		"scene", scene, "scope", scope, "by", ev.SenderID)

	var b strings.Builder
	fmt.Fprintf(&b, "已添加 admin：%s", openid)
	if note != "" {
		fmt.Fprintf(&b, "（%s）", note)
	}
	fmt.Fprintf(&b, "\n范围 %s｜除增删管理员外，权限与 owner 相同", describeAdmin(scene, scope))
	c.replyText(ctx, ev, b.String())
}

func (c *Core) adminRemove(ctx context.Context, ev *host.Event, role string, fields []string) {
	if role != store.RoleOwner {
		c.replyText(ctx, ev, "仅 owner 可以添加或删除管理员。")
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
		c.replyText(ctx, ev, "该用户是 owner，/admin rm 不能删除 owner。")
	default:
		c.replyText(ctx, ev, "未找到该 admin。用 /admin list 查看现有管理员。")
	}
}

// describeAdmin 把 (scene, peer_scope) 说成人话。
func describeAdmin(scene, scope string) string {
	switch {
	case scene == string(host.SceneC2C):
		return "私聊"
	case scope == "":
		return "全部群"
	default:
		return "群 " + shortID(scope)
	}
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
