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

	"github.com/knight000/go_qqbot/internal/httpx"
)

// StateStore 是 token / session 的持久化接口，由 store.DB 实现。
// 单独抽接口是为了让 api 包不直接依赖 store，便于测试。
type StateStore interface {
	GetState(ctx context.Context, key string) (string, bool, error)
	PutState(ctx context.Context, key, value string) error
}

// uploadTimeout 是富媒体上传的整体超时。
// 官方建议上传接口不少于 5 s，而 5 MB 的 base64 请求体在慢速上行下
// 5 s 传不完——默认的 [http].timeout 对上传太紧。
const uploadTimeout = 30 * time.Second

type Client struct {
	hc       *httpx.Client
	hcSlow   *httpx.Client // 共享连接池与限流器，只放宽超时
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
	hc := o.HTTP
	var hcSlow *httpx.Client
	if hc != nil {
		hcSlow = hc.WithTimeout(uploadTimeout)
	}
	return &Client{
		hc:       hc,
		hcSlow:   hcSlow,
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

// postOnce 用于非幂等写操作：不自动重试，避免重复发送。
func (c *Client) postOnce(ctx context.Context, path string, in, out any) error {
	return c.postOnceWith(ctx, c.hc, path, in, out)
}

// postOnceSlow 用更长的超时跑大请求体（富媒体上传）。
func (c *Client) postOnceSlow(ctx context.Context, path string, in, out any) error {
	return c.postOnceWith(ctx, c.hcSlow, path, in, out)
}

// postOnceWith 是两者共用的实现。重试次数恒为 0：非幂等写操作在超时后
// 可能已经落地，重试等于重复发送或重复上传。
func (c *Client) postOnceWith(ctx context.Context, hc *httpx.Client, path string, in, out any) error {
	tok, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{"Authorization": "QQBot " + tok}
	if err := hc.PostJSONOnce(ctx, c.base+path, h, in, out); err != nil {
		if httpx.IsStatus(err, 401) {
			c.InvalidateToken()
		}
		return err
	}
	return nil
}

// scenePath 拼按场景分类的接口路径：群走 /v2/groups/，单聊走 /v2/users/。
//
// 群与单聊的接口在平台侧完全隔离（连富媒体上传的文件都不通用），
// 所以路径只能由场景决定，不能靠调用方传。
func scenePath(scene, peer, suffix string) (string, error) {
	switch scene {
	case "group":
		return "/v2/groups/" + url.PathEscape(peer) + "/" + suffix, nil
	case "c2c":
		return "/v2/users/" + url.PathEscape(peer) + "/" + suffix, nil
	default:
		return "", fmt.Errorf("暂不支持的场景: %s", scene)
	}
}

// MediaRef 引用一次富媒体上传的结果，值是上传接口返回的 file_info。
// 平台要求原样透传，不要尝试解析或改写。
type MediaRef struct {
	FileInfo string `json:"file_info"`
}

// MessageBody 是发送消息的请求体。
type MessageBody struct {
	Content string    `json:"content"`
	MsgType int       `json:"msg_type"`
	MsgID   string    `json:"msg_id,omitempty"`
	MsgSeq  int       `json:"msg_seq,omitempty"`
	Media   *MediaRef `json:"media,omitempty"`
}

// 富媒体文件类型（file_type）。
const (
	MediaImage = 1 // png/jpg，软限 20 MB
	MediaVideo = 2 // 仅 mp4
	MediaVoice = 3 // silk/mp3/wav/ogg
	MediaFile  = 4 // 任意格式，按文件卡片展示
)

// MaxInlineMediaBytes 是「二进制直传」的原始字节上限。
//
// 平台规定 file_data 不超过 5 MB，超过必须走分片上传
// （upload_prepare → 逐片 PUT → upload_part_finish → 携带 upload_id 合并）。
// 分片上传当前未实现，所以调用方要在此之前把大小判掉。
const MaxInlineMediaBytes = 5 << 20

// MediaUpload 是 POST /v2/{groups|users}/{openid}/files 的请求体。
//
// URL 与 FileData 互斥：前者由平台下载转存，后者是 base64 编码的原始字节。
// SrvSendMsg 恒为 false——上传与发送分成两步，发送才能复用核心的频控、
// msg_seq 与「一条消息一种 msg_type」的约束。
type MediaUpload struct {
	FileType   int    `json:"file_type"`
	URL        string `json:"url,omitempty"`
	FileData   string `json:"file_data,omitempty"`
	FileName   string `json:"file_name,omitempty"`
	SrvSendMsg bool   `json:"srv_send_msg"`
}

// MediaUploadResult 是上传接口的响应。TTL 为 file_info 的有效期（秒），
// 0 表示长期有效；过期后必须重新上传。
type MediaUploadResult struct {
	FileUUID string `json:"file_uuid"`
	FileInfo string `json:"file_info"`
	TTL      int    `json:"ttl"`
}

// UploadMedia 上传富媒体，返回可用于发消息的 file_info。
func (c *Client) UploadMedia(ctx context.Context, scene, peer string, up MediaUpload) (MediaUploadResult, error) {
	path, err := scenePath(scene, peer, "files")
	if err != nil {
		return MediaUploadResult{}, err
	}
	if up.FileType == 0 {
		up.FileType = MediaImage
	}
	var out MediaUploadResult
	// 上传不重试：每次重试都会多存一份，占用平台每日上传容量。
	// 单次超时用更长的窗口——5 s 的默认值不够传完 5 MB。
	if err := c.postOnceSlow(ctx, path, up, &out); err != nil {
		return MediaUploadResult{}, err
	}
	if out.FileInfo == "" {
		return out, fmt.Errorf("上传响应缺少 file_info")
	}
	return out, nil
}

// SendMessage 按场景分发到群/单聊接口。
// 频道场景当前未实现（ARCHITECTURE.md D3：频道作为可选扩展）。
func (c *Client) SendMessage(ctx context.Context, scene, peer string, body MessageBody) error {
	path, err := scenePath(scene, peer, "messages")
	if err != nil {
		return err
	}
	return c.postOnce(ctx, path, body, nil)
}

// GroupInfo 是 GET /v2/groups/{group_openid}/info 的响应。
//
// 该接口目前标注为"仅白名单机器人可用"（错误码 11253），因此调用方必须
// 把失败当作正常分支处理：群名留空，由管理员用 /group name 手工补。
type GroupInfo struct {
	GroupOpenID    string   `json:"group_openid"`
	GroupName      string   `json:"group_name"`
	GroupMemo      string   `json:"group_finger_memo"`
	GroupClass     string   `json:"group_class_text"`
	GroupTags      []string `json:"group_tags"`
	GroupMemberNum int      `json:"group_member_num"`
}

// GroupInfo 获取群基本信息（主要是群名称）。
func (c *Client) GroupInfo(ctx context.Context, groupOpenID string) (GroupInfo, error) {
	var out GroupInfo
	if groupOpenID == "" {
		return out, fmt.Errorf("群 openid 为空")
	}
	err := c.Get(ctx, "/v2/groups/"+url.PathEscape(groupOpenID)+"/info", &out)
	return out, err
}
