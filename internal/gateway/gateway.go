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
)

// stateStore 是网关需要的全部持久化能力：session_id 与 seq 两个键。
// 用窄接口而不是 *store.DB，是为了让"删库失败"这类现场在单测里可造。
type stateStore interface {
	GetState(ctx context.Context, key string) (string, bool, error)
	PutState(ctx context.Context, key, value string) error
	DeleteState(ctx context.Context, key string) error
}

type Options struct {
	API        *api.Client
	Store      stateStore
	Log        *slog.Logger
	Intents    uint64
	ShardTotal int
	ShardID    int
	Resume     bool
	Handler    func(ctx context.Context, ev *host.Event)
	// RawDump 非空时，每条 dispatch 事件在解码前把原始 JSON 交出去（调试用）。
	RawDump func(typ string, raw []byte)

	// HTTPClient 注入 WSS 握手的传输层，nil 时用 15s 超时的默认客户端。
	// 与 httpx 的 HTTPClient 同一个用途：自签证书、将来接代理时不必改构造函数。
	HTTPClient *http.Client
}

// ErrNotConnected 表示当前没有活着的连接可断——通常是网关正在退避重连。
var ErrNotConnected = errors.New("网关当前未连接")

// errManualReconnect 是内部信号：这次断开是 /reset reconnect 要求的，
// 不是链路故障，所以 Run 不该等退避。
var errManualReconnect = errors.New("手动断开连接")

// errHandshakeTimeout 表示连接建起来了，但握手没走完：发过 Identify/Resume，
// 在 defaultHandshakeTimeout 内没收到 READY/RESUMED。
//
// 它必须与普通读错误区分开——两者在日志里都长成一条 EOF，而现场的含义完全不同：
// 前者是"这条连接从没可用过"，后者是"用过的连接断了"。
var errHandshakeTimeout = errors.New("握手超时")

type Client struct {
	o  Options
	lg *slog.Logger

	// invalidateToken 作废缓存 token（*api.Client.InvalidateToken 的方法值）。
	// 抽成字段只为了让"4004 会作废 token"这条接线可测；Options.API 为 nil 时留空。
	invalidateToken func()
	// hc 只用于 WSS 握手。
	hc *http.Client

	// handshakeTimeout 是 Identify/Resume 之后等 READY/RESUMED 的上限。
	// 做成字段只为让测试注入短超时，正常路径永远是 defaultHandshakeTimeout。
	handshakeTimeout time.Duration

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
	c := &Client{o: o, lg: o.Log.With("component", "gateway"), handshakeTimeout: defaultHandshakeTimeout}
	if o.API != nil {
		c.invalidateToken = o.API.InvalidateToken
	}
	c.hc = o.HTTPClient
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 15 * time.Second}
	}
	return c
}

// Run 是长期协程：断线后指数退避重连；只有连接稳定 READY 过才复位退避。
//
// 返回非 nil 只发生在永久性错误（*FatalError：下架/封禁/intents 无权限/分片参数
// 错误）——这时再连也不会好，必须让调用方看见原因并决定进程去向，不能静默重试。
func (c *Client) Run(ctx context.Context) error {
	if c.o.Handler == nil {
		c.lg.Error("未设置事件处理器，网关不启动")
		return nil
	}
	c.loadState(ctx)

	b := &backoff{d: minBackoff}
	for {
		if ctx.Err() != nil {
			return nil
		}
		readyFor, err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}

		var fatal *FatalError
		if errors.As(err, &fatal) {
			return fatal
		}

		b.note(readyFor)
		// 手动重连（/reset reconnect）：链路本身可能就是好的，
		// 等退避纯属白等，而且会让"我按了重置但十秒没反应"变成用户困惑。
		if errors.Is(err, errManualReconnect) {
			c.lg.Info("已断开当前连接，立即重连（保留 session，走 Resume）")
			b.reset()
			if !sleep(ctx, 100*time.Millisecond) {
				return nil
			}
			continue
		}
		wait := b.next(closeDelay(err))
		c.lg.Warn("连接断开，准备重连", "err", err, "backoff", wait.String())
		if !sleep(ctx, wait) {
			return nil
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

// connectOnce 建立一条连接并跑到它断开为止。
//
// readyFor 是"READY/RESUMED 之后这条连接还活着多久"：用来判断连接是否稳定过，
// 从而决定 Run 里的退避要不要复位。没收到 READY/RESUMED 时为 0。
func (c *Client) connectOnce(ctx context.Context) (readyFor time.Duration, err error) {
	var readySince time.Time
	defer func() {
		if !readySince.IsZero() {
			readyFor = time.Since(readySince)
		}
	}()

	gwURL, err := c.o.API.GatewayURL(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取网关地址: %w", err)
	}
	token, err := c.o.API.AccessToken(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取 token: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dialCtx, gwURL, &websocket.DialOptions{HTTPClient: c.hc})
	cancel()
	if err != nil {
		return 0, fmt.Errorf("连接网关: %w", err)
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
		return 0, err
	}
	c.lg.Info("网关握手完成", "heartbeat", interval.String())

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()

	// hsOp 记录这一轮实际发出的握手请求：超时日志要能看出是 Identify 还是
	// Resume 没被回应，两者的处置方向不同。
	hsOp := opIdentify
	if c.willResume() {
		if err := c.sendResume(ctx, ws, token); err != nil {
			c.lg.Warn("发送 Resume 失败，改为 Identify", "err", err)
			c.clearSession(ctx)
			if err := c.sendIdentify(ctx, ws, token); err != nil {
				return 0, err
			}
		} else {
			hsOp = opResume
			c.mu.Lock()
			sid, seq := c.sessionID, c.seq
			c.mu.Unlock()
			c.lg.Info("已发送 Resume", "session_id", sid, "seq", seq)
		}
	} else {
		if err := c.sendIdentify(ctx, ws, token); err != nil {
			return 0, err
		}
	}

	// 握手定时器：Identify/Resume 只是"请求接入"，READY/RESUMED 才是"接入成功"。
	// 缺了这一段，平台接受 WS、心跳照常回 ACK、但 READY 帧丢失（或 Identify
	// 未被授权）时，下面的读循环会永远阻塞——Run 不返回、不重连，表现是
	// "进程在线、心跳正常、一条消息都不回"，而日志里连异常都没有。
	hsCtx, hsCancel := context.WithCancel(ctx)
	defer hsCancel()
	hs := &handshakeState{}
	go c.watchHandshake(hsCtx, ws, hsOp, hs)

	var ackMu sync.Mutex
	acked := true
	go c.heartbeatLoop(hbCtx, ws, interval, &ackMu, &acked)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if c.takeManual() {
				return 0, errManualReconnect
			}
			if hs.aborted() {
				// 不再往下走 onReadError：这条连接从没 READY 过，关闭码是
				// 我们自己发的，拿它做 session 迁移没有意义。
				return 0, fmt.Errorf("%w（%s 发出后 %s 内未收到 READY/RESUMED）: %v",
					errHandshakeTimeout, opcodeName(hsOp), c.handshakeTimeout, err)
			}
			// 关闭码在这里被翻译成"下一步做什么"：session 的迁移必须发生在
			// 读错误出口，否则下一轮 connectOnce 还会做出同样的错误选择。
			return 0, c.onReadError(ctx, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var p payload
		if err := json.Unmarshal(data, &p); err != nil {
			c.lg.Warn("忽略无法解析的帧", "err", err)
			continue
		}
		// 可确认 seq 的推进时机（P3-11）：非 dispatch 帧没有 Handler，收到即可
		// 确认；dispatch 帧必须等 Handler 返回，见下面的 setSeq。
		if p.Op != opDispatch && p.S > 0 {
			c.setSeq(ctx, p.S)
		}

		switch p.Op {
		case opDispatch:
			if p.T == "READY" {
				hs.markReady()
				hsCancel()
				c.onReady(ctx, p.D)
				readySince = time.Now()
			} else if p.T == "RESUMED" {
				hs.markReady()
				hsCancel()
				c.lg.Info("Resume 成功，事件不中断")
				readySince = time.Now()
			} else {
				c.dispatch(ctx, p.T, p.D)
			}
			// 走到这里说明这一帧已经处理完（Handler 已返回，或事件被解码丢弃）。
			// 提前置位会让崩溃落在"seq 已落盘、事件未处理"的窗口里：下次 Resume
			// 从已确认 seq 之后开始，那一条就永久丢了（P3-11）。落盘仍有 10s
			// 节流，但落下去的 seq 永远是"已处理完的最大连续 seq"。
			if p.S > 0 {
				c.setSeq(ctx, p.S)
			}
		case opHeartbeat:
			c.sendHeartbeat(ctx, ws)
		case opHeartbeatACK:
			ackMu.Lock()
			acked = true
			ackMu.Unlock()
		case opReconnect:
			return 0, errors.New("服务端要求重连(op7)")
		case opInvalidSession:
			c.lg.Warn("session 失效(op9)，清除后重新 Identify")
			c.clearSession(ctx)
			return 0, errors.New("session 失效(op9)")
		case opHello:
			// 重复 hello：忽略
		default:
			c.lg.Debug("未处理的 opcode", "op", p.Op)
		}
	}
}

// handshakeState 记录这一轮握手的结果：读循环的错误出口靠它区分
// 「握手超时」与「连接断开」——两者在日志里都只是一条 EOF。
type handshakeState struct {
	mu    sync.Mutex
	ready bool
	timed bool
}

// markReady 在 READY/RESUMED 到达时调用。定时器随后会自行退出。
func (h *handshakeState) markReady() {
	h.mu.Lock()
	h.ready = true
	h.mu.Unlock()
}

// aborted 报告"这条连接是因为握手没完成、被我们自己断掉的"。
// 定时器和 READY 可能几乎同时到达：只要 READY 已经被处理过就不算超时，
// 否则日志会把一条刚接通的连接说成握手失败。
func (h *handshakeState) aborted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.timed && !h.ready
}

// watchHandshake 是握手截止：窗口内没等到 READY/RESUMED 就主动关连接，
// 让读循环立刻从 ws.Read 返回，Run 随后走常规重连。
//
// 它不能省成"心跳没 ACK 才断"：心跳只证明链路活着，不证明这次接入成功。
func (c *Client) watchHandshake(ctx context.Context, ws *websocket.Conn, op int, hs *handshakeState) {
	t := time.NewTimer(c.handshakeTimeout)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return
	case <-t.C:
		hs.mu.Lock()
		if hs.ready {
			hs.mu.Unlock()
			return
		}
		hs.timed = true
		hs.mu.Unlock()

		c.lg.Error("握手超时：已发出握手但未收到 READY/RESUMED，主动断开重连",
			"op", opcodeName(op), "opcode", op,
			"timeout", c.handshakeTimeout.String(),
			"note", "心跳 ACK 正常不代表这条连接可用")
		// 用 CloseNow 而不是 Close：宽限期里对端大概率也在傻等（平台侧
		// 就是把 READY 弄丢的那一方），而 Close 要等它回 close 帧——最多 5s，
		// 正好把一次干净的快速重连拖成"连上就卡"。
		_ = ws.CloseNow()
	}
}

// opcodeName 只为人读日志：握手超时要看得出是哪一步没完成。
func opcodeName(op int) string {
	switch op {
	case opIdentify:
		return "Identify"
	case opResume:
		return "Resume"
	default:
		return fmt.Sprintf("op%d", op)
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

// confirmedSeq 返回当前可确认（已处理完）的最大连续 seq。
//
// 它是落盘与 Resume 的唯一来源：c.seq 只在帧处理完之后被 setSeq 推进，
// 所以任何被持久化的 seq 都意味着它之前的 dispatch 事件都已经交给 Handler 并返回。
func (c *Client) confirmedSeq() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// willResume 决定这一轮握手走 Resume 还是 Identify。
// 它只读当下的 session 状态，所以"清掉 session"与"下一轮 Identify"是同一件事。
func (c *Client) willResume() bool {
	return c.o.Resume && c.hasSession()
}

func (c *Client) clearSession(ctx context.Context) {
	c.mu.Lock()
	c.sessionID = ""
	c.seq = 0
	c.mu.Unlock()
	// 删库失败只告警，但必须留痕：内存里的 session 已经清掉，若库里还留着
	// session_id，下次启动会继续 Resume 一个死 session——P1-1 在持久化层复发，
	// 而现场日志里只会看到"怎么又在 Resume"。
	for _, k := range [...]string{"session_id", "seq"} {
		if err := c.o.Store.DeleteState(ctx, k); err != nil {
			c.lg.Warn("清理 session 持久化失败，重启后可能仍 Resume 旧 session",
				"key", k, "err", err)
		}
	}
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

// onReadError 是读循环的唯一错误出口：按关闭码做状态迁移，并给出 Run 需要的
// "要不要再连、最短等多久"。
//
// 关键点是**在这里**改 session 状态——只在日志里换文案，下一轮 connectOnce
// 仍会按旧状态发 Resume，于是一个死 session 会被永远 Resume 下去。
func (c *Client) onReadError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	code := int(websocket.CloseStatus(err))
	d := classifyClose(code)

	switch d.action {
	case actionReIdentify:
		c.lg.Warn("关闭码要求重新 Identify，清除 session 后重连", "code", code, "reason", d.reason)
		c.clearSession(ctx)
	case actionResume:
		// 官方明确 4009 应 Resume：这里保留 session_id / seq，事件不中断。
		c.lg.Info("连接已过期，保留 session 重连（走 Resume）", "code", code, "reason", d.reason)
	case actionInvalidToken:
		c.lg.Warn("token 已失效，作废后重连", "code", code, "reason", d.reason)
		if c.invalidateToken != nil {
			c.invalidateToken()
		}
	case actionRateLimited:
		c.lg.Warn("平台要求降频，本轮会等到频控窗口过去再连",
			"code", code, "reason", d.reason, "delay", d.delay.String())
	case actionFatal:
		c.lg.Error("网关遇到永久性错误，停止重连", "code", code, "reason", d.reason)
		return &FatalError{Code: code, Reason: d.reason, Err: err}
	default:
		// 未识别的关闭码/无关闭帧：保持原始错误文本，按普通链路故障退避重连，
		// session 原样保留（能不能 Resume 让平台自己回答）。
		return err
	}
	return &closeError{delay: d.delay, reason: d.reason, err: err}
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
