package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/httpx"
)

const tokenPath = "/token"

// authTransport 模拟平台鉴权：token 接口每次发一个新 token，业务接口只认
// **最近一次发出去的**那个，旧 token 一律 401。
//
// 这样"401 之后有没有拿新 token 重发"是可观测的行为，而不是靠内部状态推断：
// 断言的就是平台实际收到的 Authorization 头。
type authTransport struct {
	mu         sync.Mutex
	seq        int
	current    string // 平台当前认可的 token；空 = 谁都拒
	tokenCalls int
	apiCalls   map[string]int
	auths      map[string][]string // 业务路径 → 收到的 Authorization，按时间序
	failAlways bool                // 业务接口永远 401：换 token 也救不回来
	bodies     map[string]string
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if req.URL.Path == tokenPath {
		a.seq++
		a.tokenCalls++
		a.current = fmt.Sprintf("tok-%d", a.seq)
		return jsonResponse(http.StatusOK,
			fmt.Sprintf(`{"access_token":%q,"expires_in":7200}`, a.current)), nil
	}

	path := req.URL.Path
	a.apiCalls[path]++
	auth := req.Header.Get("Authorization")
	a.auths[path] = append(a.auths[path], auth)

	if a.failAlways || a.current == "" || auth != "QQBot "+a.current {
		return jsonResponse(http.StatusUnauthorized, `{"message":"invalid token"}`), nil
	}
	if b, ok := a.bodies[path]; ok {
		return jsonResponse(http.StatusOK, b), nil
	}
	return jsonResponse(http.StatusOK, `{"err_code":0}`), nil
}

// revoke 模拟平台提前吊销 token（密钥轮换、时钟偏差、平台侧失效）。
// 此后只有客户端**新换**到的 token 才会被接受。
func (a *authTransport) revoke() {
	a.mu.Lock()
	a.current = ""
	a.mu.Unlock()
}

func (a *authTransport) stats(path string) (calls, tokenCalls int, auths []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apiCalls[path], a.tokenCalls, append([]string(nil), a.auths[path]...)
}

// fakeState 是内存版 bot_state，额外记录被删过的键。
type fakeState struct {
	mu      sync.Mutex
	kv      map[string]string
	deleted []string
}

func (s *fakeState) GetState(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[key]
	return v, ok, nil
}

func (s *fakeState) PutState(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = value
	return nil
}

func (s *fakeState) DeleteState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kv, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *fakeState) seed(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = value
}

func (s *fakeState) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[key]
	return v, ok
}

func (s *fakeState) wasDeleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.deleted {
		if k == key {
			return true
		}
	}
	return false
}

func newAuthTestAPI(t *testing.T, bodies map[string]string) (*Client, *authTransport, *fakeState) {
	t.Helper()
	at := &authTransport{
		apiCalls: map[string]int{},
		auths:    map[string][]string{},
		bodies:   bodies,
	}
	st := &fakeState{kv: map[string]string{}}
	hc := httpx.New(httpx.Options{
		Retries:    3,
		RatePerMin: 60000,
		HTTPClient: &http.Client{Transport: at},
	})
	c := New(Options{
		HTTP:     hc,
		BaseURL:  "https://api.example.com",
		TokenURL: "https://bots.example.com" + tokenPath,
		AppID:    "app",
		Secret:   "secret",
		State:    st,
		Log:      slog.New(slog.DiscardHandler),
	})
	return c, at, st
}

// 401 之后必须换新 token 重试一次，而不是把 401 直接抛给上层：
// 平台提前吊销 token 时，上层看到的是"发送失败"，用户什么都没收到。
func TestAuthEndpointsRetryOnceWithFreshTokenAfter401(t *testing.T) {
	cases := []struct {
		name string
		path string
		do   func(*Client, context.Context) error
	}{
		{
			name: "Get",
			path: "/v2/groups/g1/info",
			do: func(c *Client, ctx context.Context) error {
				_, err := c.GroupInfo(ctx, "g1")
				return err
			},
		},
		{
			name: "Post",
			path: "/v2/groups/g1/messages",
			do: func(c *Client, ctx context.Context) error {
				var out struct{ ID string }
				return c.Post(ctx, "/v2/groups/g1/messages", MessageBody{Content: "hi"}, &out)
			},
		},
		{
			name: "postOnce",
			path: "/v2/groups/g1/messages",
			do: func(c *Client, ctx context.Context) error {
				return c.SendMessage(ctx, "group", "g1", MessageBody{Content: "hi"})
			},
		},
		{
			// 上传走 hcSlow（30s 超时）：重试必须还在这个 client 上，
			// 否则大文件会退回 5s 默认超时，必然超时。
			name: "postOnceSlow",
			path: "/v2/users/u1/files",
			do: func(c *Client, ctx context.Context) error {
				_, err := c.UploadMedia(ctx, "c2c", "u1", MediaUpload{})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies := map[string]string{
				tc.path: `{"err_code":0,"id":"m-1","group_name":"测试群","file_info":"info-1"}`,
			}
			c, at, st := newAuthTestAPI(t, bodies)
			ctx := context.Background()

			if _, err := c.AccessToken(ctx); err != nil {
				t.Fatalf("预热取 token 失败: %v", err)
			}
			at.revoke() // tok-1 从此被判 401

			if err := tc.do(c, ctx); err != nil {
				t.Fatalf("401 后应换新 token 重试成功，实际 err=%v", err)
			}

			calls, tokenCalls, auths := at.stats(tc.path)
			if calls != 2 {
				t.Fatalf("应恰好请求 2 次（原请求 + 换 token 后重试），实际 %d 次", calls)
			}
			if tokenCalls != 2 {
				t.Errorf("应恰好换 2 次 token（预热 + 401 后），实际 %d 次", tokenCalls)
			}
			if len(auths) != 2 || auths[0] != "QQBot tok-1" || auths[1] != "QQBot tok-2" {
				t.Fatalf("第二次必须带新 token，实际 Authorization 序列 = %v", auths)
			}
			// 只清内存不删库，下一个请求又会把坏 token 读回来——必须连持久化一起删。
			if !st.wasDeleted("access_token") || !st.wasDeleted("access_token_exp") {
				t.Errorf("作废必须删除持久化的 access_token/access_token_exp，实际删除记录 = %v", st.deleted)
			}
		})
	}
}

// 进程重启后从 bot_state 读回一个已被吊销的 token：这是 P1-2 的真实入口。
// 只清内存的实现在这里会第二次又读到同一个坏 token，然后 401 到底。
func TestRevokedPersistedTokenIsReplacedNotReadBack(t *testing.T) {
	c, at, st := newAuthTestAPI(t, map[string]string{
		"/v2/groups/g1/info": `{"err_code":0,"group_name":"测试群"}`,
	})
	st.seed("access_token", "tok-stale")
	st.seed("access_token_exp", time.Now().Add(time.Hour).Format(time.RFC3339))

	if _, err := c.GroupInfo(context.Background(), "g1"); err != nil {
		t.Fatalf("库里读回的 token 被判 401 后应换新 token，实际 err=%v", err)
	}

	calls, tokenCalls, auths := at.stats("/v2/groups/g1/info")
	if calls != 2 {
		t.Fatalf("应请求 2 次，实际 %d 次", calls)
	}
	if tokenCalls != 1 {
		t.Errorf("库里的 token 仍可用时不该先换 token，只该在 401 后换一次，实际 %d 次", tokenCalls)
	}
	if auths[1] != "QQBot tok-1" {
		t.Fatalf("重试必须用新换到的 token，实际 %v", auths)
	}
	if v, ok := st.get("access_token"); !ok || v != "tok-1" {
		t.Errorf("持久化应被新 token 覆盖，实际 %q (ok=%v)", v, ok)
	}
}

// 换新 token 之后仍然 401（权限被回收、appid 被停用）：只重试一次就报错，
// 不能无限重试——那会把 token 接口打成热点，还会把失败拖成超时。
func Test401AfterRefreshReturnsErrorWithoutFurtherRetry(t *testing.T) {
	c, at, _ := newAuthTestAPI(t, nil)
	at.failAlways = true

	err := c.SendMessage(context.Background(), "group", "g1", MessageBody{Content: "hi"})
	if !httpx.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("仍 401 时应返回 401 错误，实际 %v", err)
	}
	if calls, tokenCalls, _ := at.stats("/v2/groups/g1/messages"); calls != 2 || tokenCalls != 2 {
		t.Fatalf("应只重试一次（2 次业务请求 / 2 次换 token），实际 %d / %d", calls, tokenCalls)
	}
}

// 多个请求同时撞上 401 时只该换一次 token：每个 401 都去换 token 会把
// token 接口打成热点，且晚到的请求会把别人刚换好的 token 又作废掉。
func TestConcurrent401RefreshesTokenOnce(t *testing.T) {
	const n = 20
	c, at, _ := newAuthTestAPI(t, map[string]string{
		"/v2/groups/g1/info": `{"err_code":0,"group_name":"测试群"}`,
	})
	ctx := context.Background()
	if _, err := c.AccessToken(ctx); err != nil {
		t.Fatalf("预热取 token 失败: %v", err)
	}
	at.revoke()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GroupInfo(ctx, "g1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发请求 %d 应换新 token 后成功，实际 err=%v", i, err)
		}
	}
	if _, tokenCalls, _ := at.stats("/v2/groups/g1/info"); tokenCalls != 2 {
		t.Fatalf("N 个并发 401 只该换 1 次 token（预热 1 次 + 刷新 1 次），实际 %d 次", tokenCalls)
	}
}

// InvalidateToken 是网关 4004（鉴权失败）的共用入口：它没有请求 ctx，
// 也必须把持久化的 token 一起删掉。
func TestInvalidateTokenDeletesPersistedToken(t *testing.T) {
	c, at, st := newAuthTestAPI(t, nil)
	ctx := context.Background()

	if _, err := c.AccessToken(ctx); err != nil {
		t.Fatalf("预热取 token 失败: %v", err)
	}
	if _, ok := st.get("access_token"); !ok {
		t.Fatal("预热后应已持久化 access_token")
	}

	c.InvalidateToken()

	if _, ok := st.get("access_token"); ok {
		t.Error("作废后不该在库里留下 access_token")
	}
	if _, ok := st.get("access_token_exp"); ok {
		t.Error("作废后不该在库里留下 access_token_exp")
	}
	// 作废之后必须真的重新换：若还能从缓存/库里拿到旧 token，作废形同虚设。
	if _, err := c.AccessToken(ctx); err != nil {
		t.Fatalf("作废后应重新换 token: %v", err)
	}
	if _, tokenCalls, _ := at.stats(""); tokenCalls != 2 {
		t.Fatalf("作废后应重新换 1 次 token，实际共 %d 次", tokenCalls)
	}
}
