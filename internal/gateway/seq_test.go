package gateway

// P3-11 的守卫：可确认 seq 必须按"处理完成"推进，而不是按"收到帧"推进。
//
// 反例的现场是：10s 落盘节流窗口内崩溃，seq 已经写成 2，而事件 2 还没跑完
// Handler。重启后 Resume 从 2 之后开始，平台不再重发——去重表挡得住重复，
// 挡不住"从未处理"。所以这里在 Handler 阻塞期间直接断言 seq 没有越过它。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// seqPlatform 是第一轮发 READY + 两帧 dispatch、后续轮只记录握手的假平台。
type seqPlatform struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []seqHandshake
}

type seqHandshake struct {
	op  int
	seq int64
}

func newSeqPlatform(t *testing.T) *seqPlatform {
	t.Helper()
	p := &seqPlatform{t: t}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *seqPlatform) handle(w http.ResponseWriter, r *http.Request) {
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

func (p *seqPlatform) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		p.t.Logf("接受 websocket: %v", err)
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

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
	var hs struct {
		Op int `json:"op"`
		D  struct {
			Seq int64 `json:"seq"`
		} `json:"d"`
	}
	if err := json.Unmarshal(data, &hs); err != nil {
		return
	}

	p.mu.Lock()
	p.reqs = append(p.reqs, seqHandshake{op: hs.Op, seq: hs.D.Seq})
	round := len(p.reqs) - 1
	p.mu.Unlock()

	hello := `{"op":0,"s":1,"t":"RESUMED","d":{}}`
	if hs.Op != opResume {
		hello = `{"op":0,"s":1,"t":"READY","d":{"session_id":"seq-session"}}`
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		return
	}

	if round == 0 {
		// 两帧连着下发：读循环处理第一帧时 Handler 会阻塞，第二帧留在连接缓冲里。
		for _, seq := range []int{2, 3} {
			frame := `{"op":0,"s":` + strconv.Itoa(seq) + `,"t":"TEST_EVENT","d":{}}`
			if err := ws.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
	}
	<-ctx.Done()
}

func (p *seqPlatform) handshakes() []seqHandshake {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seqHandshake(nil), p.reqs...)
}

// seqClient 组装一个 Handler 可控的网关客户端。
func (p *seqPlatform) seqClient(db *store.DB, h func(context.Context, *host.Event)) *Client {
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
		Store:      db,
		Log:        testLogger(),
		Handler:    h,
		HTTPClient: p.srv.Client(),
	})
}

func TestConfirmedSeqWaitsForHandler(t *testing.T) {
	db := openTestStore(t)
	p := newSeqPlatform(t)

	entered := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	c := p.seqClient(db, func(ctx context.Context, _ *host.Event) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	waitEntered := func(what string) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("等待 %s 的 Handler 进入超时；握手记录 %+v", what, p.handshakes())
		}
	}

	// 第一帧（seq=2）的 Handler 正卡着：它还没处理完，seq 就只能停在 READY 的 1。
	waitEntered("seq=2")
	if got := c.confirmedSeq(); got != 1 {
		t.Fatalf("Handler 未返回时可确认 seq 已推进到 %d，崩溃后 Resume 会跳过这一条", got)
	}

	// 模拟此刻崩溃：强制落盘一次，落下去的必须是 1，而不是正在处理的 2。
	c.saveState(context.Background(), true)
	if v, ok, err := db.GetState(context.Background(), "seq"); err != nil || !ok {
		t.Fatalf("读取 seq: ok=%v err=%v", ok, err)
	} else if v != "1" {
		t.Fatalf("崩溃前落盘的 seq=%q，期望 1（2 还没处理完）", v)
	}

	// 恢复后 Resume 从正确 seq 起：新客户端读取同一份库，应当带着 1 重新握手。
	revived := New(Options{Store: db, Log: testLogger(), Resume: true})
	revived.loadState(context.Background())
	if got := revived.confirmedSeq(); got != 1 {
		t.Fatalf("恢复后应 Resume 自 seq=1，实际 %d", got)
	}
	if !revived.willResume() {
		t.Fatal("落盘的 session 与 seq 应当允许 Resume")
	}

	// 放行第一帧：这一帧处理完之后才允许推进到 2，第二帧（seq=3）随即阻塞。
	release <- struct{}{}
	waitEntered("seq=3")
	if got := c.confirmedSeq(); got != 2 {
		t.Fatalf("第一帧已处理完，可确认 seq 应为 2，实际 %d", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 在取消后应返回 nil，实际: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("取消后 Run 未返回")
	}
}
