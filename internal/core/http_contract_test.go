package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件的接缝只有一个：host.HTTP，断言全部从插件真正拿到的那层发起。
//
// 为什么放在 core 而不是 pkg/host：翻译 `*httpx.HTTPError` → `*host.HTTPStatusError`
// 与「按 Manifest.AllowedDomains 收窄客户端」这两件事都在 hostHTTP 适配器里
// （ADR-0005 决策 1：契约包不得 import 实现包）。测的既然是**装配后的**行为，
// 就该跟适配器住在一起——顺带保证适配器漏翻译任何一条路径都会红。

// testHTTPHost 构造只看 HTTP 的宿主，并把测试服务器的传输注入出站客户端
// （信任 httptest 的自签证书）。返回宿主与根客户端，后者用于核对生效额度。
//
// 域名收窄走生产路径：`Core.hostHTTPBackend` → `WithDomains` → `hostHTTP`。
// 测试里自己拼 hostHTTP 会让「装配点漏调 WithDomains」这件事无人守门。
func testHTTPHost(t *testing.T, srv *httptest.Server, o httpx.Options) (*host.Service, *httpx.Client) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务器地址: %v", err)
	}
	o.HTTPClient = srv.Client()
	hc := httpx.New(o)
	backend := (&Core{http: hc}).hostHTTPBackend([]string{u.Hostname()})
	return host.NewService(host.Options{Name: "probe", HTTP: backend}), hc
}

// TestPluginHTTPStaysInsideManifestDomains 守住域名收窄的**唯一**入口。
//
// `Options.Domains` 删掉之后，白名单是否生效只取决于 Core.hostHTTPBackend
// 有没有调 WithDomains——它漏一次，插件就能打到任意主机（SSRF）。
func TestPluginHTTPStaysInsideManifestDomains(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()

	hc := httpx.New(httpx.Options{Timeout: time.Second, HTTPClient: srv.Client()})
	backend := (&Core{http: hc}).hostHTTPBackend([]string{"declared.example"})
	h := host.NewService(host.Options{Name: "probe", HTTP: backend})

	err := h.HTTP().GetJSON(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("未在 Manifest 声明的域名必须被拒绝")
	}
	if !strings.Contains(err.Error(), "未在插件 Manifest 中声明") {
		t.Fatalf("拒绝原因应是白名单，实际: %v", err)
	}
}

func TestHostHTTPForwardsHeaders(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": 7})
	}))
	defer srv.Close()

	h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second})
	var out struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	err := h.HTTP().GetJSONWithHeader(context.Background(), srv.URL, map[string]string{
		"Authorization": "ApiKey secret-value",
		"User-Agent":    "go_qqbot-test/1.0",
	}, &out)
	if err != nil {
		t.Fatalf("带请求头的 GET 失败: %v", err)
	}
	if gotAuth != "ApiKey secret-value" {
		t.Errorf("服务端收到的 Authorization=%q", gotAuth)
	}
	if gotUA != "go_qqbot-test/1.0" {
		t.Errorf("服务端收到的 User-Agent=%q（Cloudflare 一类前置会拦默认 UA）", gotUA)
	}
	if !out.OK || out.N != 7 {
		t.Errorf("响应体未正确解码: %+v", out)
	}
}

func TestHostHTTPGetJSONOnceStopsRetrying(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second, Retries: 3})
	if err := h.HTTP().GetJSONOnce(context.Background(), srv.URL, nil, nil); err == nil {
		t.Fatal("5xx 应返回错误")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("「只尝试一次」在服务端应表现为 1 次请求，实际 %d 次", got)
	}
}

func TestHostHTTPWithTimeoutCanBeChained(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release) // 先放行挂起的 handler，再关服务器

	h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: 5 * time.Second})
	fast := h.HTTP().WithTimeout(2 * time.Second).WithTimeout(50 * time.Millisecond)

	start := time.Now()
	err := fast.GetJSON(context.Background(), srv.URL, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("服务端超过指定超时仍未响应，应返回错误")
	}
	if elapsed > time.Second {
		t.Fatalf("应在毫秒级超时返回，实际耗时 %s", elapsed)
	}

	// 原视图不受影响：放宽超时返回的是新客户端，不修改共享实例。
	if base := h.HTTP().WithTimeout(5 * time.Second); base == nil {
		t.Fatal("WithTimeout 不应返回 nil")
	}
}

func TestOutboundDomainBucketBypassesDefaultQuota(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	// 默认桶每分钟 1 次（第一次之后要等约 60 s），该域名单独给足额度。
	// 若分桶没生效，第二次请求会排在默认桶上，1 s 的 ctx 直接超时。
	h, _ := testHTTPHost(t, srv, httpx.Options{
		Timeout:          time.Second,
		RatePerMin:       1,
		DomainRatePerMin: map[string]int{u.Hostname(): 6000},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 1; i <= 5; i++ {
		if err := h.HTTP().GetJSON(ctx, srv.URL, nil); err != nil {
			t.Fatalf("第 %d 次请求被默认桶卡住，域名分桶未生效: %v", i, err)
		}
	}
}

func TestOutboundUnlistedDomainUsesDefaultQuota(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()

	// 默认桶只够一次；给别的域名放宽额度不应影响本域名。
	h, _ := testHTTPHost(t, srv, httpx.Options{
		Timeout:          time.Second,
		RatePerMin:       1,
		DomainRatePerMin: map[string]int{"example.com": 6000},
	})

	ctx := context.Background()
	if err := h.HTTP().GetJSON(ctx, srv.URL, nil); err != nil {
		t.Fatalf("第一次请求应放行: %v", err)
	}

	// 第二次必须等默认桶：用短 ctx 把它变成可断言的超时，而不是真的等一分钟。
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err := h.HTTP().GetJSON(waitCtx, srv.URL, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("未配置独立额度的域名应受默认桶约束，实际错误: %v", err)
	}
}

func TestOutboundDomainQuotaInvalidFallsBack(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	h, hc := testHTTPHost(t, srv, httpx.Options{
		Timeout:    time.Second,
		RatePerMin: 6000,
		DomainRatePerMin: map[string]int{
			u.Hostname():  0,  // 零：必须回退默认值，不做「零等于无限」
			"example.com": -5, // 负数同理
		},
	})
	if got := hc.DomainLimits(); len(got) != 0 {
		t.Fatalf("非法额度不应生效，实际 %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 1; i <= 3; i++ {
		if err := h.HTTP().GetJSON(ctx, srv.URL, nil); err != nil {
			t.Fatalf("非法额度回退后应按默认额度放行，第 %d 次失败: %v", i, err)
		}
	}
}

func TestOutboundDomainBucketSharedAcrossPlugins(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	hc := httpx.New(httpx.Options{
		Timeout:          time.Second,
		RatePerMin:       6000,
		DomainRatePerMin: map[string]int{u.Hostname(): 1}, // 每分钟一次
		HTTPClient:       srv.Client(),
	})
	// 两个插件各自拿一个视图：一个用基础视图，一个用 WithTimeout 之后的视图。
	// 它们必须共用同一个域名桶，否则额度会被复制成两份。
	a := host.NewService(host.Options{Name: "query", HTTP: (&Core{http: hc}).hostHTTPBackend([]string{u.Hostname()})})
	b := host.NewService(host.Options{Name: "broadcast", HTTP: (&Core{http: hc}).hostHTTPBackend([]string{u.Hostname()})})

	if err := a.HTTP().GetJSON(context.Background(), srv.URL, nil); err != nil {
		t.Fatalf("第一次请求应放行: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := b.HTTP().WithTimeout(2*time.Second).GetJSON(waitCtx, srv.URL, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("同域名的两个插件视图应共用额度，实际错误: %v", err)
	}
}

// statusServer 返回固定状态码与固定 body 的测试服务器。
func statusServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

const (
	// rejectBody 是测试里用的响应体原文；它绝不能出现在错误的文本里。
	rejectBody = `{"error":{"code":16,"error":"Access level too low"}}`
)

// 以下四个用例锁定「非 2xx 如何到达插件」这条契约：插件只能拿到
// host.HTTPStatusError，不得被迫（也不得能够）import internal/httpx 去读状态码。

func TestHostHTTPStatusErrorWrapping(t *testing.T) {
	// 429 / 5xx 走 httpx 的 lastErr 分支，400 / 404 走直接返回分支，
	// 两条路径都必须被同一处转换覆盖。
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := statusServer(t, code, rejectBody)
			defer srv.Close()

			// 即使配置了重试，GetJSONOnce 也只打一次；返回的仍是 lastErr。
			h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second, Retries: 2})

			err := h.HTTP().GetJSONOnce(context.Background(), srv.URL, nil, nil)
			var se *host.HTTPStatusError
			if !errors.As(err, &se) {
				t.Fatalf("非 2xx 应返回 *host.HTTPStatusError，实际 %T: %v", err, err)
			}
			if se.Status != code {
				t.Errorf("Status=%d，期望 %d", se.Status, code)
			}
			if se.Body != rejectBody {
				t.Errorf("Body 应是 httpx 截断后的原文，实际 %q", se.Body)
			}

			// 转换后不得再命中 httpx 的类型：否则调用方不知道该信哪一个。
			var he *httpx.HTTPError
			if errors.As(err, &he) {
				t.Fatalf("转换后仍能 errors.As 到 *httpx.HTTPError（Status=%d）", he.Status)
			}
		})
	}
}

func TestHostHTTPAllMethodsWrapStatusError(t *testing.T) {
	// 四个方法各有自己的转发行，漏掉任何一个都会让插件在某条路径上拿不到状态码。
	methods := map[string]func(h *host.Service, url string) error{
		"GetJSON": func(h *host.Service, url string) error {
			return h.HTTP().GetJSON(context.Background(), url, nil)
		},
		"GetJSONWithHeader": func(h *host.Service, url string) error {
			return h.HTTP().GetJSONWithHeader(context.Background(), url, map[string]string{"Authorization": "ApiKey x"}, nil)
		},
		"GetJSONOnce": func(h *host.Service, url string) error {
			return h.HTTP().GetJSONOnce(context.Background(), url, nil, nil)
		},
		"PostJSON": func(h *host.Service, url string) error {
			return h.HTTP().PostJSON(context.Background(), url, map[string]any{"a": 1}, nil)
		},
	}

	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(t, http.StatusForbidden, rejectBody)
			defer srv.Close()

			h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second, Retries: 0})
			err := call(h, srv.URL)

			var se *host.HTTPStatusError
			if !errors.As(err, &se) {
				t.Fatalf("%s 的非 2xx 应返回 *host.HTTPStatusError，实际 %T: %v", name, err, err)
			}
			if se.Status != http.StatusForbidden {
				t.Errorf("%s: Status=%d，期望 403", name, se.Status)
			}
		})
	}
}

func TestHostHTTPStatusErrorTextOmitsURLAndBody(t *testing.T) {
	srv := statusServer(t, http.StatusTooManyRequests, rejectBody)
	defer srv.Close()

	h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second, Retries: 1})
	err := h.HTTP().GetJSONOnce(context.Background(), srv.URL+"/v2/user/1?key=secret", nil, nil)
	if err == nil {
		t.Fatal("429 应返回错误")
	}

	text := err.Error()
	if !strings.Contains(text, "429") {
		t.Errorf("错误串应含状态码，实际 %q", text)
	}
	// 错误串会进日志，而 URL 里有 query、body 里可能有敏感内容。
	for _, banned := range []string{rejectBody, "Access level", "secret", srv.URL, "key="} {
		if strings.Contains(text, banned) {
			t.Errorf("错误串不得包含 %q，实际 %q", banned, text)
		}
	}
}

func TestHostHTTPNetworkErrorNotWrapped(t *testing.T) {
	// 关掉的监听器：连接被拒，属于网络错误，不是「上游返回了非 2xx」。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听器失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	// 出站层只接受 https，所以用 https:// 去连一个已经关掉的端口。
	hc := httpx.New(httpx.Options{Timeout: time.Second, Retries: 0})
	h := host.NewService(host.Options{Name: "probe", HTTP: (&Core{http: hc}).hostHTTPBackend([]string{"127.0.0.1"})})

	callErr := h.HTTP().GetJSONOnce(context.Background(), "https://"+addr+"/x", nil, nil)
	if callErr == nil {
		t.Fatal("连接失败应返回错误")
	}
	var se *host.HTTPStatusError
	if errors.As(callErr, &se) {
		t.Fatalf("网络错误不得被包装成 *host.HTTPStatusError，实际 %v", callErr)
	}
}

func TestHostHTTPContextTimeoutNotWrapped(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := h.HTTP().GetJSONOnce(ctx, srv.URL, nil, nil)
	if err == nil {
		t.Fatal("ctx 超时应返回错误")
	}
	var se *host.HTTPStatusError
	if errors.As(err, &se) {
		t.Fatalf("ctx 超时不得被包装成 *host.HTTPStatusError，实际 %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx 超时应原样保留 DeadlineExceeded，实际 %v", err)
	}
}

// TestHostHTTPBackendNilStaysNil 守住 typed-nil：把未初始化的客户端包进接口
// 会得到非 nil 的接口值，host 侧的判空失效、调用即 panic。核心单测大量使用
// 不接 HTTP 的 Core，这条路径必须给「未初始化」而不是崩。
func TestHostHTTPBackendNilStaysNil(t *testing.T) {
	c := &Core{}
	backend := c.hostHTTPBackend(nil)
	if backend != nil {
		t.Fatalf("没有 HTTP 客户端时应返回未包装的 nil 接口，实际 %T", backend)
	}

	svc := host.NewService(host.Options{Name: "probe", HTTP: backend})
	err := svc.HTTP().GetJSON(context.Background(), "https://example.com/x", nil)
	if err == nil || !strings.Contains(err.Error(), "未初始化") {
		t.Fatalf("缺失后端时调用应报「未初始化」，实际 %v", err)
	}
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv
}
