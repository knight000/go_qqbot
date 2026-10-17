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

// Trace 是调试模式的控制台轨迹：一条消息从"收到"到"发出"的完整链路。
//
// 与 slog 的分工：slog 面向机器（可检索、可接 journald），Trace 面向人。
// 只在 [debug].enabled=true 或命令行 -debug 时启用；格式固定、不加颜色，
// 重定向到文件也能直接读。nil 的 Trace 表示关闭，所有方法可安全调用。
type Trace struct {
	mu sync.Mutex
	w  io.Writer
}

// NewTrace 构造轨迹输出；w 为 nil 时返回 nil（表示关闭）。
func NewTrace(w io.Writer) *Trace {
	if w == nil {
		return nil
	}
	return &Trace{w: w}
}

func (t *Trace) enabled() bool { return t != nil && t.w != nil }

// emit 一次写完整块，避免多协程输出交错。
func (t *Trace) emit(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.w, s)
}

func stamp() string { return time.Now().Format("15:04:05.000") }

// Received 打印收到的事件。放在去重之前调用，保证"收到的每一条"都能看到。
func (t *Trace) Received(ev *host.Event) {
	if !t.enabled() {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s] 收到 %s · %s\n", stamp(), sceneLabel(ev.Scene), ev.Type)
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
	if !t.enabled() {
		return
	}
	t.emit(fmt.Sprintf("[%s] 重复事件，已跳过：%s\n", stamp(), dash(ev.ID)))
}

// NoMatch 打印没有插件认领的消息。
func (t *Trace) NoMatch(ev *host.Event) {
	if !t.enabled() {
		return
	}
	what := ev.Type
	if ev.IsMessage() {
		what = "内容 " + preview(ev.Content, 40)
	}
	t.emit(fmt.Sprintf("[%s] 未命中任何插件（%s），忽略\n", stamp(), what))
}

// Hit 打印路由命中的插件与具体命令。
func (t *Trace) Hit(m Match) {
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
	t.emit(fmt.Sprintf("[%s] 命中插件 %s · %s · 优先级 %d\n", stamp(), name, cmd, m.Priority))
}

// PluginDone 打印插件处理结果；回复正文由 Sent 统一打印，避免重复。
func (t *Trace) PluginDone(m Match, cost time.Duration, reply *host.Reply, err error) {
	if !t.enabled() {
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
	t.emit(fmt.Sprintf("[%s] 命中核心指令 %s（不进入插件路由）\n", stamp(), cmd))
}

// Sent 打印一次出站结果，包含实际发送的正文。
// seq 为被动回复序号，主动消息没有 seq（传 0）。
func (t *Trace) Sent(kind string, scene host.Scene, peer string, seq int, content string, err error, cost time.Duration) {
	if !t.enabled() {
		return
	}

	var b strings.Builder
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
	if content != "" {
		b.WriteString("    内容  ：" + contentBlock(content) + "\n")
	}
	t.emit(b.String())
}

// Raw 打印平台原始事件 JSON（[debug].raw=true）。只给排查字段缺失用，
// 正常调试不必打开：一条事件就可能几十行。
func (t *Trace) Raw(typ string, raw []byte) {
	if !t.enabled() {
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
