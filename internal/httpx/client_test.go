package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
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
