// Package md 把一段 markdown 源码画成图片回复，是最小的「渲染能力」示例插件。
//
// 指令：`/md <内容>`；也接受裸写的 `md <内容>`，两者都要求指令词独立成词
// （`/md5`、`md5sum` 不会被误判）。
//
// 两条实现约束值得复制到别的渲染类插件里：
//  1. 渲染必须在 Handle 内同步完成。Sender.Reply 的错误在 dispatch 里才处理，
//     那时 Handle 已经返回，插件拿不到"渲染失败"，所以"发送时再渲染"这条路不通。
//  2. 渲染成功就不要再带文本。Reply 同时有 Content 与 Image 时核心会拆成两条消息，
//     消耗两格被动配额（群 5 次/5 分钟），纯属浪费。
//
// 裸写形式是刻意保留的便利，不是疏忽：中文群里 "md" 也是常见脏话缩写，
// 放宽到只认 `md ` 加空格能把误伤压到很低，但无法归零。真要彻底关掉，
// 把 isCommand 里的 bare 分支和 Manifest 里的 Pattern 命令一起删掉即可。
package md

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"

	"github.com/knight000/go_qqbot/internal/host"
)

func init() { host.Register(&Plugin{}) }

const usage = "用法：/md <markdown 内容>，例如 /md # 标题"

type Plugin struct {
	host host.Host
	log  *slog.Logger
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "md",
		Version:      "0.1.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.reply", "render", "log"},
		Intents:      []string{"message"},
		Commands: []host.Command{
			{Prefix: "/md", Priority: 100, Description: "把 markdown 画成图片"},
			// 正则而不是前缀 "md"：前缀匹配是裸的 HasPrefix，会把 "md5 xxx" 也吃掉。
			{Pattern: `^md\s`, Priority: 100, Description: "同上（裸写形式）"},
		},
	}
}

func (p *Plugin) Init(_ context.Context, h host.Host) error {
	p.host = h
	p.log = h.Log().With("plugin", "md")
	p.log.Info("md 插件已初始化")
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	src, ok := parse(ev.Content)
	if !ok {
		return nil, nil // 不是本指令，交给优先级更低的插件
	}
	if src == "" {
		return &host.Reply{Content: usage}, nil
	}

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
// 只取第一个空白前的词做判定，剩下的整段原样交给渲染器——用 strings.Fields
// 会把换行压成空格，多行 markdown（表格、代码块）会被拼坏。
func parse(content string) (src string, ok bool) {
	content = strings.TrimSpace(content)
	head, rest := content, ""
	if i := strings.IndexFunc(content, unicode.IsSpace); i >= 0 {
		head, rest = content[:i], content[i:]
	}
	// 大小写敏感是刻意的：Manifest 的前缀匹配本身区分大小写，
	// 这里再宽容会造成"路由没命中但插件认了"的不一致。
	if head != "/md" && head != "md" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}
