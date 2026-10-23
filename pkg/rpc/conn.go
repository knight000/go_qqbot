package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrConnClosed 表示连接已关闭（对端退出、读到半包、自己调了 Close）。
	ErrConnClosed = errors.New("rpc: 连接已关闭")
	// ErrMethodNotFound 表示对端没注册这个方法。可选接口（Start/Close/RunJob）
	// 的缺席就是这样暴露的，所以它必须可判别、不能塌成一条超时。
	ErrMethodNotFound = errors.New("rpc: 方法未注册")
	// ErrHandlerPanic 表示对端 handler panic 后被 recover。
	ErrHandlerPanic = errors.New("rpc: handler panic")
)

// Request 是 handler 收到的入参。
type Request struct {
	Params json.RawMessage
	Body   []byte
}

// Response 是 handler 的返回值。Result 为 nil 时线上是 `{}`（成功、无返回值）。
type Response struct {
	Result any
	Body   []byte
}

// Handler 处理一个方法调用。ctx 带对端传来的 deadline。
type Handler func(ctx context.Context, req Request) (Response, error)

// Result 是一次成功调用的返回值。
type Result struct {
	raw  json.RawMessage
	body []byte
}

// Decode 把结果解进 v。数字一律 UseNumber 后规范化（见 wire.go 的 decodeJSON）。
func (r Result) Decode(v any) error {
	if len(r.raw) == 0 {
		r.raw = json.RawMessage("{}")
	}
	return decodeJSON(r.raw, v)
}

// Body 返回这一帧的正文槽（图片字节）。
func (r Result) Body() []byte { return r.body }

// Options 是构造 Conn 的可选项。
type Options struct {
	// Registry 是错误码表。两端必须传入**同一张**表（至少码值一致），
	// 否则对端还原不出本地哨兵。零值等价于 NewRegistry()。
	Registry *Registry
	// Logger 只用于本进程内的排障（panic 堆栈、未知 id 的应答）。
	Logger *slog.Logger
}

// Conn 是一条双向多路复用的 RPC 连接。
//
// 它同时是客户端与服务端：核心与插件都用同一个类型，只是注册的方法不同。
// 这正是「Init 里可以调 Host」的前提——读循环在自己的协程里跑，一个 handler
// 等应答时，同一连接上的其他请求照常被分发。
type Conn struct {
	r    io.Reader
	w    io.Writer
	reg  *Registry
	log  *slog.Logger
	once sync.Once
	done chan struct{}

	// 写侧串行：一帧必须原子落进管道，否则读侧永久失步。
	wmu      sync.Mutex
	writeErr error

	mu      sync.Mutex
	closed  bool
	pending map[uint64]chan delivery
	runErr  error

	hmu      sync.RWMutex
	handlers map[string]Handler

	nextID atomic.Uint64
}

// delivery 是一次调用的结局：对端给的结果，或一个已经还原成本地形态的错误。
// 名字刻意与调用方看到的 Result 区分：一个是通道内部的中转，一个是
// 「这次调用返回了什么」。
type delivery struct {
	hdr  header
	body []byte
	err  error
}

// header 同时是请求头与应答头：请求带 Method，应答带 Result / Error。
// 用一个类型是为了让分发只看一个字段（Method 是否为空），不必先探两种结构。
type header struct {
	ID       uint64          `json:"id"`
	Method   string          `json:"method,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Deadline int64           `json:"deadline,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// NewConn 用一对读写端构造连接。
//
// 传 io.Reader / io.Writer 而不是 ReadWriteCloser：核心侧是一对管道、
// 插件侧是 stdin/stdout，没有共同的接口；关闭时各自实现 io.Closer 的会被关。
func NewConn(r io.Reader, w io.Writer, o Options) *Conn {
	c := &Conn{
		r:        r,
		w:        w,
		reg:      o.Registry,
		log:      o.Logger,
		done:     make(chan struct{}),
		pending:  make(map[uint64]chan delivery),
		handlers: make(map[string]Handler),
	}
	if c.reg == nil {
		c.reg = NewRegistry()
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// Done 在连接关闭后关闭，供调用方等待收摊。
func (c *Conn) Done() <-chan struct{} { return c.done }

// Handle 注册一个方法。必须在 Run 之前调用；重复注册返回错误而不是覆盖
// ——「谁把 host.reply 抢了」是必须立刻暴露的那类问题。
func (c *Conn) Handle(method string, h Handler) error {
	if method == "" {
		return errors.New("rpc: 方法名为空")
	}
	if h == nil {
		return fmt.Errorf("rpc: 方法 %s 的 handler 为 nil", method)
	}
	c.hmu.Lock()
	defer c.hmu.Unlock()
	if _, dup := c.handlers[method]; dup {
		return fmt.Errorf("rpc: 方法 %s 重复注册", method)
	}
	c.handlers[method] = h
	return nil
}

// Handles 报告某方法是否已注册，供装配期断言用（插件是否实现 Closer 之类）。
func (c *Conn) Handles(method string) bool {
	c.hmu.RLock()
	defer c.hmu.RUnlock()
	_, ok := c.handlers[method]
	return ok
}

func (c *Conn) lookup(method string) (Handler, bool) {
	c.hmu.RLock()
	defer c.hmu.RUnlock()
	h, ok := c.handlers[method]
	return h, ok
}

// Run 跑读循环，直到对端关闭或读出错。
//
// 对端在帧边界上正常退出返回 nil（子进程下班是正常事件）；半包、长度前缀
// 错乱、自己 Close 都返回错误。无论哪种，全部在途调用立即失败，不会挂死。
func (c *Conn) Run() error {
	for {
		f, err := readFrame(c.r)
		if err != nil {
			return c.shutdown(err)
		}
		var hdr header
		if err := json.Unmarshal(f.header, &hdr); err != nil {
			return c.shutdown(fmt.Errorf("rpc: 头部不是合法 JSON: %w", err))
		}
		if hdr.Method != "" {
			go c.dispatch(hdr, f.body)
			continue
		}
		c.deliver(hdr, f.body)
	}
}

// dispatch 处理一个入站请求。每个请求一个协程：Session.Await 这种慢调用
// 不能把同一连接上的其他调用排在自己后面。
func (c *Conn) dispatch(hdr header, body []byte) {
	id := hdr.ID
	h, ok := c.lookup(hdr.Method)
	if !ok {
		c.respond(id, Response{}, fmt.Errorf("%w: %s", ErrMethodNotFound, hdr.Method))
		return
	}
	ctx := context.Background()
	if hdr.Deadline != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, hdr.Deadline))
		defer cancel()
	}
	resp, err := c.invoke(ctx, hdr.Method, h, Request{Params: hdr.Params, Body: body})
	c.respond(id, resp, err)
}

// invoke 包一层 recover：插件的 panic 不能带崩协议协程，也不能让对端
// 悬在一个永远不会到的应答上。
func (c *Conn) invoke(ctx context.Context, method string, h Handler, req Request) (resp Response, err error) {
	defer func() {
		if v := recover(); v != nil {
			// 堆栈只进本进程日志（插件的 stderr 由核心重放），不进错误串：
			// 几十 KB 的堆栈塞进帧里，既没用也把 msg 撑爆。
			c.log.Error("rpc handler panic", "method", method, "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %s: %v", ErrHandlerPanic, method, v)
			resp = Response{}
		}
	}()
	return h(ctx, req)
}

// deliver 把一个应答交给等待中的调用方。
func (c *Conn) deliver(hdr header, body []byte) {
	c.mu.Lock()
	ch := c.pending[hdr.ID]
	delete(c.pending, hdr.ID)
	c.mu.Unlock()
	if ch == nil {
		// 调用方已经超时走人。这不是错误，但值得留痕：频繁出现说明
		// 超时设得比实际耗时紧。应答帧里没有 method（只有 id），
		// 所以别在这一行里找方法名。
		c.log.Debug("rpc 丢弃无人等待的应答", "id", hdr.ID)
		return
	}
	if hdr.Error != nil {
		ch <- delivery{err: c.reg.Decode(hdr.Error)}
		return
	}
	ch <- delivery{hdr: hdr, body: body}
}

// respond 写应答。err 非 nil 时只写错误码——错误串在 Msg 里，仅供排障。
func (c *Conn) respond(id uint64, resp Response, err error) {
	if err != nil {
		_ = c.send(header{ID: id, Error: c.reg.Encode(err)}, nil)
		return
	}
	raw := json.RawMessage("{}")
	if resp.Result != nil {
		b, mErr := json.Marshal(resp.Result)
		if mErr != nil {
			_ = c.send(header{ID: id, Error: c.reg.Encode(fmt.Errorf("rpc: 结果无法编码: %w", mErr))}, nil)
			return
		}
		raw = b
	}
	if err := c.send(header{ID: id, Result: raw}, resp.Body); errors.Is(err, ErrFrameTooLarge) {
		// 结果太大（典型是超限的图片）：回一条小错误，让调用方知道这次
		// 为什么没成。不发就等于让对面等到超时——那是最难查的一种失败。
		_ = c.send(header{ID: id, Error: c.reg.Encode(err)}, nil)
	}
}

// send 写一帧。除了「帧超上限」以外的写失败都会触发收摊：管道断了之后
// 这条连接上的任何调用都不会再有应答，早失败比让调用方各自超时更好排障。
//
// 超上限单独处理——它不是管道的问题，是**这一次**的内容太大（比如一张
// 超过 32 MiB 的图）。把它当成连接级故障会让后续所有调用一起失败。
func (c *Conn) send(hdr header, body []byte) error {
	jb, err := json.Marshal(hdr)
	if err != nil {
		c.log.Error("rpc 头部无法编码", "id", hdr.ID, "err", err)
		return fmt.Errorf("rpc: 头部无法编码: %w", err)
	}
	c.wmu.Lock()
	if c.writeErr != nil {
		err = c.writeErr
	} else if err = writeFrame(c.w, frame{header: jb, body: body}); err != nil {
		if !errors.Is(err, ErrFrameTooLarge) {
			c.writeErr = err
		}
	}
	c.wmu.Unlock()
	if err != nil && !errors.Is(err, ErrFrameTooLarge) {
		c.shutdown(err)
	}
	return err
}

// Call 发一个请求并等应答。params 为 nil 时不带参数段。
func (c *Conn) Call(ctx context.Context, method string, params any) (Result, error) {
	return c.CallBody(ctx, method, params, nil)
}

// CallBody 与 Call 相同，额外带一帧正文槽（图片字节）。
func (c *Conn) CallBody(ctx context.Context, method string, params any, body []byte) (Result, error) {
	hdr := header{ID: c.nextID.Add(1), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return Result{}, fmt.Errorf("rpc: 参数无法编码: %w", err)
		}
		hdr.Params = b
	}
	if dl, ok := ctx.Deadline(); ok {
		// 把截止时间捎给对端：否则「核心 3s 后放弃」与「插件还在跑」之间
		// 没有任何联系，插件里的 HTTP 调用不会跟着中止（决策 7）。
		hdr.Deadline = dl.UnixNano()
	}
	ch := make(chan delivery, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Result{}, ErrConnClosed
	}
	c.pending[hdr.ID] = ch
	c.mu.Unlock()

	if err := c.send(hdr, body); err != nil {
		c.forget(hdr.ID)
		return Result{}, err
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return Result{}, r.err
		}
		return Result{raw: r.hdr.Result, body: r.body}, nil
	case <-ctx.Done():
		c.forget(hdr.ID)
		return Result{}, ctx.Err()
	case <-c.done:
		c.forget(hdr.ID)
		return Result{}, c.errOrClosed()
	}
}

func (c *Conn) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// errOrClosed 给出「为什么这条调用没成」：连接是被哪一头、因为什么断的。
func (c *Conn) errOrClosed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runErr != nil && !errors.Is(c.runErr, io.EOF) {
		return fmt.Errorf("%w: %w", ErrConnClosed, c.runErr)
	}
	return ErrConnClosed
}

// Close 主动收摊：断开底层读写、让全部在途调用失败。
func (c *Conn) Close() error {
	c.shutdown(nil)
	return nil
}

// shutdown 只生效一次。err 为对端正常退出（io.EOF）时记 nil。
func (c *Conn) shutdown(err error) error {
	if errors.Is(err, io.EOF) {
		err = nil
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.runErr = err
		pend := c.pending
		c.pending = make(map[uint64]chan delivery, len(pend))
		c.mu.Unlock()

		for _, ch := range pend {
			// 带缓冲的通道，这里不会阻塞：调用方可能已经放弃，
			// 但缓冲位保证发送方（也就是我们）不会卡死。
			if errors.Is(err, io.ErrUnexpectedEOF) {
				ch <- delivery{err: fmt.Errorf("%w: 对端半包退出", ErrConnClosed)}
			} else {
				ch <- delivery{err: ErrConnClosed}
			}
		}
		// 两端各自实现 io.Closer 就关：插件侧是 stdin/stdout，核心侧是管道。
		// 同一个对象实现两者时会被关两次，第二次的 "file already closed"
		// 在这里没有意义，忽略即可。
		if cl, ok := c.r.(io.Closer); ok {
			_ = cl.Close()
		}
		if cl, ok := c.w.(io.Closer); ok {
			_ = cl.Close()
		}
		close(c.done)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runErr
}
