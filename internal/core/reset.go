package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /reset 是分级重置（§8.3 / §8.4）。它不是"清空一切"的按钮，
// 而是四个影响半径不同、身份要求不同、冷却不同的操作。
//
// 三条硬约定（任何级别都遵守）：
//   - 不碰 bot_state：凭证/session/seq 是运维资产，重置不该把机器人搞成连不上；
//   - 不碰 admins：删了就永久锁在门外，而它是自举的；
//   - 先删媒体文件、再删行：反过来会留下无引用的孤儿文件。
//
// 第四条是本实现在文档之外补的：**R3 不碰 qq_groups**。群名单是访问控制
// 配置而不是业务数据，清掉等于对所有群静默拉黑——和删 admins 是同一类事故。

const (
	// resetCodeTTL 是确认码的有效期。60 秒足够复制粘贴，又不至于让人
	// 半分钟后回来还误以为有效。
	resetCodeTTL = 60 * time.Second
	// resetPhrase 是 R3 在确认码之外必须原样输入的词。
	// 确认码防误触，短语防"看都没看就按了两次"。
	resetPhrase = "确认"
)

// Reconnector 是网关的主动重连能力。
//
// 核心不 import gateway：边界在"核心只知道有人能重连"，不知道 WSS 长什么样。
// 这也是 R0 与 /reload 的分界——R0 不重启进程、不重读配置。
type Reconnector interface {
	Reconnect() error
}

// SetReconnector 注入网关。必须在网关开始投递事件之前调用：
// 赋值与读发生在不同 goroutine，靠"先赋值再起 goroutine"建立顺序。
func (c *Core) SetReconnector(r Reconnector) { c.reconnect = r }

type resetSpec struct {
	level string // 也是审计里的 level 字段
	alias string // R 编号
	name  string // 给人看的名字
	perm  host.Perm
	// target 表示这一级作用于单个会话（需要 peer）；否则是全局操作。
	target   bool
	cooldown time.Duration
	confirm  bool // 是否需要 6 位确认码
	phrase   bool // 是否还需要短语
}

// 顺序即帮助文本的顺序：从"什么都不删"到"全删"。
// 用字段名而不是位置写法：这一行里的 perm 写错就是把 R3 开给了管理员。
var resetSpecs = []resetSpec{
	{
		level: store.ResetLevelReconnect, alias: "r0", name: "重连",
		perm: host.PermBotOwner, target: false,
		cooldown: 60 * time.Second, confirm: false, phrase: false,
	},
	{
		level: store.ResetLevelSession, alias: "r1", name: "会话",
		perm: host.PermBotAdmin, target: true,
		cooldown: 10 * time.Minute, confirm: false, phrase: false,
	},
	{
		level: store.ResetLevelData, alias: "r2", name: "数据",
		perm: host.PermBotAdmin, target: true,
		cooldown: time.Hour, confirm: true, phrase: false,
	},
	{
		level: store.ResetLevelAll, alias: "r3", name: "全量",
		perm: host.PermBotOwner, target: false,
		cooldown: 24 * time.Hour, confirm: true, phrase: true,
	},
}

const resetUsage = `用法：
/reset reconnect | r0   断开并立即重连，不动数据（仅所有者，冷却 60 秒）
/reset session   | r1   清空本会话的插件状态（管理员，冷却 10 分钟）
/reset data      | r2   清空本会话的消息与媒体文件（管理员 + 确认码，冷却 1 小时）
/reset all       | r3   清空全部业务数据（仅所有者 + 确认码 + 短语，冷却 24 小时）
/reset status          查看冷却与最近记录

群聊里作用于当前群，私聊里作用于你自己；也可显式指定 /reset data <group_openid>。
任何级别都不会动管理员、群名单与网关凭证。
需要确认的级别会先回一个 6 位确认码，60 秒内回 /reset <级别> <确认码>。`

func (c *Core) handleReset(ctx context.Context, ev *host.Event) {
	fields := strings.Fields(strings.TrimSpace(ev.Content))
	sub := ""
	args := []string(nil)
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
		args = argsAfter(fields, 2)
	}

	p := c.authorize(ctx, ev)
	if !p.IsBotAdmin() {
		// 与 /admin、/group 一致：明确拒绝比静默更好排障。
		// 未放行的群根本走不到这里——闸门已经把消息丢了。
		c.replyText(ctx, ev, "该指令仅管理员可用。")
		return
	}

	switch sub {
	case "", "help":
		c.replyText(ctx, ev, resetUsage)
		return
	case "status":
		c.resetStatus(ctx, ev, p)
		return
	}

	spec, ok := lookupResetLevel(sub)
	if !ok {
		c.replyText(ctx, ev, "未知的重置级别。\n"+resetUsage)
		return
	}
	if !spec.perm.Admits(p) {
		c.replyText(ctx, ev, "该级别仅 Bot 所有者可用。")
		return
	}

	target, code, phrase := splitResetArgs(args)
	peer, errText := resolveResetTarget(ev, spec, target)
	if errText != "" {
		c.replyText(ctx, ev, errText)
		return
	}
	if !spec.confirm {
		if code != "" {
			c.replyText(ctx, ev, "该级别不需要确认码，直接 /reset "+spec.level+" 即可。")
			return
		}
		// 不需要确认码不等于没有闸门：冷却同样约束 R0/R1，
		// 否则连续 /reset reconnect 可以一路把 session 配额烧光。
		if remain := c.resetCooldown(ctx, spec, peer); remain > 0 {
			c.replyText(ctx, ev, fmt.Sprintf("该级别冷却中，还需等待 %s。", humanRemain(remain)))
			return
		}
		c.resetExecute(ctx, ev, spec, peer, 0)
		return
	}

	// 第一次调用：只预览 + 发码，绝不执行。
	if code == "" {
		c.resetBegin(ctx, ev, spec, peer)
		return
	}
	c.resetConfirm(ctx, ev, spec, peer, code, phrase)
}

// resolveResetTarget 决定"清谁"。
//
// 全局级别（R0/R3）用 '*' 作为作用域标记；会话级别优先用显式 openid，
// 否则退化为当前会话——群聊是当前群，私聊是发言者自己。
func resolveResetTarget(ev *host.Event, spec resetSpec, target string) (string, string) {
	if !spec.target {
		if target != "" {
			return "", "该级别是全局操作，不接受目标 openid。"
		}
		return store.ResetScopeGlobal, ""
	}
	if target != "" {
		return target, ""
	}
	if ev.PeerID == "" {
		return "", "无法确定要清理的会话，请显式给出 openid。"
	}
	return ev.PeerID, ""
}

// resetBegin 生成确认码并回预览。任何一步失败都不执行。
func (c *Core) resetBegin(ctx context.Context, ev *host.Event, spec resetSpec, peer string) {
	if remain := c.resetCooldown(ctx, spec, peer); remain > 0 {
		c.replyText(ctx, ev, fmt.Sprintf("该级别冷却中，还需等待 %s。", humanRemain(remain)))
		return
	}

	preview, err := c.resetPreview(ctx, spec, peer)
	if err != nil {
		c.log.Error("统计待清理数据失败", "level", spec.level, "peer", peer, "err", err)
		c.replyText(ctx, ev, "统计待清理数据失败，请查看日志。")
		return
	}

	code, err := newResetCode()
	if err != nil {
		c.log.Error("生成确认码失败", "err", err)
		c.replyText(ctx, ev, "生成确认码失败，请查看日志。")
		return
	}
	if _, err := c.store.OpenResetPending(ctx, ev.SenderID, peer, spec.level, code, resetCodeTTL); err != nil {
		c.log.Error("写入待确认重置失败", "err", err)
		c.replyText(ctx, ev, "写入审计失败，已中止。")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s，不可恢复。\n", preview)
	b.WriteString("保留：管理员、群名单、网关凭证、任务定义。\n")
	fmt.Fprintf(&b, "%d 秒内回复 /reset %s %s", int(resetCodeTTL.Seconds()), spec.level, code)
	if spec.phrase {
		fmt.Fprintf(&b, " %s", resetPhrase)
	}
	b.WriteString(" 确认。")
	c.replyText(ctx, ev, b.String())
}

// resetConfirm 校验确认码并执行。
//
// 确认码绑定 (actor, peer, level)：换个会话用不了，别人也代答不了。
// 这三者里任何一个不同，PendingReset 都查不到记录。
func (c *Core) resetConfirm(ctx context.Context, ev *host.Event, spec resetSpec, peer, code, phrase string) {
	rec, ok, err := c.store.PendingReset(ctx, ev.SenderID, peer, spec.level)
	if err != nil {
		c.log.Error("读取待确认重置失败", "err", err)
		c.replyText(ctx, ev, "读取审计失败，请查看日志。")
		return
	}
	if !ok || time.Now().After(rec.ExpiresAt) {
		c.replyText(ctx, ev, "没有待确认的请求，或确认码已过期。请重新发送 /reset "+spec.level+"。")
		return
	}
	if subtle.ConstantTimeCompare([]byte(rec.Code), []byte(code)) != 1 {
		c.replyText(ctx, ev, "确认码不正确。")
		return
	}
	// 短语校验放在确认码之后：先证明"你有这个码"，再要求"你想清楚了"。
	if spec.phrase && phrase != resetPhrase {
		c.replyText(ctx, ev, fmt.Sprintf("确认码正确，还需要在末尾原样输入「%s」：\n/reset %s %s %s",
			resetPhrase, spec.level, code, resetPhrase))
		return
	}
	if remain := c.resetCooldown(ctx, spec, peer); remain > 0 {
		_ = c.store.CloseReset(ctx, rec.ID, store.ResetStatusCancelled, "冷却中")
		c.replyText(ctx, ev, fmt.Sprintf("该级别冷却中，还需等待 %s。", humanRemain(remain)))
		return
	}
	c.resetExecute(ctx, ev, spec, peer, rec.ID)
}

// resetExecute 真正动手。auditID > 0 表示这次执行有对应的 pending 记录。
//
// 成功与失败写的是两种状态：failed 只进审计、不参与冷却（LastResetDoneAt
// 只认 done）。否则 R3 失败一次就锁 24 小时，而用户看到的还是"重置失败"。
func (c *Core) resetExecute(ctx context.Context, ev *host.Event, spec resetSpec, peer string, auditID int64) {
	summary, err := c.runReset(ctx, spec, peer)

	status := store.ResetStatusDone
	detail := summary
	if err != nil {
		status = store.ResetStatusFailed
		detail = "失败: " + err.Error()
	}

	if auditID > 0 {
		if err := c.store.CloseReset(ctx, auditID, status, detail); err != nil {
			c.log.Warn("更新重置审计失败", "status", status, "err", err)
		}
	} else if err := c.store.AuditResetFinished(ctx, ev.SenderID, peer, spec.level, status, detail); err != nil {
		c.log.Warn("写入重置审计失败", "status", status, "err", err)
	}

	if err != nil {
		c.log.Error("执行重置失败", "level", spec.level, "peer", peer, "err", err)
		c.replyText(ctx, ev, "重置失败，已中止："+err.Error()+"\n本次不计入冷却，可直接重试。")
		return
	}

	c.log.Warn("执行了重置", "level", spec.level, "peer", peer, "by", ev.SenderID, "detail", summary)

	msg := resetResultText(spec, summary)
	if remain := spec.cooldown; remain > 0 {
		msg += fmt.Sprintf("\n同类重置的冷却到 %s。", time.Now().Add(remain).Format("15:04:05"))
	}
	c.replyText(ctx, ev, msg)
}

// runReset 分派到具体级别，返回写进审计的摘要。
func (c *Core) runReset(ctx context.Context, spec resetSpec, peer string) (string, error) {
	switch spec.level {
	case store.ResetLevelReconnect:
		if c.reconnect == nil {
			return "", errors.New("网关未就绪")
		}
		if err := c.reconnect.Reconnect(); err != nil {
			return "", err
		}
		return "已断开，等待 Resume", nil

	case store.ResetLevelSession:
		counts, err := c.store.ClearPeerSession(ctx, peer)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("会话状态 %d 条、会话接管 %d 个", counts.KVRows, counts.Sessions), nil

	case store.ResetLevelData:
		counts, files, err := c.clearPeerData(ctx, peer)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("消息 %d 条、媒体 %d 个（文件 %d）、任务记录 %d 条、会话接管 %d 个",
			counts.Messages, counts.MediaRows, files, counts.JobRuns, counts.Sessions), nil

	case store.ResetLevelAll:
		counts, files, err := c.clearAllData(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("消息 %d 条、媒体 %d 个（文件 %d）、插件会话 %d 条、任务记录 %d 条、去重记录 %d 条、身份映射 %d 条、会话接管 %d 个",
			counts.Messages, counts.MediaRows, files, counts.KVRows, counts.JobRuns, counts.Events, counts.Identities, counts.Sessions), nil
	}
	return "", fmt.Errorf("未知级别 %q", spec.level)
}

// clearPeerData 是 R2：先取文件路径 → 删文件 → 删行。
func (c *Core) clearPeerData(ctx context.Context, peer string) (store.ResetCounts, int, error) {
	paths, err := c.store.PeerMediaPaths(ctx, peer)
	if err != nil {
		return store.ResetCounts{}, 0, err
	}
	files := c.removeMediaFiles(paths)
	counts, err := c.store.ClearPeerData(ctx, peer)
	if err != nil {
		return counts, files, err
	}
	return counts, files, nil
}

func (c *Core) clearAllData(ctx context.Context) (store.ResetCounts, int, error) {
	paths, err := c.store.AllMediaPaths(ctx)
	if err != nil {
		return store.ResetCounts{}, 0, err
	}
	files := c.removeMediaFiles(paths)
	counts, err := c.store.ClearAllData(ctx)
	if err != nil {
		return counts, files, err
	}
	return counts, files, nil
}

// removeMediaFiles 返回成功删除的文件数。
//
// 删除失败不中止重置：行已经注定要删，留着孤儿文件顶多占空间，
// 而中止会留下"消息删了一半"的更糟状态。失败只进日志。
//
// 配了 [media].dir 时只删该目录内的路径——库里的 path 若被写坏，
// 不该变成一次任意文件删除。
func (c *Core) removeMediaFiles(paths []string) int {
	root := ""
	if c.cfg != nil && strings.TrimSpace(c.cfg.Media.Dir) != "" {
		if abs, err := filepath.Abs(c.cfg.Media.Dir); err == nil {
			root = filepath.Clean(abs)
		}
	}

	removed := 0
	for _, p := range paths {
		clean := filepath.Clean(p)
		if root != "" && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			c.log.Warn("媒体路径不在 [media].dir 内，跳过删除", "path", p, "dir", root)
			continue
		}
		if err := os.Remove(clean); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				c.log.Warn("删除媒体文件失败", "path", clean, "err", err)
			}
			continue
		}
		removed++
	}
	return removed
}

// resetPreview 生成"将清空什么"的说明。数字取自库，不猜。
//
// 措辞特意用"当前"：从发码到确认有 60 秒，这期间新到的消息也会被清掉，
// 所以预览是快照、结果才是实删。写死成"将清空 128 条"等于给一个必然对不上的承诺。
func (c *Core) resetPreview(ctx context.Context, spec resetSpec, peer string) (string, error) {
	switch spec.level {
	case store.ResetLevelData:
		counts, err := c.store.PeerStats(ctx, peer)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("将清空该会话的消息、媒体文件与任务记录：当前消息 %d 条、媒体 %d 个、任务记录 %d 条、会话接管 %d 个",
			counts.Messages, counts.MediaRows, counts.JobRuns, counts.Sessions), nil
	case store.ResetLevelAll:
		counts, err := c.store.DataStats(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("将清空全部业务数据：当前消息 %d 条、媒体 %d 个、插件会话 %d 条、任务记录 %d 条、去重记录 %d 条、身份映射 %d 条、会话接管 %d 个",
			counts.Messages, counts.MediaRows, counts.KVRows, counts.JobRuns, counts.Events, counts.Identities, counts.Sessions), nil
	}
	return "将执行 " + spec.name + " 重置", nil
}

// resetCooldown 返回剩余冷却时间；0 表示可以执行。
func (c *Core) resetCooldown(ctx context.Context, spec resetSpec, peer string) time.Duration {
	last, ok, err := c.store.LastResetDoneAt(ctx, spec.level, peer)
	if err != nil {
		// 读不到冷却就不放行：宁可多等一次，也不能让防连击的闸门失效。
		c.log.Error("读取重置冷却失败，按冷却中处理", "err", err)
		return spec.cooldown
	}
	if !ok {
		return 0
	}
	if remain := time.Until(last.Add(spec.cooldown)); remain > 0 {
		return remain
	}
	return 0
}

func (c *Core) resetStatus(ctx context.Context, ev *host.Event, p host.Principal) {
	peer := ev.PeerID
	if peer == "" {
		peer = store.ResetScopeGlobal
	}

	var b strings.Builder
	b.WriteString("重置状态")
	for _, spec := range resetSpecs {
		scope := peer
		if !spec.target {
			scope = store.ResetScopeGlobal
		}
		remain := c.resetCooldown(ctx, spec, scope)
		fmt.Fprintf(&b, "\n%s %s（%s）", strings.ToUpper(spec.alias), spec.name, spec.level)
		if remain > 0 {
			fmt.Fprintf(&b, " 冷却 %s", humanRemain(remain))
		} else {
			b.WriteString(" 可执行")
		}
		if !spec.perm.Admits(p) {
			b.WriteString("｜无权限")
		}
	}

	recs, err := c.store.RecentResets(ctx, 5)
	if err != nil {
		c.log.Warn("读取重置审计失败", "err", err)
	} else if len(recs) > 0 {
		b.WriteString("\n最近：")
		for _, r := range recs {
			fmt.Fprintf(&b, "\n· %s %s %s", r.CreatedAt.Format("01-02 15:04"),
				strings.ToUpper(r.Level), resetStatusLabel(r.Status))
			if r.Detail != "" {
				b.WriteString(" " + clipRunes(r.Detail, 40))
			}
		}
	}
	c.replyText(ctx, ev, b.String())
}

func resetStatusLabel(s string) string {
	switch s {
	case store.ResetStatusDone:
		return "已执行"
	case store.ResetStatusPending:
		return "待确认"
	case store.ResetStatusFailed:
		return "失败（未占冷却）"
	case store.ResetStatusCancelled:
		return "已取消"
	case store.ResetStatusExpired:
		return "已过期"
	}
	return s
}

func resetResultText(spec resetSpec, summary string) string {
	switch spec.level {
	case store.ResetLevelReconnect:
		return "已请求网关立即重连：session 保留，重连走 Resume，事件不会丢。\n" +
			"若 30 秒内没有任何「网关就绪」日志，用 /reload 复核进程。"
	case store.ResetLevelSession:
		return "已清空本会话的插件状态：" + summary + "。\n历史消息仍保留；要删消息用 /reset data。"
	case store.ResetLevelData:
		return "已清空本会话数据：" + summary + "。\n/group 名单与管理员不受影响。"
	}
	return "已清空全部业务数据：" + summary + "。\n管理员、群名单、网关凭证、任务定义均已保留。"
}

// lookupResetLevel 接受级别名与 R 编号两种写法。
// 两种都认是刻意的：文档里两种写法并存，硬性二选一会让另一半文档变成错的。
func lookupResetLevel(s string) (resetSpec, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, spec := range resetSpecs {
		if s == spec.level || s == spec.alias {
			return spec, true
		}
	}
	return resetSpec{}, false
}

// splitResetArgs 从参数里挑出目标 openid、确认码与短语，顺序无关。
//
// 不按位置解析的原因是三者形态天然可分：openid 是 24+ 位十六进制、
// 确认码是 6 位数字、短语是固定词。按位置反而要求用户记住顺序。
func splitResetArgs(args []string) (target, code, phrase string) {
	for _, a := range args {
		switch {
		case code == "" && isResetCode(a):
			code = a
		case target == "" && looksLikeOpenID(a):
			target = a
		case phrase == "":
			phrase = a
		}
	}
	return target, code, phrase
}

// argsAfter 等价于 fields[n:] 但不会越界。
//
// 不能直接写 fields[2:]：strings.Fields 返回切片的**容量**是内部估算的，
// 对 "/reset" 这种只有一个字段的输入，cap 可能等于 1，切片表达式就会 panic。
// 这条路径由普通用户的一条消息直接触发，不能靠运气。
func argsAfter(fields []string, n int) []string {
	if len(fields) <= n {
		return nil
	}
	return fields[n:]
}

func isResetCode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// newResetCode 用 crypto/rand 生成 6 位数字。
// 不用 math/rand：这个码是"替别人按确认键"的唯一凭证，可预测就等于没有。
func newResetCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// humanRemain 把剩余时间说成人话：秒级给秒，分钟级给分秒，小时级给时分。
func humanRemain(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()+0.5))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%d 分钟", m)
		}
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%d 小时", h)
	}
	return fmt.Sprintf("%d 小时 %d 分", h, m)
}
