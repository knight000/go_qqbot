package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 错误跨进程只走数值码（ADR-0005 决策 9）。
//
// 理由是 §3.3 早就写明的那句话：错误串**只供排障、不得解析**。一旦对面
// 拿 `strings.HasPrefix(err.Error(), "主动消息本地额度")` 来分类，句子的
// 任何一次润色都会静默改变控制流——那种事故没有编译期保护，也不会在
// 测试里出现（测试写的是哨兵），只会在线上表现为「偶尔多发一条消息」。
//
// 所以两端各自持一张表：发侧把 error 折成 {code, …}，收侧把 code 还原成
// **本地哨兵**，`errors.Is` / `errors.As` 在两端照常成立。码值一旦发布就
// 是永久契约，只能新增、不能改写。
//
// 码段：
//
//	0        成功
//	1        未分类错误（只带 msg，仅供排障）
//	2–99     pkg/host 里的哨兵与类型，闭集，见下表
//	100–999  保留
//	1000+    实现自定义（internal/core 的 ErrPassiveExhausted 取 1000）
//
// 为什么留实现自定义段：`core.ErrPassiveExhausted` 是**故意**留在 core 的
// （#36：没有第二个真实消费方，不进契约）。它必须过通道，但 pkg/rpc 不能
// import internal/core——所以只给码段与注册口，具体号码由实现方注册。
const (
	CodeOK = 0
	// CodeUnknown 是未分类错误：msg 里有原文，收侧还原成 *RemoteError。
	CodeUnknown = 1

	// —— pkg/host 哨兵 ——
	CodeActiveQuota    = 2  // host.ErrActiveQuota
	CodeActiveDisabled = 3  // host.ErrActiveDisabled
	CodeRateLimited    = 4  // host.ErrRateLimited
	CodeSessionBusy    = 5  // host.ErrSessionBusy
	CodeRenderTooLarge = 6  // host.RenderTooLargeError / host.ErrRenderTooLarge
	CodeRenderTimeout  = 7  // host.ErrRenderTimeout
	CodeHTTPStatus     = 8  // *host.HTTPStatusError（带 status 与 body）
	CodeCanceled       = 9  // context.Canceled
	CodeDeadline       = 10 // context.DeadlineExceeded
	CodeMethodNotFound = 11 // ErrMethodNotFound
	CodeConnClosed     = 12 // ErrConnClosed
	CodeHandlerPanic   = 13 // handler 内部 panic，已 recover
	CodeFrameTooLarge  = 14 // ErrFrameTooLarge：这一次的内容太大，连接没坏

	// CodeImplBase 起是实现自定义码段。注册时低于它的号码一律拒绝：
	// 让实现方「顺手」复用 2 号会把两个不同的哨兵变成同一个。
	CodeImplBase = 1000
)

// Error 是通道上的错误形态。
//
// Msg 只供排障，两个例外必须知道：
//   - HTTPStatusError 的 Msg 是 **body**（平台错误码在里面，如 Torn 的
//     "error 16"），插件读它是有正当用途的；
//   - RenderTooLargeError 的高度在 NeedHeight，不在 Msg。
type Error struct {
	Code       int    `json:"code"`
	Msg        string `json:"msg,omitempty"`
	Status     int    `json:"status,omitempty"`
	NeedHeight int    `json:"need_height,omitempty"`
}

// RemoteError 是收侧无法还原成本地哨兵时的兜底：码是对方的，文案是对方的。
//
// 它自己再被编回去时保持原码（见 Registry.Encode 的第一条分支），所以
// 「A 把不认识的错转手抛给 B」不会退化成 CodeUnknown——那会让
// `core.ErrPassiveExhausted` 这类实现自定义码在转发一次之后消失。
type RemoteError struct {
	Code int
	Msg  string
}

func (e *RemoteError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("rpc: 对端错误（码 %d）", e.Code)
}

// withMsg 是「本地哨兵的语义 + 对端的现场描述」：Unwrap 让 errors.Is 命中
// 哨兵，Error() 让日志里留下场景与 peer。用它而不是 fmt.Errorf("%w")，
// 是因为对端文案通常已经把哨兵原文包含在内（`fmt.Errorf("%w: peer=x")`），
// 再拼一次会得到「额度已用尽: 额度已用尽: peer=x」。
type withMsg struct {
	msg string
	err error
}

func (e withMsg) Error() string { return e.msg }
func (e withMsg) Unwrap() error { return e.err }

// entry 是码与哨兵的一行对应。
type entry struct {
	code int
	err  error
}

// Registry 是码表的持有者。默认表（NewRegistry）只含 pkg/host 的哨兵与
// context 的两个；实现方用自己的表覆盖它，再 Register 各自的码。
//
// 两端**必须**用同一张表才能对称还原，所以表是显式传进 Conn 的，
// 不用包级全局：全局表会让「测试里注册的码」泄漏到别的用例，也会让
// 一个进程里两个不同实现（核心 + 某插件内嵌的 host 替身）互相覆盖。
type Registry struct {
	mu      sync.RWMutex
	entries []entry
	byCode  map[int]error
}

// NewRegistry 返回带默认表的注册表。
//
// 顺序有意义：先 host 的哨兵、后 context 的两个。一个错误同时包装两者时
// （`fmt.Errorf("%w: %w", host.ErrRenderTimeout, context.Canceled)`）走前面
// 那条——业务哨兵比「被取消」更能指出现场。
func NewRegistry() *Registry {
	r := &Registry{byCode: make(map[int]error, 16)}
	for _, e := range []entry{
		{CodeActiveQuota, host.ErrActiveQuota},
		{CodeActiveDisabled, host.ErrActiveDisabled},
		{CodeRateLimited, host.ErrRateLimited},
		{CodeSessionBusy, host.ErrSessionBusy},
		{CodeRenderTooLarge, host.ErrRenderTooLarge},
		{CodeRenderTimeout, host.ErrRenderTimeout},
		{CodeCanceled, context.Canceled},
		{CodeDeadline, context.DeadlineExceeded},
		{CodeMethodNotFound, ErrMethodNotFound},
		{CodeConnClosed, ErrConnClosed},
		{CodeHandlerPanic, ErrHandlerPanic},
		{CodeFrameTooLarge, ErrFrameTooLarge},
	} {
		// 默认表自己不可能冲突，冲突即代码写错，直接 panic 比留个
		// 半残的表更难查。
		if err := r.add(e.code, e.err); err != nil {
			panic(err)
		}
	}
	return r
}

// Register 增加一条对应关系。码必须在实现自定义段里，且不能与已有条目重复。
func (r *Registry) Register(code int, err error) error {
	if code < CodeImplBase {
		return fmt.Errorf("rpc: 码 %d 落在保留段（< %d），实现自定义码请从 %d 起", code, CodeImplBase, CodeImplBase)
	}
	return r.add(code, err)
}

// add 是两条路的公共实现：默认表用保留段里的固定码，Register 只放实现段。
func (r *Registry) add(code int, err error) error {
	if err == nil {
		return errors.New("rpc: 注册 nil 错误")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.byCode[code]; ok {
		return fmt.Errorf("rpc: 码 %d 已被 %v 占用", code, prev)
	}
	for _, e := range r.entries {
		if e.err == err {
			return fmt.Errorf("rpc: 错误 %v 已注册在码 %d", err, e.code)
		}
	}
	r.entries = append(r.entries, entry{code: code, err: err})
	r.byCode[code] = err
	return nil
}

// Encode 把 error 折成通道形态。nil 返回 nil。
func (r *Registry) Encode(err error) *Error {
	if err == nil {
		return nil
	}
	// 1. 已经过了一遍通道的：原码转发，别再降级成 CodeUnknown。
	var remote *RemoteError
	if errors.As(err, &remote) {
		return &Error{Code: remote.Code, Msg: remote.Msg}
	}
	// 2. 带数据的两个类型：先按类型抓，否则会被下面的哨兵分支吃掉细节。
	var status *host.HTTPStatusError
	if errors.As(err, &status) {
		return &Error{Code: CodeHTTPStatus, Status: status.Status, Msg: status.Body}
	}
	var tooLarge *host.RenderTooLargeError
	if errors.As(err, &tooLarge) {
		return &Error{Code: CodeRenderTooLarge, NeedHeight: tooLarge.NeedHeight, Msg: err.Error()}
	}
	// 3. 哨兵。Msg 带上原文——它只供排障，而排障需要的正是「哪个 peer」。
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		if errors.Is(err, e.err) {
			return &Error{Code: e.code, Msg: err.Error()}
		}
	}
	return &Error{Code: CodeUnknown, Msg: err.Error()}
}

// Decode 把通道形态还原成本地哨兵。
//
// 还原出来的是**本地**的那个 error 值，所以 `errors.Is(err, host.ErrRateLimited)`
// 在收侧成立——这正是决策 9 的全部目的。
func (r *Registry) Decode(e *Error) error {
	if e == nil || e.Code == CodeOK {
		return nil
	}
	switch e.Code {
	case CodeHTTPStatus:
		// Body 走 Msg：平台把业务错误码放在 body 里（§3.3 唯一允许的读取用途）。
		return &host.HTTPStatusError{Status: e.Status, Body: e.Msg}
	case CodeRenderTooLarge:
		return &host.RenderTooLargeError{NeedHeight: e.NeedHeight}
	}
	r.mu.RLock()
	sentinel := r.byCode[e.Code]
	r.mu.RUnlock()
	if sentinel == nil {
		return &RemoteError{Code: e.Code, Msg: e.Msg}
	}
	// 文案与哨兵原文一致（裸哨兵过通道）时直接给哨兵本身：
	// 让 `==` 也成立，测试与日志都少一层包装。
	if e.Msg == "" || e.Msg == sentinel.Error() {
		return sentinel
	}
	return withMsg{msg: e.Msg, err: sentinel}
}
