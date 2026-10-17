package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/knight000/qqbot/internal/host"
)

// reservedPrefixes 是核心保留指令。插件若声明相同前缀，启动即拒绝加载，
// 否则第三方插件可以劫持 /reset 之类的不可逆操作（§3.4）。
var reservedPrefixes = []string{"/claim", "/reset", "/grant", "/revoke"}

type compiledCommand struct {
	prefix   string
	re       *regexp.Regexp
	priority int
}

type route struct {
	plugin   host.Plugin
	manifest host.Manifest
	cmds     []compiledCommand
	intents  map[string]bool
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

		rt := &route{
			plugin:   p,
			manifest: m,
			intents:  make(map[string]bool, len(m.Intents)),
		}
		for _, it := range m.Intents {
			rt.intents[it] = true
		}

		for _, c := range m.Commands {
			if (c.Prefix == "") == (c.Pattern == "") {
				return nil, fmt.Errorf("插件 %s 的命令必须且只能设置 Prefix 或 Pattern 之一", m.Name)
			}
			cc := compiledCommand{prefix: c.Prefix, priority: c.Priority}
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
}

func (r *Router) Match(ev *host.Event) []Match {
	var out []Match
	for _, rt := range r.routes {
		if !rt.intents[ev.Type] {
			continue
		}
		prio, ok := matchCommands(rt.cmds, ev)
		if !ok {
			continue
		}
		out = append(out, Match{Plugin: rt.plugin, Manifest: rt.manifest, Priority: prio})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Manifest.Name < out[j].Manifest.Name
	})
	return out
}

func matchCommands(cmds []compiledCommand, ev *host.Event) (int, bool) {
	// 未声明命令 = 订阅该 intent 的全部事件
	if len(cmds) == 0 {
		return 0, true
	}
	if !ev.IsMessage() {
		return 0, false
	}
	best := -1 << 31
	found := false
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
			found = true
		}
	}
	return best, found
}
