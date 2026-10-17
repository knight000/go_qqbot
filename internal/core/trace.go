package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/host"
)

// Level 是调试轨迹的详细程度。
type Level string

const (
	// LevelFull 打印收到的每条事件、插件命中、发送结果与未命中提示。
	LevelFull Level = "full"

	// LevelPlugin 只打印触发插件的内容与真正发出的回复正文，
	// 不含事件 ID、union 与未命中的消息。指令型 bot 多数消息无关，
	// 这个级别下控制台只留下"谁触发了我、我回了什么"。
	LevelPlugin Level = "plugin"
)

// Trace 是调试模式的控制台轨迹：一条消息从"收到"到"发出"的完整链路。
//
// 与 slog 的分工：slog 面向机器（可检索、可接 journald），Trace 面向人。
// 只在 [debug].enabled=true 或命令行 -debug 时启用；格式固定、不加颜色，
// 重定向到文件也能直接读。nil 的 Trace 表示关闭，所有方法可安全调用。
type Trace struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	// unknownPayload 让未知事件附带原始 JSON（[debug].unknown_events=true）。
	unknownPayload bool
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
	return &Trace{w: w, level: level}
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

func stamp() string { return time.Now().Format("15:04:05.000") }

// Received 打印收到的事件。放在去重之前调用，保证"收到的每一条"都能看到。
// level=plugin 时跳过：这个级别只关心触发了插件的消息。
func (t *Trace) Received(ev *host.Event) {
	if !t.full() {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s] 收到 %s · %s\n", stamp(), eventLabel(ev), ev.Name)
	b.WriteString("    发送者：" + dash(ev.SenderID) + "\n")

	union := ev.UnionID
	if union == "" {
		union = "—（平台未下发）"
	}
	b.WriteString("    union ：" + union + "\n")

	// 单聊的 peer 就是发送者，重复打印没有信息量
	if ev.PeerID != "" && ev.PeerID != ev.SenderID {
		b.WriteString("    peer  ：" + ev.PeerID + "\n")
	}
	b.WriteString("    事件  ：" + dash(ev.ID) + "\n")
	if ev.MsgID != "" && ev.MsgID != ev.ID {
		b.WriteString("    msg_id：" + ev.MsgID + "\n")
	}
	if ev.IsMessage() {
		b.WriteString("    内容  ：" + contentBlock(ev.Content) + "\n")
		if ev.HasMedia {
			b.WriteString("    附件  ：有（当前未下载）\n")
		}
	}
	t.emit(b.String())
}

// Duplicate 打印被幂等表拦下的事件。
func (t *Trace) Duplicate(ev *host.Event) {
	if !t.full() {
		return
	}
	t.emit(fmt.Sprintf("[%s] 重复事件，已跳过：%s\n", stamp(), dash(ev.ID)))
}

// GroupDropped 打印因群准入被丢弃的事件。
//
// 只在 full 级别输出：plugin 级别只关心"谁触发了我"，被丢弃的消息没有触发任何人。
// 它也不进幂等表——同一个群反复刷屏时，这里会反复出现，属于预期。
func (t *Trace) GroupDropped(ev *host.Event, reason string) {
	if !t.full() {
		return
	}
	t.emit(fmt.Sprintf("[%s] 群未放行，已丢弃（%s）：%s\n", stamp(), reason, dash(ev.PeerID)))
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
	t.emit(fmt.Sprintf("[%s] 未命中任何插件（%s），忽略\n", stamp(), what))
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
		t.emit(fmt.Sprintf("[%s] 命中插件 %s · %s · 优先级 %d\n", stamp(), name, cmd, m.Priority))
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s] 命中插件 %s · %s\n", stamp(), name, cmd)
	if ev != nil && ev.IsMessage() {
		b.WriteString("    内容  ：" + contentBlock(ev.Content) + "\n")
	} else if ev != nil {
		b.WriteString("    事件  ：" + ev.Name + "\n")
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
	t.emit(fmt.Sprintf("[%s] 权限不足 · 插件 %s%s（需要 %s；%s）\n",
		stamp(), m.Manifest.Name, cmd, m.Perm.String(), what))
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
		t.emit(fmt.Sprintf("[%s] 插件 %s 出错（%s）：%v\n", stamp(), m.Manifest.Name, costText(cost), err))
	case reply == nil:
		t.emit(fmt.Sprintf("[%s] 插件 %s 未回复（%s）\n", stamp(), m.Manifest.Name, costText(cost)))
	default:
		t.emit(fmt.Sprintf("[%s] 插件 %s 返回回复（%s，%d 字）\n",
			stamp(), m.Manifest.Name, costText(cost), len([]rune(reply.Content))))
	}
}

// CoreCommand 打印被核心独占处理的指令（/claim、/admin 等）。
func (t *Trace) CoreCommand(cmd string) {
	if !t.enabled() {
		return
	}
	if t.full() {
		t.emit(fmt.Sprintf("[%s] 命中核心指令 %s（不进入插件路由）\n", stamp(), cmd))
		return
	}
	t.emit(fmt.Sprintf("[%s] 核心指令 %s\n", stamp(), cmd))
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
			fmt.Fprintf(&b, "[%s] 发送失败（%s %s → %s）：%v\n", stamp(), kind, sceneLabel(scene), target, err)
		} else {
			fmt.Fprintf(&b, "[%s] 已发送（%s %s → %s）", stamp(), kind, sceneLabel(scene), target)
			if seq > 0 {
				fmt.Fprintf(&b, " seq=%d", seq)
			}
			fmt.Fprintf(&b, " · %s\n", costText(cost))
		}
	} else {
		if err != nil {
			fmt.Fprintf(&b, "[%s] %s %s 失败：%v\n", stamp(), kind, sceneLabel(scene), err)
		} else {
			fmt.Fprintf(&b, "[%s] %s %s · %s\n", stamp(), kind, sceneLabel(scene), costText(cost))
		}
	}
	if content != "" {
		b.WriteString("    内容  ：" + contentBlock(content) + "\n")
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
	fmt.Fprintf(&b, "[%s] 原始事件 %s（%d 字节）\n    %s", stamp(), typ, len(raw), buf.String())
	if truncated {
		fmt.Fprintf(&b, "\n    …（超过 %d 字节，已截断）", limit)
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

// Unknown 打印解码表里没有的事件。这是"平台推了、本地不认识"的唯一出口：
// 它不进幂等表、不落库、不触发插件，所以排查"为什么没反应"时只能靠这一行。
func (t *Trace) Unknown(ev *host.Event) {
	if !t.enabled() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] 未知事件 %s（解码表里没有，不路由）\n", stamp(), ev.Name)
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

// contentBlock 让多行内容缩进对齐，空内容显式标出来（而不是打印一片空白）。
func contentBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return "<空>"
	}
	return strings.ReplaceAll(truncateRunes(s, 2000), "\n", "\n          ")
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
