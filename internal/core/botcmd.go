package core

import (
	"context"

	"github.com/knight000/go_qqbot/pkg/host"
)

// /bot 系列是进程级命令：热增删改 Bot、只重启某一个 Bot（ADR-0008 决策 15）。
// 它只认 Core 所有者——这是与 Bot 所有者最关键的分界，否则任何租户都能操作
// 别人的 Bot。
//
// 命令体在 T8（#68）实现。本版本先占住前缀并给出明确回复：不占位的话，
// 第三方插件声明 /bot 会在加载期被拒绝（保留前缀），而核心又不处理它，
// 结果是一条永远没有回应的指令——比"暂未实现"难查得多。
func (c *Core) handleBot(ctx context.Context, ev *host.Event) {
	if !c.isCoreOwner(ev) {
		c.replyText(ctx, ev, "该指令仅 Core 所有者可用。")
		return
	}
	c.replyText(ctx, ev, "/bot 系列（热增删改 Bot）随 T8 装配，本版本尚未实现。")
}
