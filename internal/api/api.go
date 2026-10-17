// Package api 封装 QQ 开放平台 REST 接口：换 token、取网关地址、发消息。
//
// 只有这个包知道平台 URL 与鉴权头格式；gateway / core 都通过它出站。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/httpx"
)

// StateStore 是 token / session 的持久化接口，由 store.DB 实现。
// 单独抽接口是为了让 api 包不直接依赖 store，便于测试。
type StateStore interface {
	GetState(ctx context.Context, key string) (string, bool, error)
	PutState(ctx context.Context, key, value string) error
}

type Client struct {
	hc       *httpx.Client
	base     string
	tokenURL string
	appID    string
	secret   string
	state    StateStore
	log      *slog.Logger

	mu    sync.Mutex
	token string
	exp   time.Time
}

type Options struct {
	HTTP     *httpx.Client
	BaseURL  string
	TokenURL string
	AppID    string
	Secret   string
	State    StateStore
	Log      *slog.Logger
}

func New(o Options) *Client {
	logger := o.Log
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		hc:       o.HTTP,
		base:     strings.TrimRight(o.BaseURL, "/"),
		tokenURL: o.TokenURL,
		appID:    o.AppID,
		secret:   o.Secret,
		state:    o.State,
		log:      logger,
	}
}

// tokenResponse 里 expires_in 平台有时返回字符串、有时返回数字，做兼容。
type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Code        int             `json:"code"`
	Message     string          `json:"message"`
}

func (r *tokenResponse) expires() time.Duration {
	if len(r.ExpiresIn) == 0 {
		return 7200 * time.Second
	}
	var n int64
	if err := json.Unmarshal(r.ExpiresIn, &n); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	var s string
	if err := json.Unmarshal(r.ExpiresIn, &s); err == nil {
		var parsed int64
		if _, err := fmt.Sscanf(s, "%d", &parsed); err == nil && parsed > 0 {
			return time.Duration(parsed) * time.Second
		}
	}
	return 7200 * time.Second
}

// AccessToken 返回缓存中的 token，过期前 60 s 主动刷新。
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.exp.Add(-60*time.Second)) {
		return c.token, nil
	}
	// 进程重启后先尝试复用持久化的 token，能省一次换取请求
	if c.token == "" && c.state != nil {
		if tok, ok, err := c.state.GetState(ctx, "access_token"); err == nil && ok {
			if exp, ok2, _ := c.state.GetState(ctx, "access_token_exp"); ok2 {
				if t, err := time.Parse(time.RFC3339, exp); err == nil && time.Now().Before(t.Add(-60*time.Second)) {
					c.token, c.exp = tok, t
					return tok, nil
				}
			}
		}
	}

	body := map[string]string{"appId": c.appID, "clientSecret": c.secret}
	var resp tokenResponse
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.hc.PostJSON(reqCtx, c.tokenURL, body, &resp); err != nil {
		return "", fmt.Errorf("获取 access_token: %w", err)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("获取 access_token 失败: code=%d message=%s", resp.Code, resp.Message)
	}

	c.token = resp.AccessToken
	c.exp = time.Now().Add(resp.expires())
	if c.state != nil {
		if err := c.state.PutState(ctx, "access_token", c.token); err != nil {
			c.log.Warn("持久化 access_token 失败", "err", err)
		}
		if err := c.state.PutState(ctx, "access_token_exp", c.exp.Format(time.RFC3339)); err != nil {
			c.log.Warn("持久化 token 过期时间失败", "err", err)
		}
	}
	c.log.Info("已刷新 access_token", "expires_at", c.exp.Format(time.RFC3339))
	return c.token, nil
}

// InvalidateToken 在收到 401/鉴权错误时强制下次重取。
func (c *Client) InvalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

type gatewayResponse struct {
	URL string `json:"url"`
}

// GatewayURL 取 WSS 接入地址。
func (c *Client) GatewayURL(ctx context.Context) (string, error) {
	var resp gatewayResponse
	if err := c.Get(ctx, "/gateway/bot", &resp); err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", fmt.Errorf("网关地址为空")
	}
	return resp.URL, nil
}

// Get 发起带鉴权的 GET 请求。
func (c *Client) Get(ctx context.Context, path string, out any) error {
	tok, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{"Authorization": "QQBot " + tok}
	if err := c.hc.GetJSONWithHeader(ctx, c.base+path, h, out); err != nil {
		if httpx.IsStatus(err, 401) {
			c.InvalidateToken()
		}
		return err
	}
	return nil
}

// Post 发起带鉴权头的 POST（可重试，仅用于幂等写操作）。
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	tok, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{"Authorization": "QQBot " + tok}
	if err := c.hc.PostJSONWithHeader(ctx, c.base+path, h, in, out); err != nil {
		if httpx.IsStatus(err, 401) {
			c.InvalidateToken()
		}
		return err
	}
	return nil
}

// postOnce 用于发消息：不自动重试，避免非幂等写操作重复发送。
func (c *Client) postOnce(ctx context.Context, path string, in any) error {
	tok, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{"Authorization": "QQBot " + tok}
	if err := c.hc.PostJSONOnce(ctx, c.base+path, h, in, nil); err != nil {
		if httpx.IsStatus(err, 401) {
			c.InvalidateToken()
		}
		return err
	}
	return nil
}

// MessageBody 是发送消息的请求体。
type MessageBody struct {
	Content string `json:"content"`
	MsgType int    `json:"msg_type"`
	MsgID   string `json:"msg_id,omitempty"`
	MsgSeq  int    `json:"msg_seq,omitempty"`
	Image   string `json:"image,omitempty"`
}

// SendMessage 按场景分发到群/单聊接口。
// 频道场景当前未实现（ARCHITECTURE.md D3：频道作为可选扩展）。
func (c *Client) SendMessage(ctx context.Context, scene, peer string, body MessageBody) error {
	var path string
	switch scene {
	case "group":
		path = "/v2/groups/" + url.PathEscape(peer) + "/messages"
	case "c2c":
		path = "/v2/users/" + url.PathEscape(peer) + "/messages"
	default:
		return fmt.Errorf("暂不支持的消息场景: %s", scene)
	}
	return c.postOnce(ctx, path, body)
}
