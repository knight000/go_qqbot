// Package md 把一段 markdown 源码画成图片回复，是最小的「渲染能力」示例插件。
//
// 指令：`/md <内容>`。只认带斜杠的写法，理由是两条：
//   - 与核心保留指令（/claim /reset /group …）同一形状，用户不必记两套约定；
//   - `md` 在中文聊天里是常见的脏话缩写，宽松匹配会在线上的正常对话里误触。
//
// 两条实现约束值得复制到别的渲染类插件里：
//  1. 渲染必须在 Handle 内同步完成。Sender.Reply 的错误在 dispatch 里才处理，
//     那时 Handle 已经返回，插件拿不到"渲染失败"，所以"发送时再渲染"这条路不通。
//  2. 渲染成功就不要再带文本。Reply 同时有 Content 与 Image 时核心会拆成两条消息，
//     消耗两格被动配额（群 5 次/5 分钟），纯属浪费。
//
// 平台事实（2026-10-19 实测）：群消息里的换行到不了插件，平台/客户端在送达前
// 就把 `\n` 压成了空格。同一段内容两种形态的出图高度是 466 px（换行健在）对
// 166 px（压成一行），后者整段被当成一个 ATX 标题。这不是渲染器的毛病，
// 也无法在插件里还原——信息在到达之前就没了。所以在单行里写 `\n`，
// 由 unescape 还原成真换行。
package md

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/knight000/go_qqbot/internal/host"
)

func init() { host.Register(&Plugin{}) }

const usage = `用法：/md <markdown>，多行用 \n 分隔；例：/md # 标题\n- 一\n- 二`

type Plugin struct {
	host host.Host
	log  *slog.Logger
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "md",
		Version:      "0.2.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.reply", "render", "log"},
		Intents:      []string{"message"},
		Commands: []host.Command{{
			Prefix:      "/md",
			Priority:    100,
			Description: "把 markdown 画成图片",
		}},
	}
}

func (p *Plugin) Init(_ context.Context, h host.Host) error {
	p.host = h
	// 宿主已给插件的 logger 打上 plugin=<name>，这里不再 With，否则每条日志重复一次。
	p.log = h.Log()
	p.log.Info("md 插件已初始化")
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	src, ok := parse(ev.Content)
	if !ok {
		return nil, nil // 不是本指令，让给优先级更低的插件
	}
	if src == "" {
		return &host.Reply{Content: usage}, nil
	}
	src = unescape(src)
	// 平台会吃掉换行，只靠出图高度很难判断"是没换行还是没渲染"。
	// 这行是排查这类问题的唯一凭据，别删。
	p.log.Debug("渲染请求", "bytes", len(src), "lines", strings.Count(src, "\n")+1)

	img, err := p.host.Renderer().Markdown(ctx, src)
	switch {
	case err == nil && img.Valid():
		// MsgType=7 是图片消息；Content 留空，避免核心拆成两条。
		return &host.Reply{MsgType: 7, Image: img}, nil

	case err == nil:
		// 契约上不该发生：渲染器说成功却没给图。报错而不是静默，否则表现为"机器人不理人"。
		p.log.Warn("渲染器返回成功但没有图片")
		return &host.Reply{Content: "渲染失败：宿主未返回图片"}, nil

	case errors.Is(err, host.ErrRenderTooLarge):
		return &host.Reply{Content: "内容太长，画出来超过高度上限，请分段发送"}, nil

	case errors.Is(err, host.ErrRenderTimeout):
		return &host.Reply{Content: "渲染超时，请稍后再试"}, nil

	default:
		// 含"渲染能力未装配"（环境问题）与字体/布局等实现错误。
		// 细节进日志，群里只给一句人话，避免把内部错误横向扩散。
		p.log.Warn("markdown 渲染失败", "err", err, "len", len(src))
		return &host.Reply{Content: "渲染失败，请稍后再试"}, nil
	}
}

// parse 拆出"指令后剩下的部分"。ok=false 表示这条消息不是 md 指令。
//
// Manifest 的前缀匹配是裸的 HasPrefix，`/md5 abc` 也会命中，所以"指令词是否
// 独立成词"只能在这里判。后半段整段原样交给渲染器——用 strings.Fields 会把
// 换行压成空格，多行 markdown（表格、代码块）会被拼坏。
func parse(content string) (src string, ok bool) {
	rest, hit := strings.CutPrefix(strings.TrimSpace(content), "/md")
	if !hit {
		return "", false
	}
	// 紧跟的必须是空白或结尾：/md5、/mdx 不是本指令。
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// unescape 把单行里写的 `\n`、`\t`、`\\` 还原成真字符。
//
// 存在的唯一理由就是上面那条平台事实：换行送不进来，多行 markdown 只能在
// 一行里写 `\n`。代价是 `C:\new` 这类路径会被当成换行——要写反斜杠就打 `\\`。
// 只认这三个序列，其余 `\x`（`\d`、`\S` 之类正则片段）原样保留：宁可少猜，
// 也不要把用户正则里的反斜杠吃掉。
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		// UTF-8 的续字节都 ≥ 0x80，不可能等于 '\\'，所以按字节扫不会切坏中文。
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\') // 未知转义：留着，下一轮按普通字符处理
			continue
		}
		i++
	}
	return b.String()
}
