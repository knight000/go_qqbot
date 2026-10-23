// Package md 把一段 markdown 源码画成图片回复，是最小的「渲染能力」示例插件。
//
// 指令：`/md <内容>`。只认带斜杠的写法，理由是两条：
//   - 与核心保留指令（/claim /reset /group …）同一形状，用户不必记两套约定；
//   - `md` 在中文聊天里是常见的脏话缩写，宽松匹配会在线上的正常对话里误触。
//
// 命令词边界（`/md5 abc` 不算 `/md`）与参数切分都在路由层，插件只读
// Event.Command / Event.Args；这里不再自己 CutPrefix。
//
// 两条实现约束值得复制到别的渲染类插件里：
//  1. 渲染必须在 Handle 内同步完成。Sender.Reply 的错误在 dispatch 里才处理，
//     那时 Handle 已经返回，插件拿不到"渲染失败"，所以"发送时再渲染"这条路不通。
//  2. 渲染成功就不要再带文本。Reply 同时有 Content 与 Image 时核心会拆成两条消息，
//     消耗两格被动配额（群 5 次/5 分钟），纯属浪费。
//
// 平台事实（2026-10-19 用 [debug].raw 取证）：群消息里的换行是**单独 CR**
// （`"\r"`），不是 LF——`"content":"/md # 标题\r## 二级标题"`。goldmark 只认
// `\n` 与 `\r\n`，于是整段被当成一个 ATX 标题画成一行（166 px 而非 466 px）。
// 该差异已由网关统一归一化成 LF，插件拿到的正文就是正常多行。
//
// CR 归一化只解决「整段被当成一个标题」；段落内部的单个换行被拍平成空格
// 是另一层问题（#32）。换行语义成文在 `docs/ARCHITECTURE.md` §3.8：源码一行
// = 视觉一行，由渲染器统一保证，插件不参与。
//
// 因此正文一律**原样**交给渲染器：所见即所渲染。反斜杠不解释，
// 代码块里写 `C:\new`、正则里写 `\d+` 都不会被吃掉。
package md

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/knight000/go_qqbot/pkg/host"
)

const usage = "用法：/md <markdown 源码>，可直接换行；例：/md # 标题"

type Plugin struct {
	host host.Host
	log  *slog.Logger
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "md",
		Version:      "0.3.0",
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
	// 命令词边界（/md5 不算 /md）与参数切分都在路由层：命中的事件才投递到
	// 这里，正文已按 /md 之后的剩余原文 TrimSpace 好，插件不再判"是不是本指令"。
	src := ev.Args
	if src == "" {
		return &host.Reply{Content: usage}, nil
	}
	// 行数是区分"没换行"与"没渲染"的唯一凭据，排查换行类问题先看这行。
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
