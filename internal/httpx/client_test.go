package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPluginClientDeniesEmptyAllowlist(t *testing.T) {
	c := New(Options{}).WithDomains(nil)
	err := c.GetJSON(context.Background(), "https://api.torn.com/user/", nil)
	if err == nil {
		t.Fatal("插件未声明域名时应拒绝出站")
	}
}

func TestPluginClientAllowlist(t *testing.T) {
	c := New(Options{}).WithDomains([]string{"torn.com"})

	if err := c.checkURL("https://api.torn.com/user/"); err != nil {
		t.Fatalf("声明的域名应放行: %v", err)
	}
	if err := c.checkURL("https://torn.com.evil.com/user/"); err == nil {
		t.Fatal("后缀伪装域名应拒绝")
	}
	if err := c.checkURL("http://api.torn.com/user/"); err == nil {
		t.Fatal("非 https 应拒绝")
	}
}

func TestCoreClientHasNoRestriction(t *testing.T) {
	c := New(Options{})
	if err := c.checkURL("https://bots.qq.com/app/getAppAccessToken"); err != nil {
		t.Fatalf("核心客户端不应受插件白名单限制: %v", err)
	}
}

func TestPostJSONOnceDoesNotRetry(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Options{Timeout: time.Second, Retries: 3})
	c.hc = srv.Client() // 信任测试证书

	if err := c.PostJSONOnce(context.Background(), srv.URL, nil, map[string]string{"a": "b"}, nil); err == nil {
		t.Fatal("5xx 应返回错误")
	}
	if hits != 1 {
		t.Fatalf("发消息类请求不得自动重试，实际请求 %d 次", hits)
	}
}

// evilHost 只存在于测试的"未声明域名"：DNS 里查不到，由传输层改写 dial
// 目标指到本地监听器。这样白名单一旦失效，请求**真的**会被送过去，
// 「目标站 hit=0」才是可信的证据（否则失败可能只是 DNS/TLS 报错，断言恒真）。
const evilHost = "evil.test"

// redirectFixture 造出「源站 302 到 location(target.URL)」的场景。
//
// location 拿到目标站地址后自己决定跳到哪：换域名、降 scheme 或同站跳转。
type redirectFixture struct {
	src     *httptest.Server
	target  *httptest.Server
	client  *http.Client
	hits    int32 // 未声明域名的监听器收到的请求数
	srcHits int32
}

func newRedirectFixture(t *testing.T, location func(targetURL string) string) *redirectFixture {
	t.Helper()
	f := &redirectFixture{}
	f.target = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.hits, 1)
		_, _ = w.Write([]byte(`{"stolen":true}`))
	}))
	t.Cleanup(f.target.Close)

	loc := location(f.target.URL)
	f.src = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.srcHits, 1)
		http.Redirect(w, r, loc, http.StatusFound)
	}))
	t.Cleanup(f.src.Close)

	// 信任自签证书 + 把 evilHost 解析到目标监听器。TLS 校验与白名单是两件事：
	// 这里要验的是「白名单有没有拦住这一跳」，所以放过证书。
	tr := f.src.Client().Transport.(*http.Transport).Clone()
	cfg := &tls.Config{}
	if tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
	}
	cfg.InsecureSkipVerify = true
	tr.TLSClientConfig = cfg
	dial := tr.DialContext
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	targetAddr := f.target.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, _, err := net.SplitHostPort(addr); err == nil && strings.EqualFold(h, evilHost) {
			addr = targetAddr
		}
		return dial(ctx, network, addr)
	}
	f.client = &http.Client{Transport: tr}
	return f
}

func (f *redirectFixture) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(f.src.URL)
	if err != nil {
		t.Fatalf("解析源站地址: %v", err)
	}
	return u.Hostname()
}

// TestRedirectToUndeclaredDomainIsNotFollowed 是 P2-9 的永久守卫：
// 插件只声明了源站，源站 302 到别的域名时请求不得跟过去。
func TestRedirectToUndeclaredDomainIsNotFollowed(t *testing.T) {
	f := newRedirectFixture(t, func(targetURL string) string {
		return strings.Replace(targetURL, "127.0.0.1", evilHost, 1) + "/steal"
	})
	c := New(Options{Timeout: time.Second, Retries: 2, HTTPClient: f.client}).
		WithDomains([]string{f.host(t)})

	err := c.GetJSON(context.Background(), f.src.URL, nil)
	if err == nil {
		t.Fatal("重定向到未声明域名应被拦下，实际成功")
	}
	var re *RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("应返回可判定的 *RedirectError，实际 %T: %v", err, err)
	}
	if !strings.Contains(re.URL, evilHost) {
		t.Errorf("错误应指向被拒的那一跳，实际 URL=%q", re.URL)
	}
	if got := atomic.LoadInt32(&f.hits); got != 0 {
		t.Fatalf("未声明域名收到了 %d 次请求（SSRF 未被拦住）", got)
	}
	if got := atomic.LoadInt32(&f.srcHits); got != 1 {
		t.Fatalf("被策略拒绝的请求不该重试，源站应只被请求 1 次，实际 %d 次", got)
	}
}

// 降 scheme 是另一类越界：https 的承诺被重定向重写成 http，明文出网。
func TestRedirectSchemeChangeIsRejected(t *testing.T) {
	f := newRedirectFixture(t, func(targetURL string) string {
		return strings.Replace(targetURL, "https://", "http://", 1) + "/plain"
	})
	c := New(Options{Timeout: time.Second, HTTPClient: f.client}).
		WithDomains([]string{f.host(t)})

	err := c.GetJSON(context.Background(), f.src.URL, nil)
	var re *RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("应返回可判定的 *RedirectError，实际 %T: %v", err, err)
	}
	if !strings.Contains(strings.ToLower(re.Reason), "scheme") {
		t.Errorf("拒绝理由应点明 scheme 变化，实际 %q", re.Reason)
	}
	if got := atomic.LoadInt32(&f.hits); got != 0 {
		t.Fatalf("降 scheme 的重定向被跟随了（明文出网），目标站收到 %d 次", got)
	}
}

// 克隆出来的 client 必须带同样的检查。两种克隆顺序都要覆盖：
// 白名单是在 WithDomains 里设的，而 http.Client 是 WithTimeout 里重建的。
func TestRedirectPolicySurvivesClones(t *testing.T) {
	build := map[string]func(*Client, []string) *Client{
		"WithDomainsThenWithTimeout": func(c *Client, domains []string) *Client {
			return c.WithDomains(domains).WithTimeout(2 * time.Second)
		},
		"WithTimeoutThenWithDomains": func(c *Client, domains []string) *Client {
			return c.WithTimeout(2 * time.Second).WithDomains(domains)
		},
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			f := newRedirectFixture(t, func(targetURL string) string {
				return strings.Replace(targetURL, "127.0.0.1", evilHost, 1) + "/steal"
			})
			c := mk(New(Options{Timeout: time.Second, HTTPClient: f.client}), []string{f.host(t)})

			err := c.GetJSON(context.Background(), f.src.URL, nil)
			var re *RedirectError
			if !errors.As(err, &re) {
				t.Fatalf("克隆出的 client 丢了重定向检查，实际 %T: %v", err, err)
			}
			if got := atomic.LoadInt32(&f.hits); got != 0 {
				t.Fatalf("克隆出的 client 把请求跟到了未声明域名，目标站收到 %d 次", got)
			}
		})
	}
}

// 反向守卫：白名单内的同站跳转必须照常跟随，否则"拒绝一切重定向"也能让
// 上面几条测试变绿。
func TestRedirectWithinAllowlistIsFollowed(t *testing.T) {
	var finalHits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		atomic.AddInt32(&finalHits, 1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	c := New(Options{Timeout: time.Second, HTTPClient: srv.Client()}).WithDomains([]string{u.Hostname()})
	if err := c.GetJSON(context.Background(), srv.URL+"/hop", nil); err != nil {
		t.Fatalf("白名单内的重定向应照常跟随: %v", err)
	}
	if got := atomic.LoadInt32(&finalHits); got != 1 {
		t.Fatalf("重定向未被跟随，终点 handler 收到 %d 次", got)
	}
}
