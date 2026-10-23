package core

import (
	"context"
	"errors"
	"time"

	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/pkg/host"
)

// hostHTTP 把 internal/httpx 的客户端适配成 host.HTTPBackend（ADR-0005 决策 1）。
//
// 适配器存在的唯一理由是边界：`pkg/host` 不得 import 任何实现包，所以
// 「非 2xx 翻译成 *host.HTTPStatusError」只能发生在这层——只有它同时认识
// 两边的类型。插件看到的仍是 host 的类型，不需要（也不能）import httpx。
type hostHTTP struct{ c *httpx.Client }

func (h hostHTTP) WithTimeout(d time.Duration) host.HTTPBackend {
	return hostHTTP{h.c.WithTimeout(d)}
}

func (h hostHTTP) GetJSON(ctx context.Context, url string, out any) error {
	return wrapHTTPErr(h.c.GetJSON(ctx, url, out))
}

func (h hostHTTP) GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error {
	return wrapHTTPErr(h.c.GetJSONWithHeader(ctx, url, header, out))
}

// GetJSONOnce 同样要翻译：429 / 5xx 走的是 httpx 循环里保存的 lastErr，
// 也是 *httpx.HTTPError，只是转换点换了位置。
func (h hostHTTP) GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error {
	return wrapHTTPErr(h.c.GetJSONOnce(ctx, url, header, out))
}

func (h hostHTTP) PostJSON(ctx context.Context, url string, in, out any) error {
	return wrapHTTPErr(h.c.PostJSON(ctx, url, in, out))
}

// wrapHTTPErr 把 httpx 的非 2xx 错误翻译成宿主契约里的类型。
//
// 刻意不 Unwrap 出原始的 *httpx.HTTPError：两个类型同时可命中会让调用方
// 分不清该信哪个，也会让「插件只依赖 host」这条边界在错误处理上失效。
// 非 HTTPError 的错误（网络失败、ctx 取消、解析失败）原样返回。
func wrapHTTPErr(err error) error {
	if err == nil {
		return nil
	}
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		return &host.HTTPStatusError{Status: he.Status, Body: he.Body}
	}
	return err
}
