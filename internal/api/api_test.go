package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/knight000/go_qqbot/internal/httpx"
)

// fakeTransport 让测试完全掌控响应体。
//
// api.Client 只认 httpx，而 httpx.Options.HTTPClient 可注入 *http.Client，
// 所以换掉 RoundTripper 就够了：不起真服务器，也不受证书与端口的干扰。
type fakeTransport struct {
	mu     sync.Mutex
	paths  []string
	bodies map[string]string
	errs   map[string]error // 按路径注入网络错误
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.paths = append(f.paths, req.URL.Path)
	err, hasErr := f.errs[req.URL.Path]
	body, ok := f.bodies[req.URL.Path]
	f.mu.Unlock()

	if hasErr {
		return nil, err
	}
	if !ok {
		// 未声明的路径一律 404：测试不该悄悄依赖"什么请求都能成功"。
		return jsonResponse(http.StatusNotFound, `{"message":"not found"}`), nil
	}
	return jsonResponse(http.StatusOK, body), nil
}

func (f *fakeTransport) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newTestAPI 构造一个指向 fakeTransport 的客户端；token 请求默认成功。
//
// Retries 故意开到 3：官方接口里 Get/Post 本是可重试的，业务错误若被当成
// "可重试的失败"，请求数会立刻露馅（见 TestGroupInfoBusinessErrorIsNotRetried）。
func newTestAPI(t *testing.T, bodies map[string]string) (*Client, *fakeTransport) {
	t.Helper()
	ft := &fakeTransport{bodies: map[string]string{
		"/app/getAppAccessToken": `{"access_token":"tok","expires_in":7200}`,
	}}
	for k, v := range bodies {
		ft.bodies[k] = v
	}
	hc := httpx.New(httpx.Options{
		Retries:    3,
		RatePerMin: 6000,
		HTTPClient: &http.Client{Transport: ft},
	})
	return New(Options{
		HTTP:     hc,
		BaseURL:  "https://api.example.com",
		TokenURL: "https://bots.example.com/app/getAppAccessToken",
		AppID:    "app",
		Secret:   "secret",
		Log:      slog.New(slog.DiscardHandler),
	}), ft
}

// 平台以 HTTP 200 + err_code 返回失败时，发送必须返回业务错误：
// 否则上层会记成"已发送"，扣掉配额却什么都没发出去。
func TestSendMessageReturnsBusinessError(t *testing.T) {
	c, ft := newTestAPI(t, map[string]string{
		"/v2/groups/g1/messages": `{"err_code":40034005,"message":"msg_id 已过期","trace_id":"trace-1"}`,
	})

	err := c.SendMessage(context.Background(), "group", "g1", MessageBody{Content: "hi"})
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("200 + err_code 应返回业务错误，实际 err=%v", err)
	}
	if be.Code != 40034005 {
		t.Errorf("err_code 应透出为 Code，实际 %d", be.Code)
	}
	if be.Message != "msg_id 已过期" {
		t.Errorf("message 应透出，实际 %q", be.Message)
	}
	// 排障时必须能拿 trace_id 找平台工单，所以它要出现在错误串里。
	if !strings.Contains(err.Error(), "trace-1") {
		t.Errorf("错误串应包含 trace_id，实际 %q", err.Error())
	}
	if n := ft.count("/v2/groups/g1/messages"); n != 1 {
		t.Fatalf("平台拒绝不该重试，实际请求 %d 次", n)
	}
}

// 上传同样会以 200 + err_code 被拒（file_data 不合规、文件类型不符等）；
// 漏判的后果是"图没传上去，但仍然去发一条没有 media 的消息"。
func TestUploadMediaReturnsBusinessError(t *testing.T) {
	c, _ := newTestAPI(t, map[string]string{
		"/v2/users/u1/files": `{"err_code":1100101,"message":"内容被风控"}`,
	})

	_, err := c.UploadMedia(context.Background(), "c2c", "u1", MediaUpload{})
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("200 + err_code 应返回业务错误，实际 err=%v", err)
	}
	if be.Code != 1100101 {
		t.Errorf("err_code 应透出为 Code，实际 %d", be.Code)
	}
}

// 取群名走的是可重试的 GET：业务错误必须原地返回，不能落进重试分支——
// 11253（仅白名单可用）重试一百次也还是 11253，只会白烧额度。
func TestGroupInfoBusinessErrorIsNotRetried(t *testing.T) {
	c, ft := newTestAPI(t, map[string]string{
		"/v2/groups/g1/info": `{"err_code":11253,"message":"仅白名单机器人可用","trace_id":"trace-2"}`,
	})

	_, err := c.GroupInfo(context.Background(), "g1")
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("200 + err_code 应返回业务错误，实际 err=%v", err)
	}
	if be.Code != 11253 {
		t.Errorf("err_code 应透出为 Code，实际 %d", be.Code)
	}
	if n := ft.count("/v2/groups/g1/info"); n != 1 {
		t.Fatalf("业务错误不该重试（Get 默认重试 3 次），实际请求 %d 次", n)
	}
}

// 成功响应的 err_code 为 0（有时干脆没有这个字段）：不能被误判成失败。
func TestSuccessResponseIsNotBusinessError(t *testing.T) {
	c, _ := newTestAPI(t, map[string]string{
		"/v2/groups/g1/messages": `{"id":"m-1","timestamp":1699000000,"err_code":0}`,
		"/v2/users/u1/files":     `{"file_uuid":"uuid-1","file_info":"info-1","ttl":300}`,
		"/v2/groups/g1/info":     `{"group_openid":"g1","group_name":"测试群","err_code":0}`,
	})
	ctx := context.Background()

	if err := c.SendMessage(ctx, "group", "g1", MessageBody{Content: "hi"}); err != nil {
		t.Fatalf("err_code=0 应视为成功: %v", err)
	}
	up, err := c.UploadMedia(ctx, "c2c", "u1", MediaUpload{})
	if err != nil {
		t.Fatalf("上传成功不应报错: %v", err)
	}
	if up.FileInfo != "info-1" {
		t.Errorf("响应体仍应解到 out，实际 file_info=%q", up.FileInfo)
	}
	info, err := c.GroupInfo(ctx, "g1")
	if err != nil {
		t.Fatalf("取群名成功不应报错: %v", err)
	}
	if info.GroupName != "测试群" {
		t.Errorf("响应体仍应解到 out，实际群名 %q", info.GroupName)
	}
}

// 网络失败不能被判成"平台拒绝"：前者发没发出去是未知的，后者是明确没发。
// 上层据此决定要不要归还配额，两者混为一谈会超发。
func TestNetworkErrorIsNotBusinessError(t *testing.T) {
	c, ft := newTestAPI(t, nil)
	connErr := errors.New("dial tcp 127.0.0.1:443: connect: connection refused")
	ft.errs = map[string]error{"/v2/groups/g1/messages": connErr}

	err := c.SendMessage(context.Background(), "group", "g1", MessageBody{Content: "hi"})
	if err == nil {
		t.Fatal("网络失败应返回错误")
	}
	var be *BusinessError
	if errors.As(err, &be) {
		t.Fatalf("网络失败被误判成平台拒绝: %v", err)
	}
	if !errors.Is(err, connErr) {
		t.Fatalf("应保留原始网络错误以便 errors.Is 判定，实际 %v", err)
	}
}

// access-token 接口失败时 HTTP 仍是 200，错误码在 code 字段（不是 err_code）。
func TestAccessTokenReturnsBusinessErrorUsingCode(t *testing.T) {
	c, _ := newTestAPI(t, map[string]string{
		"/app/getAppAccessToken": `{"code":100016,"message":"invalid appid or secret"}`,
	})

	tok, err := c.AccessToken(context.Background())
	if tok != "" {
		t.Errorf("失败时不应返回 token，实际 %q", tok)
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("200 + code 应返回业务错误，实际 err=%v", err)
	}
	if be.Code != 100016 {
		t.Errorf("code 应透出为 Code，实际 %d", be.Code)
	}
}

// 群与单聊的接口在平台侧完全隔离（连上传的富媒体都不通用），
// 路径走错就是发到另一个会话或直接 404，所以单独钉住这个映射。
func TestScenePath(t *testing.T) {
	cases := []struct {
		scene   string
		peer    string
		suffix  string
		want    string
		wantErr bool
	}{
		{scene: "group", peer: "g1", suffix: "messages", want: "/v2/groups/g1/messages"},
		{scene: "group", peer: "g1", suffix: "files", want: "/v2/groups/g1/files"},
		{scene: "c2c", peer: "u1", suffix: "messages", want: "/v2/users/u1/messages"},
		{scene: "c2c", peer: "u1", suffix: "files", want: "/v2/users/u1/files"},
		// openid 出现在路径里，必须转义：含 / 的 id 会把路径拼到别处去。
		{scene: "group", peer: "a b/c", suffix: "files", want: "/v2/groups/a%20b%2Fc/files"},
		{scene: "guild", peer: "x", suffix: "messages", wantErr: true},
		{scene: "", peer: "x", suffix: "files", wantErr: true},
	}
	for _, c := range cases {
		got, err := scenePath(c.scene, c.peer, c.suffix)
		if c.wantErr {
			if err == nil {
				t.Errorf("场景 %q 应报错，实际得到 %q", c.scene, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("场景 %q 不应报错: %v", c.scene, err)
			continue
		}
		if got != c.want {
			t.Errorf("场景 %q 路径应为 %q，实际 %q", c.scene, c.want, got)
		}
	}
}

// gateFunc 把函数适配成 Gate，测试里用闭包记录进出顺序。
type gateFunc func(ctx context.Context, fn func(context.Context) error) error

func (f gateFunc) Do(ctx context.Context, fn func(context.Context) error) error {
	return f(ctx, fn)
}

// 取网关地址必须过闸：这是启动错峰唯一的落点。闸门不生效时 N 个 Bot
// 同时启动会一起撞 GET /gateway 的 2 QPM，而症状只是"偶尔启动失败"。
func TestGatewayURLGoesThroughGate(t *testing.T) {
	c, ft := newTestAPI(t, map[string]string{
		"/gateway/bot": `{"url":"wss://gateway.example.com/ws"}`,
	})
	var calls int
	c.gate = gateFunc(func(ctx context.Context, fn func(context.Context) error) error {
		calls++
		return fn(ctx)
	})

	got, err := c.GatewayURL(context.Background())
	if err != nil {
		t.Fatalf("取网关地址: %v", err)
	}
	if got != "wss://gateway.example.com/ws" {
		t.Fatalf("网关地址 = %q", got)
	}
	if calls != 1 {
		t.Fatalf("GatewayURL 应过一次闸，实际 %d 次", calls)
	}
	if n := ft.count("/gateway/bot"); n != 1 {
		t.Fatalf("闸门放行后应请求一次 /gateway/bot，实际 %d 次", n)
	}
}

// 闸门自己报错（例如等间隔时收到关停信号）时必须原样返回，且不再发请求：
// 否则取消之后仍然打一次平台接口，关停就变成了"再多撞一次配额"。
func TestGatewayURLGateErrorSkipsRequest(t *testing.T) {
	c, ft := newTestAPI(t, map[string]string{
		"/gateway/bot": `{"url":"wss://gateway.example.com/ws"}`,
	})
	blocked := errors.New("正在错峰")
	c.gate = gateFunc(func(context.Context, func(context.Context) error) error {
		return blocked
	})

	if _, err := c.GatewayURL(context.Background()); !errors.Is(err, blocked) {
		t.Fatalf("闸门错误应原样返回，实际: %v", err)
	}
	if n := ft.count("/gateway/bot"); n != 0 {
		t.Fatalf("闸门拒绝后不该再请求 /gateway/bot，实际 %d 次", n)
	}
}
