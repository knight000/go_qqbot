package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/termcolor"
	"github.com/knight000/go_qqbot/pkg/host"
)

// Level 是调试轨迹的详细程度。
type Level string

const (
	// LevelFull 打印收到的每条事件、插件命中、发送结果与未命中提示。
	LevelFull Level = "full"

	// LevelPlugin 只打印触发插件的内容与真正发出的回复正文，
	// 不含事件 ID、发送者与未命中的消息。指令型 bot 多数消息无关，
	// 这个级别下控制台只留下"谁触发了我、我回了什么"。
	LevelPlugin Level = "plugin"
)

// Trace 是调试模式的控制台轨迹：一条消息从"收到"到"发出"的完整链路。
//
// 与 slog 的分工：slog 面向机器（可检索、可接 journald），Trace 面向人。
// 只在 [debug].enabled=true 或命令行 -debug 时启用；格式固定，
// 唯一的上色是时间戳（青蓝色），且只写给人看的终端：
// 重定向到文件时输出纯文本，落盘的轨迹里不会混进 ANSI 码。
// nil 的 Trace 表示关闭，所有方法可安全调用。
type Trace struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	color bool
	// unknownPayload 让未知事件附带原始 JSON（[debug].unknown_events=true）。
	unknownPayload bool
	// groupName 按 openid 查本地登记的群名。为 nil 时只显示 openid——
	// 测试里大多是这种情况，缺名字不影响轨迹的可读性。
	groupName func(ctx context.Context, openid string) string
}

// NewTrace 构造轨迹输出；w 为 nil 时返回 nil（表示关闭）。
// level 为空按 LevelFull 处理，保持旧行为。
func NewTrace(w io.Writer, level Level) *Trace {
	if w == nil {
		return nil
	}
	if level != LevelPlugin {
		level = LevelFull
	}
	return &Trace{w: w, level: level, color: termcolor.Supported(w)}
}

func (t *Trace) enabled() bool { return t != nil && t.w != nil }

// full 表示是否打印"收到/未命中"这类全量信息。
func (t *Trace) full() bool { return t.enabled() && t.level != LevelPlugin }

// emit 一次写完整块，避免多协程输出交错。
func (t *Trace) emit(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.w, s)
}

// stamp 生成行首时间戳 [HH:MM:SS]；终端里整段（含方括号）显示为青蓝色。
//
// 不走 slog 的 time 格式：轨迹是给人扫的，"秒"足够，毫秒在这个粒度上只添噪声。
func (t *Trace) stamp() string {
	s := "[" + time.Now().Format("15:04:05") + "]"
	if !t.color {
		return s
	}
	return termcolor.Wrap(termcolor.Cyan, s)
}

// 固定宽度的字段标签：冒号落在第 6 列（3 个汉字 / 6 个半角），
// 让"群""发送者""peer""事件""内容"五行竖着对齐。加新行时照此补齐空格。
const (
	lblGroup  = "群    ："  // 群(4) + 4 空格
	lblSender = "发送者："    // 发送者(6)
	lblPeer   = "peer  ：" // peer(4) + 2 空格
	lblEvent  = "事件  ："   // 事件(4) + 2 空格
	lblBody   = "内容  ："   // 内容(4) + 2 空格
	lblMedia  = "附件  ："
	lblRaw    = "原始  ："
)

// Received 打印收到的事件。放在去重之前调用，保证"收到的每一条"都能看到。
// level=plugin 时跳过：这个级别只关心触发了插件的消息。
//
// 需要 ctx 只为群聊事件查一次群名（读本地 qq_groups，几微秒）；
// 轨迹关闭时提前返回，不会给正常路径增加任何查询。
func (t *Trace) Received(ctx context.Context, ev *host.Event) {
	if !t.full() {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s 收到 %s · %s\n", t.stamp(), eventLabel(ev), ev.Name)
	// 群聊才打群名：单聊的 PeerID 就是对方的 openid，已经在"发送者"里了。
	if ev.Scene == host.SceneGroup && ev.PeerID != "" {
		b.WriteString(lblGroup + t.groupLabel(ctx, ev.PeerID) + "\n")
	}
	b.WriteString(lblSender + senderLabel(ev) + "\n")
	b.WriteString(lblPeer + dash(ev.PeerID) + "\n")
	b.WriteString(lblEvent + dash(ev.ID) + "\n")
	if ev.IsMessage() {
		b.WriteString(lblBody + contentBlock(ev.Content) + "\n")
		if ev.HasMedia {
			b.WriteString(lblMedia + "有（当前未下载）\n")
		}
	}
	t.emit(b.String())
}

// groupLabel 生成 `群名(group_openid)`。
//
// 群名取本地登记（qq_groups）：平台接口只有白名单机器人能调（11253），
// 名字多半是空的，所以名字缺失是常态而不是异常——空着，只留 openid。
// 首次收到某群消息时读到的还可能是空名（登记发生在"收到"之后），下一条就有。
func (t *Trace) groupLabel(ctx context.Context, openid string) string {
	name := ""
	if t.groupName != nil {
		name = t.groupName(ctx, openid)
	}
	return name + "(" + openid + ")"
}

// senderLabel 生成 `[Bot 角色]昵称(openid)[群内角色]`。
//
// Bot 角色是全局的（admins 表：owner / admin / user），核心会在打印"收到"
// 之前解析好；零值按 user 显示——没登记过的 openid 本来就没有角色。
//
// 括号里是发送者 openid：平台在 author.id / member_openid / user_openid
// 三处给的是同一个值，Event 已归一为 SenderID，这里直接用它。
// 昵称与群内角色都可能缺失（单聊没有群角色，且平台在单聊载荷里把
// author.username 给成空串），缺了就空着，不编造；群内角色只在群聊追加。
func senderLabel(ev *host.Event) string {
	s := "[" + ev.Actor.Role.String() + "]" + ev.Username + "(" + dash(ev.SenderID) + ")"
	// 群内角色只在群聊、且平台确实给了的时候追加：
	// 单聊没有群，通知类事件也没有 member_role，这两种都不显示这段。
	if ev.Scene == host.SceneGroup && ev.Actor.GroupRole != host.GroupRoleUnknown {
		s += "[" + string(ev.Actor.GroupRole) + "]"
	}
	return s
}

// Duplicate 打印被幂等表拦下的事件。
func (t *Trace) Duplicate(ev *host.Event) {
	if !t.full() {
		return
	}
	t.emit(fmt.Sprintf("%s 重复事件，已跳过：%s\n", t.stamp(), dash(ev.ID)))
}

// GroupDropped 打印因群准入被丢弃的事件。
//
// 只在 full 级别输出：plugin 级别只关心"谁触发了我"，被丢弃的消息没有触发任何人。
// 它也不进幂等表——同一个群反复刷屏时，这里会反复出现，属于预期。
func (t *Trace) GroupDropped(ev *host.Event, reason string) {
	if !t.full() {
		return
	}
	t.emit(fmt.Sprintf("%s 群未放行，已丢弃（%s）：%s\n", t.stamp(), reason, dash(ev.PeerID)))
}

// GroupLifecycle 打印改变本地群状态的入群/退群通知。
func (t *Trace) GroupLifecycle(action, groupID string) {
	if !t.full() {
		return
	}
	t.emit(fmt.Sprintf("%s 群生命周期：机器人%s %s\n", t.stamp(), action, dash(groupID)))
}

// NoMatch 打印没有插件认领的消息。
func (t *Trace) NoMatch(ev *host.Event) {
	if !t.full() {
		return
	}
	what := eventLabel(ev) + " · " + ev.Name
	if ev.IsMessage() {
		what = "内容 " + preview(ev.Content, 40)
	}
	t.emit(fmt.Sprintf("%s 未命中任何插件（%s），忽略\n", t.stamp(), what))
}

// Hit 打印路由命中的插件与具体命令。
//
// level=plugin 时附带触发内容，并省掉优先级：这个级别下"哪条消息触发了谁"
// 就是要看的全部信息，优先级只在排查多个插件争抢时才有意义。
func (t *Trace) Hit(m Match, ev *host.Event) {
	if !t.enabled() {
		return
	}
	cmd := "（订阅全部事件）"
	if m.Command != "" {
		cmd = fmt.Sprintf("命令 %q", m.Command)
	}
	name := m.Manifest.Name
	if m.Manifest.Version != "" {
		name += " v" + m.Manifest.Version
	}

	if t.full() {
		t.emit(fmt.Sprintf("%s 命中插件 %s · %s · 优先级 %d\n", t.stamp(), name, cmd, m.Priority))
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s 命中插件 %s · %s\n", t.stamp(), name, cmd)
	if ev != nil && ev.IsMessage() {
		b.WriteString(lblBody + contentBlock(ev.Content) + "\n")
	} else if ev != nil {
		b.WriteString(lblEvent + ev.Name + "\n")
	}
	t.emit(b.String())
}

// Denied 打印"命中但权限不足"的插件。
//
// 它不是错误：路由命中只说明命令存在，调用者级别不够而已。默认静默不回复，
// 所以这一行是排查"为什么没反应"的唯一线索。
func (t *Trace) Denied(m Match, ev *host.Event) {
	if !t.enabled() {
		return
	}
	cmd := ""
	if m.Command != "" {
		cmd = fmt.Sprintf(" · 命令 %q", m.Command)
	}
	what := "调用者 " + string(ev.Actor.Role)
	// 私聊的角色基线也是 member，但那是权限默认值，不是"群内身份"，不打印。
	if gr := ev.Actor.GroupRoleLabel(); gr != "" && ev.Scene == host.SceneGroup {
		what += "／" + gr
	}
	t.emit(fmt.Sprintf("%s 权限不足 · 插件 %s%s（需要 %s；%s）\n",
		t.stamp(), m.Manifest.Name, cmd, m.Perm.String(), what))
}

// Session 打印一次会话接管。这是"谁触发了我"在会话路径上的唯一答案：
// 消息没有命中任何命令就被某个插件拿走了，静态匹配那套轨迹（命中/未命中/
// 未让位）全都不适用。
//
// 两个级别都打（ADR-0004 决策 11）：plugin 级只回答"谁触发了我"，
// 而这条线上除了这行没有别的线索；full 级再补 peer 与剩余时间。
func (t *Trace) Session(m Match, ev *host.Event, remaining time.Duration) {
	if !t.enabled() {
		return
	}
	name := m.Manifest.Name
	if m.Manifest.Version != "" {
		name += " v" + m.Manifest.Version
	}
	if t.full() {
		t.emit(fmt.Sprintf("%s 会话接管 · 插件 %s · peer %s · 剩余 %s\n",
			t.stamp(), name, dash(ev.PeerID), humanRemain(remaining)))
		return
	}
	t.emit(fmt.Sprintf("%s 会话接管 · 插件 %s\n", t.stamp(), name))
}

// Blocked 打印"同组前序插件不让位，所以我没执行"的匹配者。
//
// 它只在 full 级别输出：plugin 级别只回答"谁触发了我"，被挡住的人没有触发任何人。
// 阻断不是错误——不让位是合法的默认选择，所以这行不进 slog；但在排查
// "zzz 声明了 yield=true 为什么没反应"时，它是唯一线索。
func (t *Trace) Blocked(blocker, skipped Match) {
	if !t.full() {
		return
	}
	name := skipped.Manifest.Name
	if skipped.Manifest.Version != "" {
		name += " v" + skipped.Manifest.Version
	}
	cmd := "（订阅全部事件）"
	if skipped.Command != "" {
		cmd = fmt.Sprintf("命令 %q", skipped.Command)
	}
	t.emit(fmt.Sprintf("%s 未执行 · 插件 %s · %s（%s 未让位）\n",
		t.stamp(), name, cmd, blocker.Manifest.Name))
}

// PluginDone 打印插件处理结果；回复正文由 Sent 统一打印，避免重复。
// level=plugin 时成功分支不打印，只保留出错，否则每个插件会有两行冗余。
func (t *Trace) PluginDone(m Match, cost time.Duration, reply *host.Reply, err error) {
	if !t.enabled() {
		return
	}
	if !t.full() && err == nil {
		return
	}
	switch {
	case err != nil:
		t.emit(fmt.Sprintf("%s 插件 %s 出错（%s）：%v\n", t.stamp(), m.Manifest.Name, costText(cost), err))
	case reply == nil:
		t.emit(fmt.Sprintf("%s 插件 %s 未回复（%s）\n", t.stamp(), m.Manifest.Name, costText(cost)))
	default:
		t.emit(fmt.Sprintf("%s 插件 %s 返回回复（%s，%d 字）\n",
			t.stamp(), m.Manifest.Name, costText(cost), len([]rune(reply.Content))))
	}
}

// CoreCommand 打印被核心独占处理的指令（/claim、/admin 等）。
func (t *Trace) CoreCommand(cmd string) {
	if !t.enabled() {
		return
	}
	if t.full() {
		t.emit(fmt.Sprintf("%s 命中核心指令 %s（不进入插件路由）\n", t.stamp(), cmd))
		return
	}
	t.emit(fmt.Sprintf("%s 核心指令 %s\n", t.stamp(), cmd))
}

// Sent 打印一次出站结果，包含实际发送的正文。
// seq 为被动回复序号，主动消息没有 seq（传 0）。
//
// level=plugin 时省略目标 openid 与 seq：这个级别只回答"回了什么"。
func (t *Trace) Sent(kind string, scene host.Scene, peer string, seq int, content string, err error, cost time.Duration) {
	if !t.enabled() {
		return
	}

	var b strings.Builder
	if t.full() {
		target := dash(peer)
		if err != nil {
			fmt.Fprintf(&b, "%s 发送失败（%s %s → %s）：%v\n", t.stamp(), kind, sceneLabel(scene), target, err)
		} else {
			fmt.Fprintf(&b, "%s 已发送（%s %s → %s）", t.stamp(), kind, sceneLabel(scene), target)
			if seq > 0 {
				fmt.Fprintf(&b, " seq=%d", seq)
			}
			fmt.Fprintf(&b, " · %s\n", costText(cost))
		}
	} else {
		if err != nil {
			fmt.Fprintf(&b, "%s %s %s 失败：%v\n", t.stamp(), kind, sceneLabel(scene), err)
		} else {
			fmt.Fprintf(&b, "%s %s %s · %s\n", t.stamp(), kind, sceneLabel(scene), costText(cost))
		}
	}
	if content != "" {
		b.WriteString(lblBody + contentBlock(content) + "\n")
	}
	t.emit(b.String())
}

// Raw 打印平台原始事件 JSON（[debug].raw=true）。只给排查字段缺失用，
// 正常调试不必打开：一条事件就可能几十行。
// level=plugin 时同样不输出——那是"收到的原始事件"，与精简级别相悖。
func (t *Trace) Raw(typ string, raw []byte) {
	if !t.full() {
		return
	}
	const limit = 16 << 10
	truncated := len(raw) > limit
	body := raw
	if truncated {
		body = raw[:limit]
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "    ", "  "); err != nil {
		buf.Reset()
		buf.Write(body)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s 原始事件 %s（%d 字节）\n%s%s", t.stamp(), typ, len(raw), lblRaw, buf.String())
	if truncated {
		fmt.Fprintf(&b, "\n…（超过 %d 字节，已截断）", limit)
	}
	b.WriteString("\n")
	t.emit(b.String())
}

// SetUnknownPayload 让未知事件附带原始 JSON。nil Trace 安全。
func (t *Trace) SetUnknownPayload(v bool) {
	if t == nil {
		return
	}
	t.unknownPayload = v
}

// SetGroupNames 注入群名查询（闭包在核心侧读 qq_groups）。
//
// 放在这里而不是让 Trace 直接依赖 store：轨迹只负责排版，
// 取不到名字就显示 openid，不该因为一次查库失败而让日志少一行。
func (t *Trace) SetGroupNames(fn func(ctx context.Context, openid string) string) {
	if t == nil {
		return
	}
	t.groupName = fn
}

// Unknown 打印解码表里没有的事件。这是"平台推了、本地不认识"的唯一出口：
// 它不进幂等表、不落库、不触发插件，所以排查"为什么没反应"时只能靠这一行。
func (t *Trace) Unknown(ev *host.Event) {
	if !t.enabled() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 未知事件 %s（解码表里没有，不路由）\n", t.stamp(), ev.Name)
	if t.unknownPayload && len(ev.Payload) > 0 {
		b.WriteString("    原始  ：" + contentBlock(string(ev.Payload)) + "\n")
	}
	t.emit(b.String())
}

// eventLabel 把大类与场景压成给人看的一行：群聊消息 / 单聊通知 / 未知事件。
func eventLabel(ev *host.Event) string {
	if ev.Kind == host.KindUnknown {
		return "未知事件"
	}
	return sceneLabel(ev.Scene) + kindLabel(ev.Kind)
}

func kindLabel(k host.Kind) string {
	switch k {
	case host.KindMessage:
		return "消息"
	case host.KindNotice:
		return "通知"
	case host.KindRequest:
		return "请求"
	case host.KindMeta:
		return "元事件"
	default:
		return string(k)
	}
}

func sceneLabel(s host.Scene) string {
	switch s {
	case host.SceneGroup:
		return "群聊"
	case host.SceneC2C:
		return "单聊"
	case host.SceneGuild:
		return "频道"
	default:
		return string(s)
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// contentBlock 让多行内容缩进对齐到正文列（标签占 8 列），
// 空内容显式标出来（而不是打印一片空白）。
func contentBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return "<空>"
	}
	return strings.ReplaceAll(truncateRunes(s, 2000), "\n", "\n        ")
}

// preview 把内容压成单行摘要，用于"未命中"这类一行输出。
func preview(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "<空>"
	}
	return "「" + truncateRunes(s, max) + "」"
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…（共 %d 字，已截断）", len(r))
}

func costText(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return d.Round(time.Millisecond).String()
	}
}
