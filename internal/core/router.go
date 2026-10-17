package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/knight000/qqbot/internal/host"
)

// reservedPrefixes 是核心保留指令。插件若声明相同前缀，启动即拒绝加载，
// 否则第三方插件可以劫持 /reset 之类的不可逆操作（§3.4），
// 或者用 /admin 给自己提权、用 /reload 让机器人反复下线。
var reservedPrefixes = []string{
	"/claim", "/admin", "/reset", "/grant", "/revoke", "/reload", "/restart",
}

type compiledCommand struct {
	prefix   string
	re       *regexp.Regexp
	priority int
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
		if m.APIVersion != host.APIVersion {
			return nil, fmt.Errorf("插件 %s 的 api_version=%q，与本核心 %q 不匹配",
				m.Name, m.APIVersion, host.APIVersion)
		}

		intents, err := parseIntents(m.Name, m.Intents)
		if err != nil {
			return nil, err
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
			cc := compiledCommand{prefix: c.Prefix, priority: c.Priority, label: c.Prefix}
			if c.Prefix != "" {
				for _, rp := range reservedPrefixes {
					if strings.HasPrefix(strings.ToLower(c.Prefix), rp) {
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

// Match 返回按 priority 降序排列的候选。同一优先级只应有一个插件处理，
// 除非该插件显式声明 broadcast=true。
type Match struct {
	Plugin   host.Plugin
	Manifest host.Manifest
	Priority int
	// Command 是命中的命令写法（前缀或正则），供调试输出定位"为什么是它"。
	Command string
}

func (r *Router) Match(ev *host.Event) []Match {
	var out []Match
	for _, rt := range r.routes {
		if !rt.intents.match(ev) {
			continue
		}
		prio, cmd, ok := matchCommands(rt.cmds, ev)
		if !ok {
			continue
		}
		out = append(out, Match{Plugin: rt.plugin, Manifest: rt.manifest, Priority: prio, Command: cmd})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Manifest.Name < out[j].Manifest.Name
	})
	return out
}

func matchCommands(cmds []compiledCommand, ev *host.Event) (priority int, command string, ok bool) {
	// 未声明命令 = 订阅该 intent 的全部事件
	if len(cmds) == 0 {
		return 0, "", true
	}
	if !ev.IsMessage() {
		return 0, "", false
	}
	best := -1 << 31
	content := strings.TrimSpace(ev.Content)
	for _, c := range cmds {
		hit := false
		if c.prefix != "" {
			hit = strings.HasPrefix(content, c.prefix)
		} else if c.re != nil {
			hit = c.re.MatchString(content)
		}
		if hit && c.priority > best {
			best = c.priority
			command = c.label
			ok = true
		}
	}
	return best, command, ok
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
