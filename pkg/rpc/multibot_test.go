package rpc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件是「一套插件进程服务多个 Bot」的最小回归（ADR-0008 决策 11）。
//
// 它跑在**线协议**上而不是进程内替身：插件侧拿到的是 HostClient，Bot 的
// 解析只能靠每条调用的 bot_id 与 ctx——这正是最容易写错的地方（把"当前
// Bot"存成字段，单 Bot 用例全绿，多 Bot 立刻串库）。

// multiBotKV 是一个 Bot 一个的内存 KV 后端。
type multiBotKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMultiBotKV() *multiBotKV { return &multiBotKV{m: make(map[string][]byte)} }

func (k *multiBotKV) KVGet(_ context.Context, plugin, peer, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[plugin+"|"+peer+"|"+key]
	return v, ok, nil
}

func (k *multiBotKV) KVSet(_ context.Context, plugin, peer, key string, val []byte, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[plugin+"|"+peer+"|"+key] = val
	return nil
}

func (k *multiBotKV) KVDelete(_ context.Context, plugin, peer, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, plugin+"|"+peer+"|"+key)
	return nil
}

// multiBotPlugin 把 Init 拿到的 Host 存起来，在 Handle 里照旧用它——插件
// 无感的那条路：h 是 Init 时那一个 Bot 的，但带 ctx 的调用会按**当前事件**
// 的 Bot 落库。
type multiBotPlugin struct {
	mu    sync.Mutex
	hosts map[string]host.Host // botID → Init 时拿到的 h
}

func (p *multiBotPlugin) Manifest() host.Manifest { return host.Manifest{Name: "multibot"} }

func (p *multiBotPlugin) Init(ctx context.Context, h host.Host) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hosts == nil {
		p.hosts = make(map[string]host.Host)
	}
	p.hosts[h.Bot().AppID] = h
	return nil
}

func (p *multiBotPlugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	cur := host.From(ctx).Bot()
	if cur.IsZero() {
		return &host.Reply{Content: "no-bot"}, nil
	}
	// 故意用"第一个 Init 拿到的那个 h"：如果实现里存了"当前 Bot"的可变
	// 字段，这里就会写错库。
	p.mu.Lock()
	var h host.Host
	for _, v := range p.hosts {
		h = v
		break
	}
	p.mu.Unlock()

	key := "seen"
	if err := h.KV().Set(ctx, key, []byte(cur.AppID), 0); err != nil {
		return nil, err
	}
	prev, _, err := h.KV().Get(ctx, key)
	if err != nil {
		return nil, err
	}
	// 读回来必须是自己刚写的那个值：串库时这里会是另一个 Bot 的 appid。
	return &host.Reply{Content: cur.AppID + "/" + cur.CertType + "/" + string(prev)}, nil
}

// TestSharedPluginServesTwoBots 覆盖票 #65 的第一条验收：
// 同一插件实例收到两个 Bot 的事件，Host.Bot() 能区分，Host.KV() 落到正确的库。
func TestSharedPluginServesTwoBots(t *testing.T) {
	p := &multiBotPlugin{}
	a, b := newPair(t)
	if err := ServePlugin(b, p); err != nil {
		t.Fatalf("ServePlugin: %v", err)
	}

	kvA, kvB := newMultiBotKV(), newMultiBotKV()
	hostA := host.NewService(host.Options{Name: "multibot", Bot: host.BotInfo{AppID: "bot-a", CertType: "personal"}, KV: kvA})
	hostB := host.NewService(host.Options{Name: "multibot", Bot: host.BotInfo{AppID: "bot-b", CertType: "enterprise"}, KV: kvB})
	hosts := map[string]host.Host{"bot-a": hostA, "bot-b": hostB}
	if err := ServeHost(a, func(botID string) (host.Host, error) {
		h, ok := hosts[botID]
		if !ok {
			t.Fatalf("resolver 收到了未知的 Bot %q", botID)
		}
		return h, nil
	}); err != nil {
		t.Fatalf("ServeHost: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 同一个插件实例，两个 Bot 各 init 一次。
	if err := a.CallInit(ctx, host.BotInfo{AppID: "bot-a", CertType: "personal"}, "multibot", nil); err != nil {
		t.Fatalf("init bot-a: %v", err)
	}
	if err := a.CallInit(ctx, host.BotInfo{AppID: "bot-b", CertType: "enterprise"}, "multibot", nil); err != nil {
		t.Fatalf("init bot-b: %v", err)
	}

	// 交替投递：并发下"当前 Bot"这种可变字段一定会串。
	for _, tc := range []struct{ bot, cert string }{
		{"bot-a", "personal"},
		{"bot-b", "enterprise"},
		{"bot-a", "personal"},
		{"bot-b", "enterprise"},
	} {
		reply, err := a.CallHandle(ctx, host.BotInfo{AppID: tc.bot, CertType: tc.cert}, &host.Event{
			ID: tc.bot + "-" + tc.cert, Kind: host.KindMessage, Content: otherBot(tc.bot),
		})
		if err != nil {
			t.Fatalf("%s 的 handle: %v", tc.bot, err)
		}
		want := tc.bot + "/" + tc.cert + "/" + tc.bot
		if reply == nil || reply.Content != want {
			t.Fatalf("%s 的回复 = %v，期望 %q（串库或 Bot 识别错误）", tc.bot, reply, want)
		}
	}

	// 两个库各写一个键、互不可见：写的是同一个 key，值是各自的 appid。
	// 断言落在后端（而不是再过一遍带 ctx 的客户端），交叉读不成立才说明真的
	// 是两份库，而不是两条调用都落到了同一个后端。
	if v, ok, _ := kvA.KVGet(ctx, "multibot", "", "seen"); !ok || string(v) != "bot-a" {
		t.Fatalf("bot-a 的库 = %q（found=%v），期望 bot-a", v, ok)
	}
	if v, ok, _ := kvB.KVGet(ctx, "multibot", "", "seen"); !ok || string(v) != "bot-b" {
		t.Fatalf("bot-b 的库 = %q（found=%v），期望 bot-b", v, ok)
	}
	// 只往 A 写一个 A 独有的键：B 的库里必须查不到它。
	if err := hostA.KV().Set(ctx, "only-a", []byte("x"), 0); err != nil {
		t.Fatalf("写 A 的独有键: %v", err)
	}
	if _, found, _ := kvB.KVGet(ctx, "multibot", "", "only-a"); found {
		t.Fatal("bot-a 写的键出现在 bot-b 的库里：两份 Host 没有真正分开")
	}
}

func otherBot(id string) string {
	if id == "bot-a" {
		return "bot-b"
	}
	return "bot-a"
}

// 热重连（`/bot reload <appid>`）会对同一个 Bot 再来一次 init：它在核心侧
// 是「停核心 → 摘 Host → 重新装配」，在插件侧必须是一次干净的重新初始化。
//
// 这条用例钉的是"重新 init 被允许"以及"旧状态确实被换掉"。之前线协议把
// init 当成一次性动作（Init 每 Bot 只调用一次），热重连之后这个 Bot 的所有
// 事件都会撞在"已经初始化过"的拒绝上——插件全哑，且没有任何日志。
func TestReinitSameBotAfterReload(t *testing.T) {
	p := &multiBotPlugin{}
	a, b := newPair(t)
	if err := ServePlugin(b, p); err != nil {
		t.Fatalf("ServePlugin: %v", err)
	}

	kv1, kv2 := newMultiBotKV(), newMultiBotKV()
	h1 := host.NewService(host.Options{Name: "multibot", Bot: host.BotInfo{AppID: "bot-a", CertType: "personal"}, KV: kv1})
	h2 := host.NewService(host.Options{Name: "multibot", Bot: host.BotInfo{AppID: "bot-a", CertType: "personal"}, KV: kv2})
	cur := host.Host(h1)
	if err := ServeHost(a, func(string) (host.Host, error) { return cur, nil }); err != nil {
		t.Fatalf("ServeHost: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info := host.BotInfo{AppID: "bot-a", CertType: "personal"}
	if err := a.CallInit(ctx, info, "multibot", nil); err != nil {
		t.Fatalf("首次 init: %v", err)
	}
	p.mu.Lock()
	first := p.hosts["bot-a"]
	p.mu.Unlock()

	// 核心侧重连：换上新的 Host，再 init 一次同一个 Bot。
	cur = h2
	if err := a.CallInit(ctx, info, "multibot", nil); err != nil {
		t.Fatalf("重新 init 同一个 Bot 应被允许: %v", err)
	}

	// 插件侧拿到的是**新一轮的 HostClient**：同一个 Bot 只该留一条记录，
	// 且必须换成新那份（留着旧的等于还在往已经关掉的库上写）。
	p.mu.Lock()
	second, n := p.hosts["bot-a"], len(p.hosts)
	p.mu.Unlock()
	if n != 1 || second == nil || second == first {
		t.Fatalf("插件侧应只剩新的 HostClient（n=%d，复用旧值=%v）", n, second == first)
	}

	// 重连之后事件照常可达：gate 是新建的，不该再挂着。
	reply, err := a.CallHandle(ctx, info, &host.Event{ID: "e-after-reload", Kind: host.KindMessage})
	if err != nil {
		t.Fatalf("重连后的 handle: %v", err)
	}
	if reply == nil || !strings.HasPrefix(reply.Content, "bot-a/personal/") {
		t.Fatalf("重连后的回复 = %v", reply)
	}
}
