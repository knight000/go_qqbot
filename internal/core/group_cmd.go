package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// /group 是群准入的管理入口，和 /admin 一样属于核心保留指令：
// 它决定"哪些群能跟机器人说话"，不能让插件接管。
//
// 权限只有一档：Bot 管理员及以上。群主 / 群管理员是平台身份，
// 不是 Bot 管理员——否则把机器人拉进任意群的人都能给自己开白名单。
const groupUsage = `用法：
/group list [allow|deny|all]   已记录的群（openid ↔ 名称）
/group status [openid]         准入状态；群聊省略 openid 为当前群
/group allow [openid]          加入白名单
/group deny [openid]           加入黑名单
/group clear [openid]          移出黑白名单
/group name [openid] <名称>     更新群名称

群聊里省略 openid 表示当前群；按 id 操作时写完整 32 位 openid。
白名单开启时只有白名单群可用；黑名单优先于白名单。
仅 Bot 管理员与所有者可用。`

func (c *Core) handleGroup(ctx context.Context, ev *host.Event) {
	fields := strings.Fields(strings.TrimSpace(ev.Content))
	sub := ""
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}
	args := fields[2:]

	if !c.authorize(ctx, ev).IsBotAdmin() {
		// 与 /admin 保持一致：明确拒绝比静默更好排障。
		// 未授权群里的普通用户根本走不到这里——闸门已经把消息丢了。
		c.replyText(ctx, ev, "该指令仅管理员可用。")
		return
	}

	switch sub {
	case "", "help":
		c.replyText(ctx, ev, groupUsage)
	case "list":
		c.groupList(ctx, ev, args)
	case "status":
		c.groupStatus(ctx, ev, args)
	case "allow":
		c.groupSetMode(ctx, ev, args, store.GroupModeAllow)
	case "deny":
		c.groupSetMode(ctx, ev, args, store.GroupModeDeny)
	case "clear", "reset":
		c.groupClear(ctx, ev, args)
	case "name", "rename":
		c.groupRename(ctx, ev, args)
	default:
		c.replyText(ctx, ev, "未知子命令。\n"+groupUsage)
	}
}

// resolveGroup 解析"要操作哪个群"。
//
// 规则：首个参数是完整 openid 就按 id 处理；否则群聊里退化为当前群，
// 私聊里报错。这样 /group allow 在群里可以直接用，而按 id 操作
// （通常在私聊）也不会被误当成当前群。
func (c *Core) resolveGroup(ev *host.Event, args []string) (id string, rest []string, errText string) {
	if len(args) > 0 && looksLikeOpenID(args[0]) {
		return args[0], args[1:], ""
	}
	if ev.Scene == host.SceneGroup && ev.PeerID != "" {
		return ev.PeerID, args, ""
	}
	return "", args, "请提供群 openid（32 位十六进制），或在群聊里省略以操作当前群。"
}

// looksLikeOpenID 只做形态判断：平台的群 openid 是 32 位十六进制。
// 用长度 + 字符集而不是"是否在库里"，是为了让管理员能给尚未登记的群建档。
func looksLikeOpenID(s string) bool {
	if len(s) < 24 {
		return false
	}
	for _, ch := range s {
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'a' && ch <= 'f':
		case ch >= 'A' && ch <= 'F':
		default:
			return false
		}
	}
	return true
}

func (c *Core) groupSetMode(ctx context.Context, ev *host.Event, args []string, mode string) {
	id, rest, errText := c.resolveGroup(ev, args)
	if errText != "" {
		c.replyText(ctx, ev, errText)
		return
	}
	if len(rest) > 0 {
		c.replyText(ctx, ev, "用法：/group "+mode+" [openid]")
		return
	}

	before, existed, err := c.store.GetGroup(ctx, id)
	if err != nil {
		c.log.Error("读取群失败", "group", id, "err", err)
		c.replyText(ctx, ev, "读取群信息失败，请查看日志。")
		return
	}
	if err := c.store.SetGroupMode(ctx, id, mode); err != nil {
		c.log.Error("设置群名单失败", "group", id, "mode", mode, "err", err)
		c.replyText(ctx, ev, "写入失败，请查看日志。")
		return
	}
	name := before.Name
	if !existed {
		// 未登记的群也会建档，名称稍后由 /group name 或自动抓取补上。
		if g, ok, _ := c.store.GetGroup(ctx, id); ok {
			name = g.Name
		}
	}
	c.log.Info("已更新群名单", "group", id, "mode", mode, "by", ev.SenderID)

	var b strings.Builder
	fmt.Fprintf(&b, "已将群 %s", id)
	if name != "" {
		fmt.Fprintf(&b, "（%s）", name)
	}
	fmt.Fprintf(&b, " 加入%s。", modeLabel(mode))
	if existed && before.Mode != store.GroupModeNone && before.Mode != mode {
		fmt.Fprintf(&b, "\n已从%s移出。", modeLabel(before.Mode))
	}
	switch {
	case mode == store.GroupModeDeny && !c.blacklistEnabled():
		b.WriteString("\n注意：黑名单开关当前关闭，这个设置暂不生效。")
	case mode == store.GroupModeDeny:
		b.WriteString("\n该群消息将被直接丢弃。")
	case mode == store.GroupModeAllow && !c.whitelistEnabled():
		b.WriteString("\n注意：白名单开关当前关闭，这个设置暂不生效。")
	}
	c.replyText(ctx, ev, b.String())
}

func (c *Core) groupClear(ctx context.Context, ev *host.Event, args []string) {
	id, rest, errText := c.resolveGroup(ev, args)
	if errText != "" {
		c.replyText(ctx, ev, errText)
		return
	}
	if len(rest) > 0 {
		c.replyText(ctx, ev, "用法：/group clear [openid]")
		return
	}

	g, ok, err := c.store.GetGroup(ctx, id)
	if err != nil {
		c.log.Error("读取群失败", "group", id, "err", err)
		c.replyText(ctx, ev, "读取群信息失败，请查看日志。")
		return
	}
	if !ok {
		c.replyText(ctx, ev, "未找到该群。用 /group list 查看已登记的群。")
		return
	}
	if g.Mode == store.GroupModeNone {
		c.replyText(ctx, ev, "该群本来就不在黑白名单里。")
		return
	}
	if err := c.store.SetGroupMode(ctx, id, store.GroupModeNone); err != nil {
		c.log.Error("清除群名单失败", "group", id, "err", err)
		c.replyText(ctx, ev, "写入失败，请查看日志。")
		return
	}
	c.log.Info("已清除群名单", "group", id, "was", g.Mode, "by", ev.SenderID)

	msg := fmt.Sprintf("已将群 %s", id)
	if g.Name != "" {
		msg += "（" + g.Name + "）"
	}
	msg += fmt.Sprintf(" 移出%s。", modeLabel(g.Mode))
	if c.whitelistEnabled() {
		msg += "\n白名单开启中，该群现在会被丢弃。"
	}
	c.replyText(ctx, ev, msg)
}

func (c *Core) groupRename(ctx context.Context, ev *host.Event, args []string) {
	id, rest, errText := c.resolveGroup(ev, args)
	if errText != "" {
		c.replyText(ctx, ev, errText)
		return
	}
	name := strings.TrimSpace(strings.Join(rest, " "))
	if name == "" {
		c.replyText(ctx, ev, "用法：/group name [openid] <名称>")
		return
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}

	_, existed, err := c.store.GetGroup(ctx, id)
	if err != nil {
		c.log.Error("读取群失败", "group", id, "err", err)
		c.replyText(ctx, ev, "读取群信息失败，请查看日志。")
		return
	}
	if err := c.store.SetGroupName(ctx, id, name, "manual"); err != nil {
		c.log.Error("更新群名称失败", "group", id, "err", err)
		c.replyText(ctx, ev, "写入失败，请查看日志。")
		return
	}
	c.log.Info("已更新群名称", "group", id, "name", name, "by", ev.SenderID)

	msg := fmt.Sprintf("已更新群名称：%s（%s）", name, id)
	if !existed {
		msg += "\n该群此前未登记，已建档。"
	}
	c.replyText(ctx, ev, msg)
}

func (c *Core) groupStatus(ctx context.Context, ev *host.Event, args []string) {
	// 私聊里不带参数 = 总览；群聊里不带参数 = 当前群。
	if len(args) == 0 && ev.Scene != host.SceneGroup {
		c.groupOverview(ctx, ev)
		return
	}

	id, rest, errText := c.resolveGroup(ev, args)
	if errText != "" {
		c.replyText(ctx, ev, errText)
		return
	}
	if len(rest) > 0 {
		c.replyText(ctx, ev, "用法：/group status [openid]")
		return
	}

	g, ok, err := c.store.GetGroup(ctx, id)
	if err != nil {
		c.log.Error("读取群失败", "group", id, "err", err)
		c.replyText(ctx, ev, "读取群信息失败，请查看日志。")
		return
	}
	if !ok {
		c.replyText(ctx, ev, "未记录该群。机器人收到该群消息后会自动登记。")
		return
	}

	allowed, reason := c.evaluateGroup(g)
	var b strings.Builder
	fmt.Fprintf(&b, "群 %s\n", g.OpenID)
	if g.Name != "" {
		fmt.Fprintf(&b, "名称 %s", g.Name)
		if g.NameSource == "manual" {
			b.WriteString("（手工设置）")
		}
	} else {
		b.WriteString("名称 （未命名，可用 /group name 设置）")
	}
	fmt.Fprintf(&b, "\n名单 %s\n", modeLabel(g.Mode))
	if allowed {
		b.WriteString("准入 放行")
	} else {
		fmt.Fprintf(&b, "准入 丢弃（%s）", reason)
	}
	fmt.Fprintf(&b, "\n开关 白名单 %s｜黑名单 %s\n",
		onOff(c.whitelistEnabled()), onOff(c.blacklistEnabled()))
	fmt.Fprintf(&b, "活跃 最近 %s｜首次 %s",
		g.LastSeenAt.Format("01-02 15:04"), g.FirstSeenAt.Format("01-02 15:04"))
	c.replyText(ctx, ev, b.String())
}

func (c *Core) groupOverview(ctx context.Context, ev *host.Event) {
	total, allowed, denied, err := c.store.CountGroups(ctx)
	if err != nil {
		c.log.Error("统计群失败", "err", err)
		c.replyText(ctx, ev, "读取群统计失败，请查看日志。")
		return
	}
	msg := fmt.Sprintf("群准入\n白名单 %s｜黑名单 %s\n已记录 %d 个群｜白名单 %d｜黑名单 %d\n用 /group list 查看 openid 与名称。",
		onOff(c.whitelistEnabled()), onOff(c.blacklistEnabled()), total, allowed, denied)
	c.replyText(ctx, ev, msg)
}

func (c *Core) groupList(ctx context.Context, ev *host.Event, args []string) {
	filter := "all"
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
		switch filter {
		case "allow", "deny", "all":
		default:
			c.replyText(ctx, ev, "用法：/group list [allow|deny|all]")
			return
		}
	}

	groups, err := c.store.ListGroups(ctx)
	if err != nil {
		c.log.Error("列出群失败", "err", err)
		c.replyText(ctx, ev, "读取群列表失败，请查看日志。")
		return
	}
	if len(groups) == 0 {
		c.replyText(ctx, ev, "还没有记录任何群。把机器人拉进群并在群里说一句话即可登记。")
		return
	}

	var allowed, denied int
	var shown []store.Group
	for _, g := range groups {
		switch g.Mode {
		case store.GroupModeAllow:
			allowed++
		case store.GroupModeDeny:
			denied++
		}
		if filter == "all" || g.Mode == filter {
			shown = append(shown, g)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "群 %d 个｜白名单 %d｜黑名单 %d", len(groups), allowed, denied)
	if filter != "all" {
		fmt.Fprintf(&b, "（只显示%s）", modeLabel(filter))
	}
	if len(shown) == 0 {
		fmt.Fprintf(&b, "\n没有%s的群。", modeLabel(filter))
		c.replyText(ctx, ev, b.String())
		return
	}

	const maxLines = 20
	for i, g := range shown {
		if i == maxLines {
			fmt.Fprintf(&b, "\n…另有 %d 个未显示", len(shown)-maxLines)
			break
		}
		fmt.Fprintf(&b, "\n· %s %s", modeTag(g.Mode), g.OpenID)
		if g.Name != "" {
			b.WriteString(" " + clipRunes(g.Name, 20))
		}
		fmt.Fprintf(&b, "｜活跃 %s", g.LastSeenAt.Format("01-02 15:04"))
	}
	c.replyText(ctx, ev, b.String())
}

// modeLabel 是名单的中文说法；未设置时说明后果而不是留空。
func modeLabel(mode string) string {
	switch mode {
	case store.GroupModeAllow:
		return "白名单"
	case store.GroupModeDeny:
		return "黑名单"
	default:
		return "未设置"
	}
}

func modeTag(mode string) string {
	switch mode {
	case store.GroupModeAllow:
		return "[白名单]"
	case store.GroupModeDeny:
		return "[黑名单]"
	default:
		return "[未设置]"
	}
}

func onOff(v bool) string {
	if v {
		return "开"
	}
	return "关"
}

// clipRunes 截断到 n 个字符，用于列表里的群名。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
