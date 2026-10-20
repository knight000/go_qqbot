package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/httpx"
)

// 本文件锁定「非 2xx 如何到达插件」这条契约：插件只能拿到 host.HTTPStatusError，
// 不得被迫（也不得能够）import internal/httpx 去读状态码。

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
			var se *HTTPStatusError
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
	methods := map[string]func(h *Service, url string) error{
		"GetJSON": func(h *Service, url string) error {
			return h.HTTP().GetJSON(context.Background(), url, nil)
		},
		"GetJSONWithHeader": func(h *Service, url string) error {
			return h.HTTP().GetJSONWithHeader(context.Background(), url, map[string]string{"Authorization": "ApiKey x"}, nil)
		},
		"GetJSONOnce": func(h *Service, url string) error {
			return h.HTTP().GetJSONOnce(context.Background(), url, nil, nil)
		},
		"PostJSON": func(h *Service, url string) error {
			return h.HTTP().PostJSON(context.Background(), url, map[string]any{"a": 1}, nil)
		},
	}

	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(t, http.StatusForbidden, rejectBody)
			defer srv.Close()

			h, _ := testHTTPHost(t, srv, httpx.Options{Timeout: time.Second, Retries: 0})
			err := call(h, srv.URL)

			var se *HTTPStatusError
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
	h := NewService(Options{Name: "probe", HTTP: hc, Domains: []string{"127.0.0.1"}})

	callErr := h.HTTP().GetJSONOnce(context.Background(), "https://"+addr+"/x", nil, nil)
	if callErr == nil {
		t.Fatal("连接失败应返回错误")
	}
	var se *HTTPStatusError
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
	var se *HTTPStatusError
	if errors.As(err, &se) {
		t.Fatalf("ctx 超时不得被包装成 *host.HTTPStatusError，实际 %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx 超时应原样保留 DeadlineExceeded，实际 %v", err)
	}
}

func TestAPIVersionIsNine(t *testing.T) {
	// 版本号就是「插件能依赖什么」的声明：Yield 取代 Broadcast、Event.Command/Args
	// 与前缀词边界、Event.Mentioned / Raw / RawTruncated 等契约变更必须体现在这里，
	// 否则插件按旧版本声明的能力去读新字段，核心却可能没提供。
	// 7 引入 *HTTPStatusError，8 引入上面那一批（#11 / #12 / #13），
	// 9 引入 Host.Session() 与 Event.Session（ADR-0004）。
	if APIVersion != "9" {
		t.Fatalf("APIVersion=%q，期望 \"9\"", APIVersion)
	}
}
