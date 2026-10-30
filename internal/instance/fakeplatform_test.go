package instance

// 这组测试用的假平台按 appid 路由：token、取网关地址、WSS 与发消息都靠
// Authorization 头里的 token 反查是哪个 Bot。它是「单一组装接缝」的另一半——
// 隔离类断言（A 的消息不进 B 的库、A 的令牌不出现在 B 的 bot_state）只有在
// 两个 Bot 同时活着时才有意义，所以这里不 mock 容器内部，只假平台侧。

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// tokenFor 是假平台的令牌约定：一个 appid 一个 token，B
// 拿着 A 的 token 就说明串了。
func tokenFor(appID string) string { return "token-" + appID }

// appIDFromAuth 从 `Authorization: QQBot token-<appid>` 反查 Bot。
func appIDFromAuth(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "QQBot token-")
}

// gatewayCall 是一次 GET /gateway/bot 的起止。测试断言的是"不重叠 + 有间隔"，
// 不是具体秒数：默认间隔可配，钉死数值只会让改默认值的人去改测试。
type gatewayCall struct {
	appID string
	start time.Time
	end   time.Time
}

// sendRecord 是一次出站消息：路径能看出场景，token 能看出是哪个 Bot 发的。
type sendRecord struct {
	appID   string
	path    string
	content string
}

type wsConn struct {
	c *websocket.Conn

	mu sync.Mutex
}

func (w *wsConn) write(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return w.c.Write(ctx, websocket.MessageText, payload)
}

type fakePlatform struct {
	t   *testing.T
	srv *httptest.Server

	ctx    context.Context
	cancel context.CancelFunc

	// onGateway 在每次进入 GET /gateway/bot 时回调（测试用来在启动中途
	// 注入关停信号）。必须自行避免阻塞：它就运行在请求路径上。
	onGateway func(appID string)
	// closeFirstWS 非 0 时，每个 appid 的第一条连接在读到手握手帧后
	// 立刻用这个关闭码断开（0 表示保持连接）。
	closeFirstWS int
	// fatalFor 只对列出的 appid 生效：第一轮连接立刻用给定关闭码掐掉（模拟
	// 下架/封禁）。用来验证"一个 Bot 死了、别的还活着"，closeFirstWS 做不到
	// 这件事——它是全体的。
	fatalFor map[string]int

	mu     sync.Mutex
	conns  map[string]*wsConn
	ready  map[string]chan struct{}
	closed map[string]bool
	// gatewayCalls 按请求结束的顺序记录，够断言间隔与串行。
	gatewayCalls   []gatewayCall
	gatewayBusy    int
	gatewayOverlap bool
	sends          []sendRecord
	seq            map[string]int
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakePlatform{
		t:      t,
		ctx:    ctx,
		cancel: cancel,
		conns:  map[string]*wsConn{},
		ready:  map[string]chan struct{}{},
		closed: map[string]bool{},
		seq:    map[string]int{},
	}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.handle))
	t.Cleanup(func() {
		// 先放开读循环再关服务器：WS 是 hijack 的连接，srv.Close 不保证
		// 把它们拉下来，测试收尾就会挂在 handler 上。
		p.cancel()
		p.mu.Lock()
		conns := make([]*wsConn, 0, len(p.conns))
		for _, c := range p.conns {
			conns = append(conns, c)
		}
		p.mu.Unlock()
		for _, c := range conns {
			_ = c.c.Close(websocket.StatusGoingAway, "test done")
		}
		p.srv.Close()
	})
	return p
}

// httpx 返回信任假平台自签证书的出站客户端。
func (p *fakePlatform) httpx() *httpx.Client {
	return httpx.New(httpx.Options{
		Timeout:    5 * time.Second,
		Retries:    1,
		RatePerMin: 6000,
		HTTPClient: p.srv.Client(),
		Log:        slog.New(slog.DiscardHandler),
	})
}

func (p *fakePlatform) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		var body struct {
			AppID string `json:"appId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{
			"access_token": tokenFor(body.AppID),
			"expires_in":   7200,
		})
	case r.URL.Path == "/gateway/bot":
		appID := appIDFromAuth(r)
		p.gatewayEnter(appID)
		defer p.gatewayLeave(appID)
		writeJSON(w, map[string]string{
			"url": "wss" + strings.TrimPrefix(p.srv.URL, "https") + "/ws",
		})
	case r.URL.Path == "/ws":
		p.serveWS(w, r)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		var body struct {
			Content string `json:"content"`
			MsgID   string `json:"msg_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.sends = append(p.sends, sendRecord{
			appID:   appIDFromAuth(r),
			path:    r.URL.Path,
			content: body.Content,
		})
		p.mu.Unlock()
		writeJSON(w, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func (p *fakePlatform) gatewayEnter(appID string) {
	p.mu.Lock()
	hook := p.onGateway
	if p.gatewayBusy > 0 {
		p.gatewayOverlap = true
	}
	p.gatewayBusy++
	p.gatewayCalls = append(p.gatewayCalls, gatewayCall{appID: appID, start: time.Now()})
	p.mu.Unlock()
	if hook != nil {
		hook(appID)
	}
}

func (p *fakePlatform) gatewayLeave(appID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gatewayBusy--
	for i := len(p.gatewayCalls) - 1; i >= 0; i-- {
		// 同一个 Bot 可能重连：从后往前找还没写 end 的那条。
		if p.gatewayCalls[i].appID == appID && p.gatewayCalls[i].end.IsZero() {
			p.gatewayCalls[i].end = time.Now()
			return
		}
	}
}

func (p *fakePlatform) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		p.t.Logf("接受 websocket: %v", err)
		return
	}
	ws := &wsConn{c: conn}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// 心跳间隔取大值：这里只关心"事件到了、回复走了"，不想被心跳干扰。
	if err := ws.write([]byte(`{"op":10,"d":{"heartbeat_interval":45000}}`)); err != nil {
		return
	}
	// 第一条客户端帧是 Identify/Resume，Bot 的身份在载荷的 token 里
	// （握手本身不带 Authorization：平台是按 URL 分配连接的）。
	_, data, err := conn.Read(p.ctx)
	if err != nil {
		return
	}
	var hs struct {
		D struct {
			Token string `json:"token"`
		} `json:"d"`
	}
	if err := json.Unmarshal(data, &hs); err != nil {
		return
	}
	appID := strings.TrimPrefix(hs.D.Token, "QQBot token-")
	if appID == "" || appID == hs.D.Token {
		p.t.Logf("无法从握手识别 Bot: %q", hs.D.Token)
		return
	}
	p.mu.Lock()
	p.conns[appID] = ws
	if p.ready[appID] == nil {
		p.ready[appID] = make(chan struct{})
	}
	ready := p.ready[appID]
	closeCode := 0
	if !p.closed[appID] {
		if code := p.fatalFor[appID]; code != 0 {
			p.closed[appID] = true
			closeCode = code
		} else if p.closeFirstWS != 0 {
			p.closed[appID] = true
			closeCode = p.closeFirstWS
		}
	}
	p.mu.Unlock()
	if closeCode != 0 {
		_ = conn.Close(websocket.StatusCode(closeCode), "测试用关闭码")
		return
	}

	readyFrame, _ := json.Marshal(map[string]any{
		"op": 0, "s": 1, "t": "READY",
		"d": map[string]any{
			"session_id": "session-" + appID,
			"user":       map[string]any{"id": "bot-" + appID},
		},
	})
	if err := ws.write(readyFrame); err != nil {
		return
	}
	close(ready)
	// 读循环只为把连接保持到测试结束；客户端的心跳与关闭都会让它退出。
	for {
		if _, _, err := conn.Read(p.ctx); err != nil {
			return
		}
	}
}

// waitReady 等某个 Bot 的连接走到 READY（超时算失败）。
func (p *fakePlatform) waitReady(t *testing.T, appID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		ch := p.ready[appID]
		p.mu.Unlock()
		if ch != nil {
			select {
			case <-ch:
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s READY 超时", appID)
}

// push 给某个 Bot 推一条 dispatch 事件。seq 每个 Bot 各自递增。
func (p *fakePlatform) push(t *testing.T, appID, typ string, payload any) {
	t.Helper()
	p.mu.Lock()
	ws := p.conns[appID]
	p.seq[appID]++
	seq := p.seq[appID]
	p.mu.Unlock()
	if ws == nil {
		t.Fatalf("%s 没有活着的连接", appID)
	}
	frame, err := json.Marshal(map[string]any{"op": 0, "s": seq, "t": typ, "d": payload})
	if err != nil {
		t.Fatalf("序列化事件: %v", err)
	}
	if err := ws.write(frame); err != nil {
		t.Fatalf("推送事件给 %s: %v", appID, err)
	}
}

// waitSends 等至少 n 条出站消息。
func (p *fakePlatform) waitSends(t *testing.T, n int) []sendRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := append([]sendRecord(nil), p.sends...)
		p.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("等待 %d 条出站消息超时，实际 %d 条: %+v", n, len(p.sends), p.sends)
	return nil
}

// gatewaySnapshot 返回当前的取地址记录与"是否重叠"。
func (p *fakePlatform) gatewaySnapshot() ([]gatewayCall, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := make([]gatewayCall, len(p.gatewayCalls))
	copy(calls, p.gatewayCalls)
	return calls, p.gatewayOverlap
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testLogger 打到 buffer 时用来断言日志；返回 logger 与读取函数。
func testLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	var buf strings.Builder
	l := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(b)
	}), nil))
	return l, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// waitLog 等日志里出现全部 want 子串（最多 10s），返回当时的全文。
//
// 有些结论只有日志能当证据：一个 Bot 被判死之后，它的连接与库都没有"已停止"
// 这种可查询的状态，唯一点名 appid 的那一行就是运维照着找的证据。
func waitLog(t *testing.T, logs func() string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := logs()
		missing := ""
		for _, w := range want {
			if !strings.Contains(out, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("日志里等不到 %q，实际:\n%s", missing, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// stubPlugin 是一个不碰数据库、只回一句话的插件：用来把"消息到了哪个 Bot、
// 回复从哪个 Bot 出去"变成可观察的出站请求。
type stubPlugin struct {
	name string
	seen func(*host.Event)
}

func (s *stubPlugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:       s.name,
		Version:    "0.0.1",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
	}
}

func (s *stubPlugin) Init(context.Context, host.Host) error { return nil }

func (s *stubPlugin) Handle(_ context.Context, ev *host.Event) (*host.Reply, error) {
	if s.seen != nil {
		s.seen(ev)
	}
	return &host.Reply{Content: s.name + ":" + ev.Content}, nil
}

// testConfig 造一份两（或多）Bot 的配置，指向假平台。
//
// 直接构造结构体而不是写 TOML：这里要测的是编排，不是解析。两处必须显式
// 给定的值说明一下——群准入关掉（否则未放行的群会被丢弃），错峰默认置 0
// （测试自己按用例注入间隔）。
func testConfig(t *testing.T, p *fakePlatform, appIDs ...string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	disabled := false
	bots := make([]config.Bot, 0, len(appIDs))
	for _, id := range appIDs {
		bots = append(bots, config.Bot{AppID: id, Secret: "secret-" + id, CertType: config.CertPersonal})
	}
	stagger := config.Duration{}
	return &config.Config{
		Core: config.Core{
			APIBase:  p.srv.URL,
			TokenURL: p.srv.URL + "/token",
			Owners:   []string{"owner-openid"},
		},
		Bots: bots,
		Bot: config.BotOptions{
			Intents:        1 << 25,
			ShardTotal:     1,
			Resume:         false,
			GatewayStagger: &stagger,
		},
		Store: config.Store{Path: dir + "/qqbot.db"},
		Group: config.Group{
			Whitelist: &disabled,
			Blacklist: &disabled,
		},
		HTTP: config.HTTP{
			Timeout:    config.Duration{Duration: 5 * time.Second},
			Retries:    1,
			RatePerMin: 6000,
		},
		Retention: config.Retention{
			Messages: config.Duration{Duration: 168 * time.Hour},
			Events:   config.Duration{Duration: 72 * time.Hour},
			JobRuns:  config.Duration{Duration: 168 * time.Hour},
		},
		Log: config.Log{Level: "error", Format: "text"},
	}
}

// groupMessage 是一条群消息事件体（字段与网关解码表对齐）。
func groupMessage(eventID, groupID, content string) map[string]any {
	return map[string]any{
		"id": eventID,
		"author": map[string]any{
			"id":            "M-" + groupID,
			"member_openid": "M-" + groupID,
			"member_role":   "member",
			"union_openid":  "U-" + groupID,
		},
		"content":      content,
		"group_openid": groupID,
		"message_type": 0,
		"timestamp":    "2026-10-30T12:00:00+08:00",
	}
}

// startContainer 起一个容器，测试结束自动关停。
func startContainer(t *testing.T, cfg *config.Config, p *fakePlatform, opts ...func(*Options)) *Container {
	t.Helper()
	o := Options{
		Config:    cfg,
		Log:       testLogOrDiscard(),
		Outbound:  p.httpx(),
		Handshake: p.srv.Client(),
		LoadPlugins: func(context.Context, config.Bot, *slog.Logger) ([]host.Plugin, error) {
			return []host.Plugin{&stubPlugin{name: "echo"}}, nil
		},
	}
	for _, f := range opts {
		f(&o)
	}
	c, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("启动容器: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	})
	return c
}

// testLogOrDiscard 在 QQBOT_TEST_LOG=1 时把容器日志打到 stderr（调专用）。
func testLogOrDiscard() *slog.Logger {
	if os.Getenv("QQBOT_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// quietLogger 用于不关心日志内容的用例。
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// openStoreForTest 在容器之外再开一份连接，用来验证关停后库里的数据。
func openStoreForTest(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("重新打开 %s: %v", path, err)
	}
	return db
}
