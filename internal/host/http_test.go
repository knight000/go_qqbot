package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/httpx"
)

// 本文件的接缝只有一个：host.HTTP。所有断言都从插件真正拿到的那层发起，
// 一次覆盖转发、白名单、请求头、重试次数与超时，避免在出站包与宿主包
// 各写一份重复断言。

// testHTTPHost 构造只看 HTTP 的宿主，并把测试服务器的传输注入出站客户端
// （信任 httptest 的自签证书）。返回宿主与根客户端，后者用于核对生效额度。
func testHTTPHost(t *testing.T, srv *httptest.Server, o httpx.Options) (*Service, *httpx.Client) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务器地址: %v", err)
	}
	o.HTTPClient = srv.Client()
	hc := httpx.New(o)
	svc := NewService(Options{Name: "probe", HTTP: hc, Domains: []string{u.Hostname()}})
	return svc, hc
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
	a := NewService(Options{Name: "query", HTTP: hc, Domains: []string{u.Hostname()}})
	b := NewService(Options{Name: "broadcast", HTTP: hc, Domains: []string{u.Hostname()}})

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

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv
}
