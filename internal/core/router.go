package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/knight000/go_qqbot/pkg/host"
)

// reservedPrefixes 是核心保留指令。插件若声明相同前缀，启动即拒绝加载，
// 否则第三方插件可以劫持 /reset 之类的不可逆操作（§3.4），
// 或者用 /admin 给自己提权、用 /reload 让机器人反复下线。
//
// 这份清单必须与 handleReserved 的 case 一一对应：列了却不处理，等于
// 白占一个前缀（第三方插件想用也加载不了），而未列却被处理，则插件声明的
// 同名命令会被核心静默遮蔽。/grant、/revoke 已随功能移除一并退场。
var reservedPrefixes = []string{
	"/claim", "/admin", "/group", "/reset", "/reload", "/restart", "/upgrade",
}

// reservedConflict 判断插件前缀是否占用了核心指令，按**词边界**比较。
//
// 不能裸 HasPrefix：`/groupinfo`、`/reset2` 是独立的名字，与 `/group`、`/reset`
// 无关，裸前缀比较会把它们一起拒掉（注释说的"声明相同前缀"并不成立）。
// 边界与 argsAfterPrefix 的严格匹配一致：前缀之后必须是结尾或空白。
// prefix 需已转小写；reservedPrefixes 本身是小写字面量。
func reservedConflict(prefix, reserved string) bool {
	return prefix == reserved || strings.HasPrefix(prefix, reserved+" ")
}

type compiledCommand struct {
	prefix string
	// loose 放宽前缀匹配：默认要求前缀之后是空白或结尾（/md5 不算 /md）。
	loose    bool
	re       *regexp.Regexp
	priority int
	// perm 是解析后的权限要求（已处理"命令级覆盖插件级"的继承）。
	perm host.Perm
	// deny 是权限不足时的回复，空串表示静默。
	deny string
	// label 是给人看的原始写法（prefix 或正则），只在调试轨迹里用。
	label string
}

type route struct {
	plugin   host.Plugin
	manifest host.Manifest
	cmds     []compiledCommand
	intents  intentSet
}

// Router 在插件的 matcher 表上做匹配，核心不解析业务语义。
type Router struct {
	routes []*route
}

func NewRouter(plugins []host.Plugin) (*Router, error) {
	r := &Router{}
	for _, p := range plugins {
		m := p.Manifest()
		if m.Name == "" {
			return nil, fmt.Errorf("插件 Manifest.Name 为空")
		}
		// 加载判定看的是「插件实际用到的契约成员有没有变」，不是版本号相等
		// （ADR-0007）。没有声明使用面的插件仍按整体相等拒载，与引入判定
		// 之前一字不差；错误串点名具体成员，现场可以直接照日志改。
		if err := host.CheckCompat(m.APIVersion, m.Uses); err != nil {
			return nil, fmt.Errorf("插件 %s: %w", m.Name, err)
		}

		intents, err := parseIntents(m.Name, m.Intents)
		if err != nil {
			return nil, err
		}
		// 权限位写错（拼了一个不存在的位）会让插件变成"谁都能用"或"谁都不能用"，
		// 属于装配期错误，必须在启动时报出来而不是线上观察。
		if !m.Permission.Valid() {
			return nil, fmt.Errorf("插件 %s 的 Manifest.Permission 含未定义的权限位（%s）",
				m.Name, m.Permission.String())
		}
		rt := &route{
			plugin:   p,
			manifest: m,
			intents:  intents,
		}

		for _, c := range m.Commands {
			if (c.Prefix == "") == (c.Pattern == "") {
				return nil, fmt.Errorf("插件 %s 的命令必须且只能设置 Prefix 或 Pattern 之一", m.Name)
			}
			if !c.Permission.Valid() {
				return nil, fmt.Errorf("插件 %s 的命令 %q 含未定义的权限位（%s）",
					m.Name, c.Prefix+c.Pattern, c.Permission.String())
			}
			cc := compiledCommand{
				prefix:   c.Prefix,
				loose:    c.Loose,
				priority: c.Priority,
				label:    c.Prefix,
				perm:     inheritPerm(m.Permission, c.Permission),
				deny:     inheritDeny(m.DenyReply, c.DenyReply),
			}
			if c.Prefix != "" {
				lower := strings.ToLower(c.Prefix)
				for _, rp := range reservedPrefixes {
					if reservedConflict(lower, rp) {
						return nil, fmt.Errorf("插件 %s 声明了核心保留前缀 %s，拒绝加载", m.Name, rp)
					}
				}
			}
			if c.Pattern != "" {
				re, err := regexp.Compile(c.Pattern)
				if err != nil {
					return nil, fmt.Errorf("插件 %s 的正则 %q 无法编译: %w", m.Name, c.Pattern, err)
				}
				cc.re = re
				cc.label = c.Pattern
			}
			rt.cmds = append(rt.cmds, cc)
		}
		r.routes = append(r.routes, rt)
	}
	return r, nil
}

// Lookup 按插件名取路由项，未加载时返回 nil。
//
// 线性查找而不是建 map：插件数量是个位数，而给 Router 加第二份索引意味着
// 两处状态要保持一致；会话投递只在命中会话行时调用，不在每条消息的热路径上。
func (r *Router) Lookup(name string) *route {
	for _, rt := range r.routes {
		if rt.manifest.Name == name {
			return rt
		}
	}
	return nil
}

// Match 返回按 priority 降序排列的候选：priority 降序，同 priority 按插件名升序。
// 这个顺序就是 Dispatcher 的执行顺序——同组内逐个执行，遇到不让位者为止（§3.4），
// 所以排序必须确定，不能依赖 map 遍历。
type Match struct {
	Plugin   host.Plugin
	Manifest host.Manifest
	Priority int
	// Command 是命中的命令写法（前缀或正则），供调试输出定位"为什么是它"；
	// 也是回传给插件的 Event.Command。
	Command string
	// Args 是命令之后、已 TrimSpace 的剩余原文，回传给插件的 Event.Args。
	Args string
	// Perm 是执行该命令所需的权限（已解析继承关系）；零值表示不限制。
	Perm host.Perm
	// DenyReply 是权限不足时的回复文本，空串表示静默丢弃。
	DenyReply string
}

func (r *Router) Match(ev *host.Event) []Match {
	var out []Match
	for _, rt := range r.routes {
		if !rt.intents.match(ev) {
			continue
		}
		cmd, args, ok := matchCommands(rt.cmds, ev)
		if !ok {
			continue
		}
		m := Match{Plugin: rt.plugin, Manifest: rt.manifest}
		if cmd == nil {
			// 未声明命令 = 订阅该 intent 的全部事件，权限取插件级
			m.Perm, m.DenyReply = rt.manifest.Permission, rt.manifest.DenyReply
		} else {
			m.Priority, m.Command, m.Args = cmd.priority, cmd.label, args
			m.Perm, m.DenyReply = cmd.perm, cmd.deny
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Manifest.Name < out[j].Manifest.Name
	})
	return out
}

// inheritPerm 处理"命令级覆盖插件级"：命令未声明（零值）时继承插件默认。
// 想让某个命令显式公开，写 PermUser——零值不能表达"公开"，因为零值表示未声明。
func inheritPerm(plugin, cmd host.Perm) host.Perm {
	if cmd == 0 {
		return plugin
	}
	return cmd
}

func inheritDeny(plugin, cmd string) string {
	if cmd == "" {
		return plugin
	}
	return cmd
}

// matchCommands 返回命中的命令（nil 表示该插件没有声明命令，订阅全部事件）
// 以及命令之后的剩余原文。
func matchCommands(cmds []compiledCommand, ev *host.Event) (best *compiledCommand, args string, ok bool) {
	// 未声明命令 = 订阅该 intent 的全部事件
	if len(cmds) == 0 {
		return nil, "", true
	}
	if !ev.IsMessage() {
		return nil, "", false
	}
	content := strings.TrimSpace(ev.Content)
	for i := range cmds {
		c := &cmds[i]
		var rest string
		hit := false
		switch {
		case c.prefix != "":
			rest, hit = argsAfterPrefix(content, c.prefix, c.loose)
		case c.re != nil:
			// 正则按源码找匹配区间，Args 取区间之后的剩余（不是整段内容）。
			if loc := c.re.FindStringIndex(content); loc != nil {
				rest, hit = strings.TrimSpace(content[loc[1]:]), true
			}
		}
		if hit && (best == nil || c.priority > best.priority) {
			best, args = c, rest
		}
	}
	return best, args, best != nil
}

// argsAfterPrefix 判断 content 是否以 prefix 开头，并返回其后的剩余原文（已 TrimSpace）。
//
// 词边界在这里收口：loose=false 要求 prefix 之后是空白或字符串结尾，所以 `/foobar abc`
// 不命中 `/foo`。这条规则曾经散在每个插件里（各自的 CutPrefix + IsSpace 判断），
// 漏一个插件就多一处误命中，所以只有这一份实现。loose=true 退回裸 HasPrefix 语义，
// 只给"前缀本身就是完整词、后面紧跟内容"的写法用。
func argsAfterPrefix(content, prefix string, loose bool) (args string, ok bool) {
	rest, ok := strings.CutPrefix(content, prefix)
	if !ok {
		return "", false
	}
	if !loose {
		if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
			return "", false
		}
	}
	return strings.TrimSpace(rest), true
}

// intent 是解析后的订阅项：kind 必填，scene 为空表示"任意场景"。
type intent struct {
	scene host.Scene
	kind  host.Kind
}

type intentSet []intent

// parseIntents 解析 Manifest.Intents：`message`（任意场景）或 `group.message`（限定场景）。
//
// 写错在装配阶段就报错，而不是让插件"看起来订阅了、实际一条都收不到"——
// 未知事件永不进入路由，拼写错误不会被任何运行时现象掩盖。
func parseIntents(plugin string, list []string) (intentSet, error) {
	set := make(intentSet, 0, len(list))
	for _, raw := range list {
		parts := strings.Split(strings.TrimSpace(raw), ".")
		it := intent{}
		switch len(parts) {
		case 1:
			it.kind = host.Kind(parts[0])
		case 2:
			it.scene = host.Scene(parts[0])
			it.kind = host.Kind(parts[1])
		default:
			return nil, fmt.Errorf("插件 %s 的 intent %q 格式错误，应为 `message` 或 `group.message`", plugin, raw)
		}
		if !validKind(it.kind) {
			return nil, fmt.Errorf("插件 %s 的 intent %q 含未知大类 %q（可选 message / notice / request / meta）",
				plugin, raw, it.kind)
		}
		if it.scene != "" && !validScene(it.scene) {
			return nil, fmt.Errorf("插件 %s 的 intent %q 含未知场景 %q（可选 group / c2c / guild）",
				plugin, raw, it.scene)
		}
		set = append(set, it)
	}
	return set, nil
}

func validKind(k host.Kind) bool {
	switch k {
	case host.KindMessage, host.KindNotice, host.KindRequest, host.KindMeta:
		return true
	}
	return false
}

func validScene(s host.Scene) bool {
	switch s {
	case host.SceneGroup, host.SceneC2C, host.SceneGuild:
		return true
	}
	return false
}

func (s intentSet) match(ev *host.Event) bool {
	for _, it := range s {
		if it.kind == ev.Kind && (it.scene == "" || it.scene == ev.Scene) {
			return true
		}
	}
	return false
}
