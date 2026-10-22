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
	// DeleteState 用于作废 token：只清内存不删库，下一次又从库里读回
	// 同一个坏 token，全站在 exp 到期前（最长 7200s）鉴权失败。
	DeleteState(ctx context.Context, key string) error
}

// uploadTimeout 是富媒体上传的整体超时。
// 官方建议上传接口不少于 5 s，而 5 MB 的 base64 请求体在慢速上行下
// 5 s 传不完——默认的 [http].timeout 对上传太紧。
const uploadTimeout = 30 * time.Second

// BusinessError 表示平台**受理了 HTTP 请求但拒绝了业务**（err_code / code 非 0）。
//
// 官方调用指南：成功时 err_code 为 0；失败时响应体带 err_code，而 HTTP 状态
// 仍可能是 200/201/202。只看状态码就会把"平台拒绝"当成"已发送"——轨迹说发了、
// 配额扣了，用户什么都没收到，排障时所有日志都指向成功。
//
// Code 对一般接口来自 err_code；access-token 接口来自 code（该接口失败时
// HTTP 恒为 200）。调用方用 errors.As 判定它，据此与网络失败区分：
//
//	var be *api.BusinessError
//	if errors.As(err, &be) { /* 平台明确拒绝，重试无意义 */ }
//
// 网络错误、超时、非 2xx 响应都不会被包装成它。
type BusinessError struct {
	Code    int
	Message string
	// TraceID 是平台给的排障标识（响应体 trace_id），可能为空。
	TraceID string
}

func (e *BusinessError) Error() string {
	s := fmt.Sprintf("平台业务错误(err_code=%d): %s", e.Code, e.Message)
	if e.TraceID != "" {
		s += " [trace_id=" + e.TraceID + "]"
	}
	return s
}

// envelope 是平台响应里公共的错误信封。
//
// 只解 err_code，不含 code：access-token 用 code、其余接口用 err_code，字段名
// 不同但语义相同；混在一起会让"正常响应里恰好带 code"被误判成失败。
type envelope struct {
	ErrCode int    `json:"err_code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

// decodeEnvelope 校验信封，再把 body 解到 out（out 可为 nil）。
//
// out 为 nil 也必须解码：发消息的响应体里只有 id/timestamp/err_code，
// 那是唯一能判定"平台到底收没收"的地方，丢掉 body 等于放弃判据。
func decodeEnvelope(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("解析响应: %w", err)
	}
	if env.ErrCode != 0 {
		return &BusinessError{Code: env.ErrCode, Message: env.Message, TraceID: env.TraceID}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析响应: %w", err)
	}
	return nil
}

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
	// gen 是 token 的代际号：缓存内容每变一次（从库里装载、刷新、作废）就自增。
	// 请求拿 token 时记下它的代际号，401 时只有"缓存还是我失败的那一份"
	// 才真正作废——否则 N 个并发 401 会换 N 次 token，后到的还会把别人
	// 刚换好的 token 又删掉。
	gen uint64
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
	tok, _, err := c.accessToken(ctx)
	return tok, err
}

// accessToken 额外返回 token 的代际号，供 401 时判断该不该作废。
//
// 整个换取过程都在 mutex 里：同一时刻只有一个 goroutine 会去打 token 接口，
// 并发请求要么等它换好直接复用，要么读到新 token——不需要额外的 single-flight。
func (c *Client) accessToken(ctx context.Context) (string, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.exp.Add(-60*time.Second)) {
		return c.token, c.gen, nil
	}
	// 进程重启后先尝试复用持久化的 token，能省一次换取请求
	if c.token == "" && c.state != nil {
		if tok, ok, err := c.state.GetState(ctx, "access_token"); err == nil && ok {
			if exp, ok2, _ := c.state.GetState(ctx, "access_token_exp"); ok2 {
				if t, err := time.Parse(time.RFC3339, exp); err == nil && time.Now().Before(t.Add(-60*time.Second)) {
					c.token, c.exp = tok, t
					c.gen++
					return tok, c.gen, nil
				}
			}
		}
	}

	body := map[string]string{"appId": c.appID, "clientSecret": c.secret}
	var resp tokenResponse
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.hc.PostJSON(reqCtx, c.tokenURL, body, &resp); err != nil {
		return "", c.gen, fmt.Errorf("获取 access_token: %w", err)
	}
	// 这个接口失败时 HTTP 仍是 200，错误码在 code 字段里。
	if resp.Code != 0 {
		return "", c.gen, &BusinessError{Code: resp.Code, Message: resp.Message}
	}
	if resp.AccessToken == "" {
		return "", c.gen, fmt.Errorf("获取 access_token 失败: code=%d message=%s", resp.Code, resp.Message)
	}

	c.token = resp.AccessToken
	c.exp = time.Now().Add(resp.expires())
	c.gen++
	if c.state != nil {
		if err := c.state.PutState(ctx, "access_token", c.token); err != nil {
			c.log.Warn("持久化 access_token 失败", "err", err)
		}
		if err := c.state.PutState(ctx, "access_token_exp", c.exp.Format(time.RFC3339)); err != nil {
			c.log.Warn("持久化 token 过期时间失败", "err", err)
		}
	}
	c.log.Info("已刷新 access_token", "expires_at", c.exp.Format(time.RFC3339))
	return c.token, c.gen, nil
}

// InvalidateToken 作废当前 token：内存与持久化一起清，下次必然重新换取。
//
// 网关的 4004（鉴权失败）也走这里。**必须连 bot_state 一起删**：只清内存的话，
// 下一次 accessToken 会从库里把同一个坏 token 读回来（它的 exp 可能还在
// 未来 7200s），于是每次请求都 401、每次都"作废"同一份缓存，进程重启也无效。
func (c *Client) InvalidateToken() {
	// 作废不能受调用方取消影响：401 已经收到，删库失败就会把坏 token 留在库里。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 2*time.Second)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearTokenLocked(ctx)
}

// invalidateIfCurrent 只在缓存仍是 gen 这一代时作废，否则说明别的请求
// 已经作废并换好了新 token，这时候再清一次等于把新 token 丢掉。
func (c *Client) invalidateIfCurrent(ctx context.Context, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	c.clearTokenLocked(ctx)
}

func (c *Client) clearTokenLocked(ctx context.Context) {
	c.token = ""
	c.exp = time.Time{}
	c.gen++
	if c.state == nil {
		return
	}
	for _, key := range []string{"access_token", "access_token_exp"} {
		if err := c.state.DeleteState(ctx, key); err != nil {
			c.log.Warn("删除持久化的 token 失败，下次可能读回同一个坏 token", "key", key, "err", err)
		}
	}
}

// authHeader 构造平台鉴权头。
func authHeader(tok string) map[string]string {
	return map[string]string{"Authorization": "QQBot " + tok}
}

// doAuthed 带鉴权发一次请求；401 时作废 token、换新的再发一次。
//
// 只重试一次：401 是平台在鉴权阶段拒绝的，请求体没有被受理，所以连
// "本来就不重试"的非幂等写操作（发消息、上传）重发也不会重复落地。
// 连着两次 401 说明不是 token 陈旧（权限被回收、appid 被停用），
// 再试只会把失败拖成超时。
func (c *Client) doAuthed(ctx context.Context, do func(header map[string]string) error) error {
	tok, gen, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	err = do(authHeader(tok))
	if !httpx.IsStatus(err, 401) {
		return err
	}

	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	c.invalidateIfCurrent(ictx, gen)
	cancel()

	fresh, _, ferr := c.accessToken(ctx)
	if ferr != nil {
		c.log.Warn("401 后换取新 token 失败", "err", ferr)
		return ferr
	}
	return do(authHeader(fresh))
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

// Get 发起带鉴权的 GET 请求。401 时换新 token 重试一次。
func (c *Client) Get(ctx context.Context, path string, out any) error {
	var raw json.RawMessage
	err := c.doAuthed(ctx, func(h map[string]string) error {
		return c.hc.GetJSONWithHeader(ctx, c.base+path, h, &raw)
	})
	if err != nil {
		return err
	}
	return decodeEnvelope(raw, out)
}

// Post 发起带鉴权头的 POST（可重试，仅用于幂等写操作）。401 时换新 token 重试一次。
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	var raw json.RawMessage
	err := c.doAuthed(ctx, func(h map[string]string) error {
		return c.hc.PostJSONWithHeader(ctx, c.base+path, h, in, &raw)
	})
	if err != nil {
		return err
	}
	return decodeEnvelope(raw, out)
}

// postOnce 用于非幂等写操作：不因网络/5xx 重试，避免重复发送。
// 401 例外——平台在鉴权阶段就拒了，请求体没被受理，换新 token 重发是安全的。
func (c *Client) postOnce(ctx context.Context, path string, in, out any) error {
	return c.postOnceWith(ctx, c.hc, path, in, out)
}

// postOnceSlow 用更长的超时跑大请求体（富媒体上传）。
func (c *Client) postOnceSlow(ctx context.Context, path string, in, out any) error {
	return c.postOnceWith(ctx, c.hcSlow, path, in, out)
}

// postOnceWith 是两者共用的实现。重试次数恒为 0：非幂等写操作在超时后
// 可能已经落地，重试等于重复发送或重复上传。唯一的例外是 401 后换 token
// 重发一次（见 doAuthed），重发仍用调用方给的 hc——上传必须留在 hcSlow 上，
// 否则 5 MB 的请求体会被 5 s 默认超时掐死。
func (c *Client) postOnceWith(ctx context.Context, hc *httpx.Client, path string, in, out any) error {
	var raw json.RawMessage
	err := c.doAuthed(ctx, func(h map[string]string) error {
		return hc.PostJSONOnce(ctx, c.base+path, h, in, &raw)
	})
	if err != nil {
		return err
	}
	return decodeEnvelope(raw, out)
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
