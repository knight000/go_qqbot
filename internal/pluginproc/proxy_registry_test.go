package pluginproc

import (
	"testing"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 插件进程崩溃重启后，核心要把每个已登记的 Bot 重新 init 一遍。重放必须带上
// **认证类型**：只带 appid 会让插件侧 `Host.Bot().CertType` 变成空串，按认证
// 档位算限额/挑提示语的插件会静默算错，而且没有任何报错。
func TestRegisteredBotsKeepCertType(t *testing.T) {
	p := &proxy{
		hosts:    map[string]host.Host{},
		botInfos: map[string]host.BotInfo{},
	}
	p.hosts["bot-a"] = host.NewService(host.Options{
		Name: "p", Bot: host.BotInfo{AppID: "bot-a", CertType: "enterprise"},
	})
	p.botInfos["bot-a"] = host.BotInfo{AppID: "bot-a", CertType: "enterprise"}
	p.hosts["bot-b"] = host.NewService(host.Options{Name: "p", Bot: host.BotInfo{AppID: "bot-b"}})

	got := make(map[string]string, 2)
	for _, b := range p.registeredBots() {
		got[b.AppID] = b.CertType
	}
	if got["bot-a"] != "enterprise" {
		t.Fatalf("bot-a 的认证类型 = %q，期望 enterprise", got["bot-a"])
	}
	// 没有记录的 Bot 退回只有 appid 的形态：仍要出现在列表里，不能让这个
	// Bot 在重启后彻底不初始化。
	if _, ok := got["bot-b"]; !ok {
		t.Fatal("没有 botInfos 记录的 Bot 必须仍然出现在重放列表里")
	}
}

// 摘掉一个 Bot 之后，它的 Host 与 BotInfo 都不该留下：留着表的后果是一条
// 迟到的 host.* 调用写进一个正在关闭的库，或者重放时给一个已销毁的 Bot
// 重新 init。
func TestUnregisterBotHostDropsBoth(t *testing.T) {
	p := &proxy{
		hosts:    map[string]host.Host{},
		botInfos: map[string]host.BotInfo{},
		cfgs:     map[string]map[string]any{},
		started:  map[string]bool{},
	}
	p.hosts["bot-a"] = host.NewService(host.Options{Name: "p", Bot: host.BotInfo{AppID: "bot-a"}})
	p.botInfos["bot-a"] = host.BotInfo{AppID: "bot-a", CertType: "personal"}
	p.cfgs["bot-a"] = map[string]any{"k": "v"}
	p.started["bot-a"] = true

	p.UnregisterBotHost("bot-a")

	if len(p.hosts) != 0 || len(p.botInfos) != 0 || len(p.cfgs) != 0 || len(p.started) != 0 {
		t.Fatalf("摘掉后仍有残留：hosts=%d botInfos=%d cfgs=%d started=%d",
			len(p.hosts), len(p.botInfos), len(p.cfgs), len(p.started))
	}
}
