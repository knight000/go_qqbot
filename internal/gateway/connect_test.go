package gateway

// 这组测试不 mock 网关内部：起一个假平台（HTTP + WSS），让它按剧本用指定关闭码断开，
// 然后观察**下一轮握手实际发出的 opcode**。关闭码 → 动作的映射只有这样才能被守住：
// 「4006 不清 session」改回去时，第二轮会重新发 Resume(6) 而不是 Identify(2)。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
)

// fakePlatform 是一个能"按剧本关闭"的假平台。
//
// codes[i] 是第 i 轮连接（0 基）在收到首帧后用来断开的关闭码；0 表示保持连接。
// 它记录每轮的首个 opcode，让"下一轮走哪条路"成为可观测行为，
// 而不是去读客户端的私有字段。
type fakePlatform struct {
	t     *testing.T
	srv   *httptest.Server
	codes []int

	mu  sync.Mutex
	ops []int
}

func newFakePlatform(t *testing.T, codes ...int) *fakePlatform {
	t.Helper()
	p := &fakePlatform{t: t, codes: codes}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePlatform) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		writeJSON(w, map[string]any{"access_token": "fake-token", "expires_in": 7200})
	case "/gateway/bot":
		writeJSON(w, map[string]string{
			"url": "wss" + strings.TrimPrefix(p.srv.URL, "https") + "/ws",
		})
	case "/ws":
		p.serveWS(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *fakePlatform) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		p.t.Logf("接受 websocket: %v", err)
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	// 心跳间隔取大值：这里的测试只关心握手走哪条路，不想被心跳干扰。
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText,
		[]byte(`{"op":10,"d":{"heartbeat_interval":45000}}`)); err != nil {
		return
	}

	_, data, err := ws.Read(ctx)
	if err != nil {
		return
	}
	var first struct {
		Op int `json:"op"`
	}
	if err := json.Unmarshal(data, &first); err != nil {
		return
	}

	p.mu.Lock()
	p.ops = append(p.ops, first.Op)
	round := len(p.ops) - 1
	code := 0
	if round < len(p.codes) {
		code = p.codes[round]
	}
	p.mu.Unlock()

	if code == 0 {
		<-ctx.Done() // 保持连接，等测试收尾
		return
	}
	_ = ws.Close(websocket.StatusCode(code), "测试用关闭码")
}

func (p *fakePlatform) opList() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.ops...)
}

// waitOps 等平台收到第 n 轮握手（超时算测试失败）。
func (p *fakePlatform) waitOps(n int) {
	p.t.Helper()
	p.waitOpsWithin(n, 20*time.Second)
}

// waitOpsWithin 是带期限的版本：握手超时的用例必须证明"重连发生在平台自己
// 断线之前"。fake 平台的服务端 15s 后会自行超时，踩着那个点重连也能通过
// ——把定时器删掉照样绿，测试就白写了。
func (p *fakePlatform) waitOpsWithin(n int, d time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := len(p.ops)
		p.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("等待第 %d 轮握手超时（%v），实际收到 %v", n, d, p.opList())
}

// client 组装一个指向假平台的网关：Store 是真的（要验证持久化 session 的去留），
// API 也走真实的 api 包，只是 BaseURL 指向假平台。
func (p *fakePlatform) client(db *store.DB, resume bool) *Client {
	// httpx 只允许 https，所以假平台用 TLS；srv.Client() 自带信任它的证书。
	hc := httpx.New(httpx.Options{Timeout: 5 * time.Second, HTTPClient: p.srv.Client()})
	return New(Options{
		API: api.New(api.Options{
			HTTP:     hc,
			BaseURL:  p.srv.URL,
			TokenURL: p.srv.URL + "/token",
			AppID:    "app",
			Secret:   "secret",
			State:    db,
			Log:      testLogger(),
		}),
		Store:   db,
		Log:     testLogger(),
		Resume:  resume,
		Handler: func(context.Context, *host.Event) {},
		// WSS 握手走同一份信任测试证书的客户端。
		HTTPClient: p.srv.Client(),
	})
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return db
}

// seedSession 模拟"上次进程正常退出、session 已落盘"的启动状态。
func seedSession(t *testing.T, db *store.DB, sid string, seq int64) {
	t.Helper()
	ctx := context.Background()
	if err := db.PutState(ctx, "session_id", sid); err != nil {
		t.Fatalf("写入 session_id: %v", err)
	}
	if err := db.PutState(ctx, "seq", strconv.FormatInt(seq, 10)); err != nil {
		t.Fatalf("写入 seq: %v", err)
	}
}

// 4006（无效 session id）必须清掉持久化 session，下一轮重新 Identify。
// 改回"只改日志文案、不清 session"时，第二轮会再发 Resume —— 也就是线上
// 那条"每 60s 一条 4006、永不收事件"的死循环。
func TestReIdentifyCloseCodeClearsSession(t *testing.T) {
	db := openTestStore(t)
	seedSession(t, db, "dead-session", 42)

	p := newFakePlatform(t, 4006)
	c := p.client(db, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	p.waitOps(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run 在取消后应返回 nil，实际: %v", err)
	}

	if got := p.opList(); len(got) < 2 || got[0] != opResume || got[1] != opIdentify {
		t.Fatalf("4006 之后第二轮必须 Identify(op2)，实际 opcode 序列: %v", got)
	}
	// 内存清了还不够：进程重启会从 bot_state 读回同一个死 session。
	if v, ok, _ := db.GetState(context.Background(), "session_id"); ok {
		t.Fatalf("4006 之后 session_id 必须从 bot_state 删除，实际仍是 %q", v)
	}
	if v, ok, _ := db.GetState(context.Background(), "seq"); ok {
		t.Fatalf("4006 之后 seq 必须从 bot_state 删除，实际仍是 %q", v)
	}
}

// 4009（连接过期）恰恰相反：保留 session，下一轮继续 Resume。
// 这条不是新行为，但和 4006/4007 只差一个数字，必须有测试挡住"顺手统一成清 session"。
func TestResumeCloseCodeKeepsSession(t *testing.T) {
	db := openTestStore(t)
	seedSession(t, db, "live-session", 42)

	p := newFakePlatform(t, 4009)
	c := p.client(db, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	p.waitOps(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run 在取消后应返回 nil，实际: %v", err)
	}

	if got := p.opList(); len(got) < 2 || got[0] != opResume || got[1] != opResume {
		t.Fatalf("4009 之后第二轮仍应 Resume(op6)，实际 opcode 序列: %v", got)
	}
	if v, ok, _ := db.GetState(context.Background(), "session_id"); !ok || v != "live-session" {
		t.Fatalf("4009 不应清除 session_id，实际 ok=%v v=%q", ok, v)
	}
}

// 4004（token 无效）要作废缓存 token，否则下一轮 Identify 还是拿着同一个坏 token。
// 作废本身由 api 包实现（#22 会连持久化一起删），网关只负责接线。
func TestInvalidTokenCloseCodeInvalidatesToken(t *testing.T) {
	db := openTestStore(t)

	p := newFakePlatform(t, 4004)
	c := p.client(db, false)

	invalidated := make(chan struct{}, 1)
	c.invalidateToken = func() {
		select {
		case invalidated <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case <-invalidated:
	case <-time.After(20 * time.Second):
		t.Fatalf("4004 之后应作废缓存 token，实际没有调用；opcode 序列: %v", p.opList())
	}
}

// 平台回了 hello、心跳也照常 ACK，但 READY 帧永远不来 —— 这是最难排查的故障：
// 进程在线、心跳正常、一条消息都不回，日志里只有一条 EOF。
// 客户端必须在握手窗口到期后**自己**断开并重连（服务端在这里从不主动断）。
func TestHandshakeTimeoutClosesAndReconnects(t *testing.T) {
	db := openTestStore(t)

	// codes 为空 = 每一轮都只回 hello，然后一直保持连接、永不发 READY。
	p := newFakePlatform(t)
	c := p.client(db, false)
	c.handshakeTimeout = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 第二轮握手只可能来自"客户端自己超时关掉第一轮"：fake 平台 15s 后才会
	// 自行超时，所以这里必须给一个远小于 15s 的窗口。
	p.waitOpsWithin(2, 5*time.Second)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 在取消后应返回 nil，实际: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("握手超时后 Run 未返回，说明仍在无限读一条永远收不到 READY 的连接")
	}

	if ops := p.opList(); len(ops) < 2 || ops[0] != opIdentify || ops[1] != opIdentify {
		t.Fatalf("握手超时后应重新 Identify(op2)，实际 opcode 序列: %v", ops)
	}
}

// 握手等待期间收到关停信号必须立刻返回：关停时卡住的不是"一次 Read"，
// 而是握手定时器那一整段（默认 30s）。剩余协程也要跟着连接一起退出，
// 否则每次重启/重载都会攒下一个握着 *websocket.Conn 的定时器。
func TestRunCancelDuringHandshakeReturnsPromptly(t *testing.T) {
	db := openTestStore(t)

	p := newFakePlatform(t) // 只回 hello、永不 READY：客户端一直停在握手等待
	c := p.client(db, false)
	// 故意取远大于测试时长的超时：这里要证明的是"取消能打断它"，
	// 不是"等它自己超时"。
	c.handshakeTimeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	p.waitOps(1)
	// 此刻除了 Run 自己，还挂着手握定时器与心跳两个协程。
	during := runtime.NumGoroutine()

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 在取消后应返回 nil，实际: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("取消后 Run 未返回，仍卡在握手等待上（握手超时设为 %v）", c.handshakeTimeout)
	}
	if spent := time.Since(start); spent > 2*time.Second {
		t.Fatalf("取消后应立刻返回，实际用了 %v", spent)
	}

	// 握手定时器 + 心跳 + Run 三个协程都应退出；留一个就是漏。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= during-3 {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("取消后协程未收敛：握手期间 %d 个，现在 %d 个（应少 3 个）", during, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 4915（机器人已封禁）是永久错误：Run 必须把可判定的错误交给调用方并停止重连，
// 而不是每 60s 静默重连一次。文案也不能再把人往 intents 上引。
func TestFatalCloseCodeStopsReconnecting(t *testing.T) {
	db := openTestStore(t)

	// 第二轮不该发生：给一个会让"还在重连"这件事暴露出来的关闭码。
	p := newFakePlatform(t, 4915, 4006)
	c := p.client(db, false)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := c.Run(ctx)
	var fatal *FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("4915 应返回 *FatalError，实际: %v", err)
	}
	if fatal.Code != 4915 {
		t.Fatalf("FatalError.Code 应为 4915，实际 %d", fatal.Code)
	}
	if strings.Contains(fatal.Reason, "intents") {
		t.Fatalf("4915 是封禁，文案不该再提 intents: %q", fatal.Reason)
	}
	if ops := p.opList(); len(ops) != 1 {
		t.Fatalf("永久错误之后不应重连，实际握手 %v", ops)
	}
}
