// Package gateway 是 QQ v2 WebSocket 接入层：opcode 状态机、心跳、Resume、事件解码。
//
// 平台约束（ARCHITECTURE.md §1.2 / §9.2）：
//   - 纯出站：/gateway/bot → op10 → op2 Identify → op1 心跳 → op6 Resume；
//   - session 配额 1000/天，重启必须优先 Resume；
//   - 任何 opcode 细节都收敛在本包，插件与核心感知不到 WSS。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

type Options struct {
	API        *api.Client
	Store      *store.DB
	Log        *slog.Logger
	Intents    uint64
	ShardTotal int
	ShardID    int
	Resume     bool
	Handler    func(ctx context.Context, ev *host.Event)
	// RawDump 非空时，每条 dispatch 事件在解码前把原始 JSON 交出去（调试用）。
	RawDump func(typ string, raw []byte)
}

// ErrNotConnected 表示当前没有活着的连接可断——通常是网关正在退避重连。
var ErrNotConnected = errors.New("网关当前未连接")

// errManualReconnect 是内部信号：这次断开是 /reset reconnect 要求的，
// 不是链路故障，所以 Run 不该等退避。
var errManualReconnect = errors.New("手动断开连接")

type Client struct {
	o  Options
	lg *slog.Logger

	mu        sync.Mutex
	sessionID string
	seq       int64
	lastSave  time.Time
	conn      *websocket.Conn
	// manual 由 Reconnect 置位、由读循环消费：连接是被我们自己关掉的。
	manual bool
}

func New(o Options) *Client {
	if o.ShardTotal <= 0 {
		o.ShardTotal = 1
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Client{o: o, lg: o.Log.With("component", "gateway")}
}

// Run 是长期协程：断线后指数退避重连，成功 READY 后退避复位。
func (c *Client) Run(ctx context.Context) {
	if c.o.Handler == nil {
		c.lg.Error("未设置事件处理器，网关不启动")
		return
	}
	c.loadState(ctx)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		ready, err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if ready {
			backoff = time.Second
		}
		// 手动重连（/reset reconnect）：链路本身可能就是好的，
		// 等退避纯属白等，而且会让"我按了重置但十秒没反应"变成用户困惑。
		if errors.Is(err, errManualReconnect) {
			c.lg.Info("已断开当前连接，立即重连（保留 session，走 Resume）")
			backoff = time.Second
			if !sleep(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}
		c.lg.Warn("连接断开，准备重连", "err", err, "backoff", backoff.String())
		if !sleep(ctx, jitter(backoff)) {
			return
		}
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

// Reconnect 主动断开当前连接，由 Run 立刻重连。
//
// 它**不动** session_id / seq，所以重连走 Resume，事件不会丢——这是 R0
// 与 /reload 的分界线：R0 不重启进程、不重读配置，只换一条连接。
// 没有连接时返回 ErrNotConnected，让调用方能把"网关正在退避"说清楚。
func (c *Client) Reconnect() error {
	c.mu.Lock()
	ws := c.conn
	if ws == nil {
		c.mu.Unlock()
		return ErrNotConnected
	}
	c.manual = true
	c.mu.Unlock()

	// 关闭会让阻塞中的 ws.Read 立刻返回错误，读循环据此退出。
	_ = ws.Close(websocket.StatusGoingAway, "manual reconnect")
	return nil
}

// takeManual 消费"本次断开是手动触发的"标记。每次断开都要消费一次，
// 否则上一轮的标记会把之后一次真实的链路故障误报成手动重连。
func (c *Client) takeManual() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.manual
	c.manual = false
	return m
}

func (c *Client) connectOnce(ctx context.Context) (bool, error) {
	gwURL, err := c.o.API.GatewayURL(ctx)
	if err != nil {
		return false, fmt.Errorf("获取网关地址: %w", err)
	}
	token, err := c.o.API.AccessToken(ctx)
	if err != nil {
		return false, fmt.Errorf("获取 token: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dialCtx, gwURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	})
	cancel()
	if err != nil {
		return false, fmt.Errorf("连接网关: %w", err)
	}
	ws.SetReadLimit(1 << 20)

	// 登记当前连接：Reconnect 要能把它关掉。
	// 同时清掉上一轮残留的手动标记——否则一次真实故障会被误报成"手动重连"。
	c.mu.Lock()
	c.conn = ws
	c.manual = false
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = ws.Close(websocket.StatusNormalClosure, "")
		c.saveState(context.Background(), true)
	}()

	helloCtx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	interval, err := c.readHello(helloCtx, ws)
	hcancel()
	if err != nil {
		return false, err
	}
	c.lg.Info("网关握手完成", "heartbeat", interval.String())

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()

	if c.hasSession() && c.o.Resume {
		if err := c.sendResume(ctx, ws, token); err != nil {
			c.lg.Warn("发送 Resume 失败，改为 Identify", "err", err)
			c.clearSession(ctx)
			if err := c.sendIdentify(ctx, ws, token); err != nil {
				return false, err
			}
		} else {
			c.mu.Lock()
			sid, seq := c.sessionID, c.seq
			c.mu.Unlock()
			c.lg.Info("已发送 Resume", "session_id", sid, "seq", seq)
		}
	} else {
		if err := c.sendIdentify(ctx, ws, token); err != nil {
			return false, err
		}
	}

	var ackMu sync.Mutex
	acked := true
	go c.heartbeatLoop(hbCtx, ws, interval, &ackMu, &acked)

	ready := false
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if c.takeManual() {
				return ready, errManualReconnect
			}
			return ready, c.closeReason(err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var p payload
		if err := json.Unmarshal(data, &p); err != nil {
			c.lg.Warn("忽略无法解析的帧", "err", err)
			continue
		}
		if p.S > 0 {
			c.setSeq(ctx, p.S)
		}

		switch p.Op {
		case opDispatch:
			if p.T == "READY" {
				c.onReady(ctx, p.D)
				ready = true
			} else if p.T == "RESUMED" {
				c.lg.Info("Resume 成功，事件不中断")
				ready = true
			} else {
				c.dispatch(ctx, p.T, p.D)
			}
		case opHeartbeat:
			c.sendHeartbeat(ctx, ws)
		case opHeartbeatACK:
			ackMu.Lock()
			acked = true
			ackMu.Unlock()
		case opReconnect:
			return ready, errors.New("服务端要求重连(op7)")
		case opInvalidSession:
			c.lg.Warn("session 失效(op9)，清除后重新 Identify")
			c.clearSession(ctx)
			return ready, errors.New("session 失效(op9)")
		case opHello:
			// 重复 hello：忽略
		default:
			c.lg.Debug("未处理的 opcode", "op", p.Op)
		}
	}
}

func (c *Client) readHello(ctx context.Context, ws *websocket.Conn) (time.Duration, error) {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("等待 hello: %w", err)
	}
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return 0, fmt.Errorf("解析 hello: %w", err)
	}
	if p.Op != opHello {
		return 0, fmt.Errorf("首帧不是 op10 hello，收到 op%d", p.Op)
	}
	var d struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(p.D, &d); err != nil || d.HeartbeatInterval <= 0 {
		return 0, fmt.Errorf("hello 缺少心跳间隔")
	}
	return time.Duration(d.HeartbeatInterval) * time.Millisecond, nil
}

func (c *Client) sendIdentify(ctx context.Context, ws *websocket.Conn, token string) error {
	body := map[string]any{
		"op": opIdentify,
		"d": map[string]any{
			"token":   "QQBot " + token,
			"intents": c.o.Intents,
			"shard":   [2]int{c.o.ShardID, c.o.ShardTotal},
			"properties": map[string]string{
				"$os":      "linux",
				"$browser": "qqbot",
				"$device":  "qqbot",
			},
		},
	}
	if err := ws.Write(ctx, websocket.MessageText, mustJSON(body)); err != nil {
		return fmt.Errorf("发送 Identify: %w", err)
	}
	c.lg.Info("已发送 Identify", "shard", fmt.Sprintf("%d/%d", c.o.ShardID, c.o.ShardTotal), "intents", c.o.Intents)
	return nil
}

func (c *Client) sendResume(ctx context.Context, ws *websocket.Conn, token string) error {
	body := map[string]any{
		"op": opResume,
		"d": map[string]any{
			"token":      "QQBot " + token,
			"session_id": c.sessionID,
			"seq":        c.seq,
		},
	}
	return ws.Write(ctx, websocket.MessageText, mustJSON(body))
}

func (c *Client) heartbeatLoop(ctx context.Context, ws *websocket.Conn, interval time.Duration, ackMu *sync.Mutex, acked *bool) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ackMu.Lock()
			ok := *acked
			*acked = false
			ackMu.Unlock()
			if !ok {
				// 上一轮心跳未 ack：连接已半死，主动断开触发重连
				c.lg.Warn("心跳未收到 ACK，断开连接")
				_ = ws.Close(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
			c.sendHeartbeat(ctx, ws)
		}
	}
}

func (c *Client) sendHeartbeat(ctx context.Context, ws *websocket.Conn) {
	c.mu.Lock()
	seq := c.seq
	c.mu.Unlock()

	var d any
	if seq > 0 {
		d = seq
	}
	if err := ws.Write(ctx, websocket.MessageText, mustJSON(map[string]any{"op": opHeartbeat, "d": d})); err != nil {
		c.lg.Debug("心跳发送失败", "err", err)
	}
}

func (c *Client) onReady(ctx context.Context, raw json.RawMessage) {
	var d struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.SessionID == "" {
		c.lg.Warn("READY 缺少 session_id", "err", err)
		return
	}
	c.mu.Lock()
	c.sessionID = d.SessionID
	c.mu.Unlock()
	c.saveState(ctx, true)
	c.lg.Info("网关就绪", "session_id", d.SessionID)
}

func (c *Client) dispatch(ctx context.Context, t string, raw json.RawMessage) {
	if c.o.RawDump != nil {
		c.o.RawDump(t, raw)
	}
	ev, err := Decode(t, raw)
	if err != nil {
		c.lg.Warn("忽略无法解码的事件", "t", t, "err", err)
		return
	}
	if ev.ID == "" {
		ev.ID = stableEventID(t, raw)
	}
	c.o.Handler(ctx, ev)
}

func (c *Client) hasSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID != ""
}

func (c *Client) clearSession(ctx context.Context) {
	c.mu.Lock()
	c.sessionID = ""
	c.seq = 0
	c.mu.Unlock()
	_ = c.o.Store.DeleteState(ctx, "session_id")
	_ = c.o.Store.DeleteState(ctx, "seq")
}

func (c *Client) setSeq(ctx context.Context, seq int64) {
	c.mu.Lock()
	c.seq = seq
	c.mu.Unlock()
	c.saveState(ctx, false)
}

func (c *Client) loadState(ctx context.Context) {
	sid, ok, err := c.o.Store.GetState(ctx, "session_id")
	if err != nil {
		c.lg.Warn("读取 session 失败", "err", err)
		return
	}
	if ok {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	if v, ok, _ := c.o.Store.GetState(ctx, "seq"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.mu.Lock()
			c.seq = n
			c.mu.Unlock()
		}
	}
}

func (c *Client) saveState(ctx context.Context, force bool) {
	c.mu.Lock()
	seq := c.seq
	sid := c.sessionID
	last := c.lastSave
	if !force && time.Since(last) < 10*time.Second {
		c.mu.Unlock()
		return
	}
	c.lastSave = time.Now()
	c.mu.Unlock()

	if sid != "" {
		if err := c.o.Store.PutState(ctx, "session_id", sid); err != nil {
			c.lg.Warn("持久化 session 失败", "err", err)
		}
	}
	if seq > 0 {
		if err := c.o.Store.PutState(ctx, "seq", strconv.FormatInt(seq, 10)); err != nil {
			c.lg.Warn("持久化 seq 失败", "err", err)
		}
	}
}

func (c *Client) closeReason(err error) error {
	if err == nil {
		return nil
	}
	code := websocket.CloseStatus(err)
	switch code {
	case 4006, 4009:
		return fmt.Errorf("session 不可用(%d): %w", code, err)
	case 4914, 4915:
		return fmt.Errorf("intents 非法(%d)，检查 bot.intents 配置: %w", code, err)
	}
	return err
}

func stableEventID(t string, raw json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	parts := []string{t}
	for _, k := range []string{"group_openid", "op_member_openid", "timestamp", "id"} {
		if v, ok := m[k]; ok {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, ":")
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"op":1,"d":null}`)
	}
	return b
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	return d/2 + time.Duration(rand.Int63n(int64(d)))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
