package core

import (
	"github.com/knight000/go_qqbot/internal/host"
)

// 平台 intent 位（bot.intents 位掩码）。只列本核心真正解码过的事件所在的那几位，
// 不抄整张官方表：多列一位就意味着某条映射没人验证过，而这里的每条映射都会
// 影响启动日志。
const (
	// intentGroupAndC2C：群与单聊事件，官方名 GROUP_AND_C2C_EVENT。
	// 覆盖群/单聊消息、入群退群、好友增删、消息接收与拒绝回执。
	intentGroupAndC2C uint64 = 1 << 25
	// intentInteraction：交互事件，官方名 INTERACTION。
	// 覆盖 INTERACTION_CREATE（按钮回调），对应 host.KindMeta。
	intentInteraction uint64 = 1 << 26
)

// kindIntentBit 把 Manifest.Intents 的大类映射到平台位。
//
// 只描述"要收到这一类事件，配置里必须打开哪一位"。映射错了的后果是启动日志
// 里一条无意义的 Warn 或一条缺失的 Warn，不会改变任何路由行为。
var kindIntentBit = map[host.Kind]struct {
	bit  uint64
	name string
}{
	host.KindMessage: {intentGroupAndC2C, "1<<25 GROUP_AND_C2C_EVENT"},
	host.KindNotice:  {intentGroupAndC2C, "1<<25 GROUP_AND_C2C_EVENT"},
	host.KindRequest: {intentGroupAndC2C, "1<<25 GROUP_AND_C2C_EVENT"},
	host.KindMeta:    {intentInteraction, "1<<26 INTERACTION"},
}

// warnIntentGaps 做装配期的交叉校验：插件声明的 Intents 与 [bot].intents 位掩码
// 求交，缺位**只告警、不拒绝启动**。
//
// 为什么不是 fail-fast：路由表与网关订阅是两件独立的事，缺位不会让进程处于
// 不一致状态——插件只是永远收不到那类事件。升级时因为一条告警级问题直接起不来，
// 代价远大于收益；反过来，完全静默又会让"插件装了、指令没反应"无从排查。
//
// 解析失败在这里静默跳过：NewRouter 已经拒绝过非法 intent，能走到这一步说明
// 清单是合法的，重复报错只会让同一条问题出现两次。
func (c *Core) warnIntentGaps() {
	for _, p := range c.plugins {
		m := p.Manifest()
		intents, err := parseIntents(m.Name, m.Intents)
		if err != nil {
			continue
		}
		seen := make(map[host.Kind]bool, len(intents))
		for _, it := range intents {
			if seen[it.kind] {
				continue
			}
			seen[it.kind] = true
			req, ok := kindIntentBit[it.kind]
			if !ok || c.cfg.Bot.Intents&req.bit != 0 {
				continue
			}
			c.log.Warn("插件订阅的事件类型没有在 bot.intents 打开，网关不会收到",
				"plugin", m.Name,
				"intent", string(it.kind),
				"missing_bit", req.name,
				"intents", c.cfg.Bot.Intents)
		}
	}
}
