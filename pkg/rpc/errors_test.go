package rpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/knight000/go_qqbot/pkg/host"
)

// TestErrorCodeRoundTrip 是验收要求的「两端 errors.Is 都成立」。
//
// 表里同时钉住**数值**：码是永久契约，改一个数字等于让所有老插件把
// 「额度用尽」读成「被限流」。这类改动必须红，而不是悄悄生效。
func TestErrorCodeRoundTrip(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"主动消息额度", host.ErrActiveQuota, CodeActiveQuota},
		{"主动消息未开启", host.ErrActiveDisabled, CodeActiveDisabled},
		{"平台限流", host.ErrRateLimited, CodeRateLimited},
		{"会话被占用", host.ErrSessionBusy, CodeSessionBusy},
		{"渲染超高", host.ErrRenderTooLarge, CodeRenderTooLarge},
		{"渲染超时", host.ErrRenderTimeout, CodeRenderTimeout},
		{"ctx 取消", context.Canceled, CodeCanceled},
		{"ctx 超时", context.DeadlineExceeded, CodeDeadline},
		{"方法未注册", ErrMethodNotFound, CodeMethodNotFound},
		{"连接已关闭", ErrConnClosed, CodeConnClosed},
	}
	if r == nil {
		t.Fatal("NewRegistry 返回 nil")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire := r.Encode(c.err)
			if wire == nil {
				t.Fatal("Encode 返回 nil")
			}
			if wire.Code != c.code {
				t.Fatalf("码 = %d，期望 %d（数值是契约，不能改）", wire.Code, c.code)
			}
			back := r.Decode(wire)
			if !errors.Is(back, c.err) {
				t.Fatalf("还原后 errors.Is(%v) = false", c.err)
			}
		})
	}
}

// TestErrorWrappedKeepsMessage：哨兵的包装（带 peer、带场景）必须保留——
// 错误串的唯一用途就是排障，剥掉它等于让线上日志失去线索。
func TestErrorWrappedKeepsMessage(t *testing.T) {
	r := NewRegistry()
	wrapped := fmt.Errorf("%w: scene=group peer=g1", host.ErrRateLimited)
	wire := r.Encode(wrapped)
	if wire.Code != CodeRateLimited {
		t.Fatalf("码 = %d，期望 %d", wire.Code, CodeRateLimited)
	}
	back := r.Decode(wire)
	if !errors.Is(back, host.ErrRateLimited) {
		t.Fatal("还原后丢掉了哨兵")
	}
	if got, want := back.Error(), wrapped.Error(); got != want {
		t.Fatalf("文案 = %q，期望 %q", got, want)
	}
}

// TestErrorHTTPStatus 断言两个额外字段过通道：状态码与 body。
//
// body 是 §3.3 里唯一被允许「读」的错误串（Torn 的业务码在里面），
// 它丢了就等于所有非 2xx 只剩一个数字。
func TestErrorHTTPStatus(t *testing.T) {
	r := NewRegistry()
	src := &host.HTTPStatusError{Status: 429, Body: `{"error":{"code":16}}`}
	wire := r.Encode(src)
	if wire.Code != CodeHTTPStatus || wire.Status != 429 {
		t.Fatalf("wire = %+v", wire)
	}
	var back *host.HTTPStatusError
	if !errors.As(r.Decode(wire), &back) {
		t.Fatal("还原后不是 *host.HTTPStatusError")
	}
	if back.Status != 429 || back.Body != src.Body {
		t.Fatalf("还原 = %+v", back)
	}
}

// TestErrorRenderTooLarge：高度必须跟着过去，否则插件没法决定「拆几条」。
func TestErrorRenderTooLarge(t *testing.T) {
	r := NewRegistry()
	src := &host.RenderTooLargeError{NeedHeight: 8123}
	wire := r.Encode(src)
	if wire.Code != CodeRenderTooLarge || wire.NeedHeight != 8123 {
		t.Fatalf("wire = %+v", wire)
	}
	back := r.Decode(wire)
	var tooLarge *host.RenderTooLargeError
	if !errors.As(back, &tooLarge) {
		t.Fatal("还原后不是 *host.RenderTooLargeError")
	}
	if tooLarge.NeedHeight != 8123 {
		t.Fatalf("NeedHeight = %d", tooLarge.NeedHeight)
	}
	if !errors.Is(back, host.ErrRenderTooLarge) {
		t.Fatal("没有命中 ErrRenderTooLarge")
	}
}

// TestErrorUnknown 保留原文，但不假装它是某个哨兵。
func TestErrorUnknown(t *testing.T) {
	r := NewRegistry()
	src := errors.New("fixture 故意失败")
	wire := r.Encode(src)
	if wire.Code != CodeUnknown || wire.Msg != src.Error() {
		t.Fatalf("wire = %+v", wire)
	}
	back := r.Decode(wire)
	if back.Error() != src.Error() {
		t.Fatalf("文案 = %q", back.Error())
	}
	if errors.Is(back, host.ErrRateLimited) {
		t.Fatal("未分类错误被误判成哨兵")
	}
}

// TestErrorRemoteCodeSurvives 是「实现自定义码」的核心用例（#39 验收里的
// ErrPassiveExhausted 走的就是这条）：插件不认识 1000 号，但它转发错误时
// 不能把码改成 CodeUnknown，否则核心再也认不出自己的哨兵。
func TestErrorRemoteCodeSurvives(t *testing.T) {
	implErr := errors.New("主动消息被动窗口已用尽")
	hostReg := NewRegistry()
	if err := hostReg.Register(CodeImplBase, implErr); err != nil {
		t.Fatalf("注册实现自定义码: %v", err)
	}

	// 核心 → 插件：插件侧的表里没有 1000 号。
	wire := hostReg.Encode(implErr)
	pluginReg := NewRegistry()
	pluginSide := pluginReg.Decode(wire)
	var remote *RemoteError
	if !errors.As(pluginSide, &remote) || remote.Code != CodeImplBase {
		t.Fatalf("插件侧看到的 = %v", pluginSide)
	}
	// 插件 → 核心：原码转发。
	back := hostReg.Encode(pluginSide)
	if back.Code != CodeImplBase {
		t.Fatalf("转发后的码 = %d，期望 %d", back.Code, CodeImplBase)
	}
	if !errors.Is(hostReg.Decode(back), implErr) {
		t.Fatal("核心没能还原自己的哨兵")
	}
}

// TestRegistryRegisterGuards：保留段与重复注册都必须拒绝。
func TestRegistryRegisterGuards(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(CodeRateLimited, errors.New("x")); err == nil {
		t.Fatal("占用了保留段的码却没有报错")
	}
	if err := r.Register(CodeImplBase, host.ErrRateLimited); err == nil {
		t.Fatal("把已注册的哨兵挂到第二个码上却没有报错")
	}
	if err := r.Register(CodeImplBase, errors.New("a")); err != nil {
		t.Fatalf("第一次注册: %v", err)
	}
	if err := r.Register(CodeImplBase, errors.New("b")); err == nil {
		t.Fatal("同一个码注册两次却没有报错")
	}
	if err := r.Register(CodeImplBase+1, nil); err == nil {
		t.Fatal("注册 nil 错误却没有报错")
	}
}

// TestErrorEncodeNil：nil 进 nil 出，别把 nil 编成一个「成功但带错」的帧。
func TestErrorEncodeNil(t *testing.T) {
	r := NewRegistry()
	if wire := r.Encode(nil); wire != nil {
		t.Fatalf("Encode(nil) = %+v", wire)
	}
	if err := r.Decode(nil); err != nil {
		t.Fatalf("Decode(nil) = %v", err)
	}
	if err := r.Decode(&Error{Code: CodeOK}); err != nil {
		t.Fatalf("Decode(CodeOK) = %v", err)
	}
}

// TestErrorDecodeKeepsLocalIdentity 校验还原出来的是**本地**的那个值本身。
func TestErrorDecodeKeepsLocalIdentity(t *testing.T) {
	r := NewRegistry()
	back := r.Decode(&Error{Code: CodeSessionBusy, Msg: host.ErrSessionBusy.Error()})
	if back != host.ErrSessionBusy {
		t.Fatalf("还原 = %#v，期望就是 host.ErrSessionBusy 本身", back)
	}
}
