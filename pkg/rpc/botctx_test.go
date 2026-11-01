package rpc

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件钉住 `HostClient.botFor` 的三级解析（client.go 的注释是规格）：
//
//	BotOf(ctx) > ctx 里的 HostClient > 本绑定
//
// 为什么值得单独一组用例：前两级是「插件无感」的全部依据，第三级是唯一的
// 兜底路径，它是**会串库**的那一级——一个插件把某个 Bot 的 Init Host 存进
// 结构体、又在另一个 Bot 的事件里发无 ctx 的调用，写进去的就是那个 Bot 的库。
// 这条兜底不能删（`Scheduler().Register` 没有 ctx），所以至少要让它是响的。

// botCtxPlugin 只用**一个字段**存 Init 拿到的 Host——最容易串库、也最常见的
// 那种写法（多 Bot 时它永远是最后一个 Init 的那份）。
type botCtxPlugin struct {
	mu sync.Mutex
	h  host.Host
}

func (p *botCtxPlugin) Manifest() host.Manifest { return host.Manifest{Name: "botctx"} }

func (p *botCtxPlugin) Init(_ context.Context, h host.Host) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.h = h
	return nil
}

func (p *botCtxPlugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	cur := host.From(ctx).Bot()
	p.mu.Lock()
	h := p.h
	p.mu.Unlock()

	// 两条分支用同一个 h、同一个 key，只差 ctx——落点必须不同。
	var callCtx context.Context
	switch ev.Content {
	case "pin":
		// 后台协程的写法：没有 Host，只有"我捕获到的那个 Bot"。
		callCtx = host.WithBot(context.Background(), cur)
	case "fallback":
		// 什么都不带：落到 h 自己绑定的那个 Bot（这里是最后一次 Init 的）。
		callCtx = context.Background()
	default:
		return nil, nil
	}
	if err := h.KV().Set(callCtx, "k", []byte("for-"+cur.AppID), 0); err != nil {
		return nil, err
	}
	return &host.Reply{Content: cur.AppID}, nil
}

// botCtxFixture 起一对连接：插件先被 bot-a、再被 bot-b 初始化，于是插件结构体
// 里留下的 h 是 **bot-b** 的那份。随后所有 Handle 都投给 bot-a——两者的差就是
// "你以为的 Bot" 与 "h 绑定的 Bot"。
type botCtxFixture struct {
	conn *Conn
	logs *bytes.Buffer
	kvA  *multiBotKV
	kvB  *multiBotKV
	ctx  context.Context
	botA host.BotInfo
	botB host.BotInfo
}

func newBotCtxFixture(t *testing.T) *botCtxFixture {
	t.Helper()
	f := &botCtxFixture{
		logs: &bytes.Buffer{},
		kvA:  newMultiBotKV(),
		kvB:  newMultiBotKV(),
		botA: host.BotInfo{AppID: "bot-a", CertType: "personal"},
		botB: host.BotInfo{AppID: "bot-b", CertType: "enterprise"},
	}
	// NewHostClient 的 logger 取自 slog.Default()，必须在 init 之前换掉。
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a, b := newPair(t)
	if err := ServePlugin(b, &botCtxPlugin{}); err != nil {
		t.Fatalf("ServePlugin: %v", err)
	}
	hosts := map[string]host.Host{
		"bot-a": host.NewService(host.Options{Name: "botctx", Bot: f.botA, KV: f.kvA}),
		"bot-b": host.NewService(host.Options{Name: "botctx", Bot: f.botB, KV: f.kvB}),
	}
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
	t.Cleanup(cancel)
	f.ctx = ctx
	f.conn = a

	// 顺序很重要：a 先、b 后 → 插件字段里留的是 b 的 h。
	for _, info := range []host.BotInfo{f.botA, f.botB} {
		if err := a.CallInit(ctx, info, "botctx", nil); err != nil {
			t.Fatalf("init %s: %v", info.AppID, err)
		}
	}
	return f
}

func (f *botCtxFixture) handle(t *testing.T, content string) {
	t.Helper()
	if _, err := f.conn.CallHandle(f.ctx, f.botA, &host.Event{
		ID: "ev-" + content, Kind: host.KindMessage, Content: content,
	}); err != nil {
		t.Fatalf("handle(%s): %v", content, err)
	}
}

// TestBotForExplicitPinBeatsOwnBinding：显式 `host.WithBot` 钉住的 Bot 赢过
// 「本绑定」。这是插件写后台协程时唯一不串库的姿势，也是本文件存在的主因。
func TestBotForExplicitPinBeatsOwnBinding(t *testing.T) {
	f := newBotCtxFixture(t)
	f.handle(t, "pin")

	// 事件属于 bot-a，插件手里的 h 是 bot-b 的——钉住 Bot 就该落到 a。
	if v, ok, _ := f.kvA.KVGet(f.ctx, "botctx", "", "k"); !ok || string(v) != "for-bot-a" {
		t.Fatalf("bot-a 的库 = %q（found=%v），期望 for-bot-a：显式钉住的 Bot 没有赢过本绑定", v, ok)
	}
	if _, ok, _ := f.kvB.KVGet(f.ctx, "botctx", "", "k"); ok {
		t.Fatal("键出现在了 bot-b 的库：WithBot 被忽略，落到了 h 绑定的 Bot")
	}
	if strings.Contains(f.logs.String(), "回落到 Init 时的绑定") {
		t.Fatalf("钉住 Bot 的正常路径不该报回落告警：\n%s", f.logs.String())
	}
}

// TestBotForFallbackUsesOwnBindingAndWarns：什么都不带的调用落到 h 自己绑定的
// Bot——**这是会串库的那一级**。用例把它钉成"已知行为"，同时要求它不再静默。
//
// 注意串库在这里长什么样：值写着 `for-bot-a`（插件显然在处理 bot-a 的业务），
// 却出现在 **bot-b 的库**里。只断言"键在 b 的库里"还不够——值不匹配才是那份
// 数据真的写错了地方的证据。
//
// 反向验证：把 noteFallback() 删掉，本用例的告警断言变红。
func TestBotForFallbackUsesOwnBindingAndWarns(t *testing.T) {
	f := newBotCtxFixture(t)
	f.handle(t, "fallback")

	// 事件是 bot-a 的，但 h 是 bot-b 的：兜底落到 bot-b。
	if v, ok, _ := f.kvB.KVGet(f.ctx, "botctx", "", "k"); !ok || string(v) != "for-bot-a" {
		t.Fatalf("bot-b 的库 = %q（found=%v），期望 for-bot-a：兜底语义变了", v, ok)
	}
	if _, ok, _ := f.kvA.KVGet(f.ctx, "botctx", "", "k"); ok {
		t.Fatal("键出现在 bot-a 的库：兜底不该按事件所属 Bot 落地（那会让告警永远不触发）")
	}

	out := f.logs.String()
	if !strings.Contains(out, "回落到 Init 时的绑定") || !strings.Contains(out, "bot=bot-b") {
		t.Fatalf("兜底必须留一条带 bot 的告警，实际日志：\n%s", out)
	}
	if !strings.Contains(out, "host.WithBot") {
		t.Fatalf("告警里要给出正确写法（host.WithBot），实际日志：\n%s", out)
	}

	// 只打一次：兜底路径上每个事件都会经过，逐条打会把 journald 淹掉。
	f.handle(t, "fallback")
	if n := strings.Count(f.logs.String(), "回落到 Init 时的绑定"); n != 1 {
		t.Fatalf("告警应每份客户端只打一次，实际 %d 次", n)
	}
}
