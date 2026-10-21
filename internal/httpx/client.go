// Package httpx 是核心层唯一的出站 HTTP 客户端。
//
// 设计约束（ARCHITECTURE.md §6.2）：
//   - 连接复用、超时、有限重试、全抖动指数退避；
//   - 域名白名单由插件的 Manifest 声明，未声明一律拒绝；
//   - 按域名分桶的进程级限流，插件不得自带 client，否则限流不可观测。
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxBodyBytes = 8 << 20 // 单次响应读取上限，防慢速大文件打满内存

// maxRedirects 与 net/http 的默认策略一致（10 跳）。自己实现 CheckRedirect
// 就必须自己带上这个上限，否则等于把默认策略整个丢掉。
const maxRedirects = 10

type Options struct {
	Timeout    time.Duration
	Retries    int
	RatePerMin int

	// DomainRatePerMin 为特定主机名配置独立额度（每分钟请求数）。
	// 键按主机名精确匹配、大小写不敏感；未列出的域名走 RatePerMin。
	// 配了独立额度的域名**只**用它，不与默认桶叠加——叠加等于没有放宽。
	// 零或负数的条目被忽略并告警，回退默认值（不做"零等于无限"这种隐式语义）。
	DomainRatePerMin map[string]int

	// HTTPClient 注入传输层实现，nil 时由 New 自己构造。
	//
	// 用途有二：测试可以注入信任自签证书的客户端；将来接代理或双向 TLS
	// 时不必再改构造函数。注入后 Timeout 不再生效——调用方给的客户端的
	// 参数被原样沿用（连接池、超时、自带的 CheckRedirect），但会拷一份
	// 再挂上本包的重定向策略，不会改动调用方的对象；WithTimeout 仍会基于
	// 它的 Transport 重建。
	HTTPClient *http.Client

	// Log 用于告警被忽略的非法域名额度；nil 时静默。
	Log *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	if o.RatePerMin <= 0 {
		o.RatePerMin = 60
	}
	return o
}

type Client struct {
	hc         *http.Client
	retries    int
	buckets    *bucketSet
	allowed    []string // 仅在 restricted=true 时生效
	restricted bool     // true = 该客户端属于插件，未声明域名一律拒绝
	disableTPS bool

	// nextRedirect 是注入 client 自带的 CheckRedirect（通常为 nil）。
	//
	// 克隆时原样继承、而不是叠加父 Client 的策略：插件 client 是 WithDomains
	// 从根客户端克隆出来的，父的白名单可能与本克隆的不同，叠加会把"父没声明
	// 的域名"也拒掉。本包自己的校验永远只有一份（见 newRequestClient）。
	nextRedirect func(*http.Request, []*http.Request) error
}

// New 构造核心客户端（进程内唯一的根客户端）。
//
// 插件客户端都由它派生（WithDomains / WithTimeout），因此连接池与桶天然共享。
// 不要为业务再 New 一个根客户端：那是第二份额度。
func New(o Options) *Client {
	o = o.withDefaults()
	hc := o.HTTPClient
	if hc == nil {
		tr := &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		hc = &http.Client{Transport: tr, Timeout: o.Timeout}
	}
	c := &Client{
		retries:      o.Retries,
		buckets:      newBucketSet(o.RatePerMin, o.DomainRatePerMin, o.Log),
		nextRedirect: hc.CheckRedirect,
	}
	c.hc = c.newRequestClient(hc)
	return c
}

// newRequestClient 复用 hc 的连接池与超时设置，但把重定向策略绑到**当前**
// Client 上。
//
// 不能原样共享父 client：CheckRedirect 是挂在 *http.Client 上的函数值，
// WithDomains 克隆只换了 allowed，闭包却仍指向父 client 的空白名单——
// 每一跳都会按"核心客户端"放行，P2-9 原样复发。
func (c *Client) newRequestClient(hc *http.Client) *http.Client {
	out := *hc
	next := c.nextRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := c.checkRedirect(req, via); err != nil {
			return err
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
	return &out
}

// DomainLimits 返回实际生效的域名独立额度（非法条目已剔除），供启动日志核对。
func (c *Client) DomainLimits() map[string]int {
	return c.buckets.limits()
}

// WithDomains 返回共享连接池与限流器的副本，只替换白名单。
// 调用后该客户端进入"受限模式"：白名单为空即拒绝一切出站请求。
// 每个插件拿到的都是自己的一份，互不影响。
func (c *Client) WithDomains(domains []string) *Client {
	clone := *c
	clone.allowed = domains
	clone.restricted = true
	clone.hc = clone.newRequestClient(c.hc)
	return &clone
}

// WithTimeout 返回共享连接池与限流器、只替换整体超时的副本。
//
// 用来给大请求体（富媒体上传）放宽窗口：http.Client.Timeout 是硬上限，
// context 只能收紧不能放宽，所以必须另起一个 Client。
func (c *Client) WithTimeout(d time.Duration) *Client {
	clone := *c
	clone.hc = clone.newRequestClient(c.hc)
	clone.hc.Timeout = d
	return &clone
}

// HTTPError 表示非 2xx 响应，调用方可据此区分限流/业务错误。
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, e.Body)
}

// IsStatus 判断错误是否为指定状态码。
func IsStatus(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == code
}

// RedirectError 表示重定向被本地策略拒绝：请求**没有**跟过去。
//
// 用独立类型而不是裸 error，调用方才能把"被白名单拦下"与"网络失败"分开：
// 后者重试有意义，前者重试只会再撞同一堵墙。
type RedirectError struct {
	URL    string // 被拒绝的那一跳（重定向的目标）
	From   string // 发起重定向的那一跳
	Reason string // 人读原因
	Err    error  // 具体判定失败的原因（可能是 checkParsedURL 的错误）
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("重定向被拒绝（%s -> %s）：%s", e.From, e.URL, e.Reason)
}

func (e *RedirectError) Unwrap() error { return e.Err }

func (c *Client) GetJSON(ctx context.Context, rawURL string, out any) error {
	return c.do(ctx, http.MethodGet, rawURL, nil, nil, out)
}

func (c *Client) PostJSON(ctx context.Context, rawURL string, in, out any) error {
	return c.PostJSONWithHeader(ctx, rawURL, nil, in, out)
}

// GetJSONWithHeader 允许调用方附加鉴权头（如 Authorization: QQBot <token>）。
func (c *Client) GetJSONWithHeader(ctx context.Context, rawURL string, header map[string]string, out any) error {
	return c.do(ctx, http.MethodGet, rawURL, header, nil, out)
}

func (c *Client) PostJSONWithHeader(ctx context.Context, rawURL string, header map[string]string, in, out any) error {
	body, err := encode(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, rawURL, header, body, out)
}

// PostJSONOnce 只尝试一次，不重试。
//
// 非幂等写操作（发消息）必须用它：HTTP 超时/5xx 时请求可能已经到达平台，
// 自动重试会造成重复发送；主动消息没有 msg_id 幂等键，平台侧无法去重。
// 429 也不在这里重试——它需要上层按维度退避并告警。
func (c *Client) PostJSONOnce(ctx context.Context, rawURL string, header map[string]string, in, out any) error {
	body, err := encode(in)
	if err != nil {
		return err
	}
	return c.doWithRetries(ctx, http.MethodPost, rawURL, header, body, out, 0)
}

// GetJSONOnce 只尝试一次，不重试。
//
// 给"配额由调用方自己记账"的场景用：出站层的隐式重试不经过业务配额，
// 按本地为准的账会被放大 2–3 倍。调用方愿意重试时自己决定节奏与退避。
func (c *Client) GetJSONOnce(ctx context.Context, rawURL string, header map[string]string, out any) error {
	return c.doWithRetries(ctx, http.MethodGet, rawURL, header, nil, out, 0)
}

func encode(in any) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("编码请求体: %w", err)
	}
	return body, nil
}

func (c *Client) do(ctx context.Context, method, rawURL string, header map[string]string, body []byte, out any) error {
	return c.doWithRetries(ctx, method, rawURL, header, body, out, c.retries)
}

func (c *Client) doWithRetries(ctx context.Context, method, rawURL string, header map[string]string, body []byte, out any, retries int) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("非法 URL: %w", err)
	}
	if err := c.checkParsedURL(u); err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return err
			}
		}
		if err := c.buckets.wait(ctx, host); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("构造请求: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 重定向被本地策略拒绝：重试只会再撞同一堵墙（还把源站多打几次），
			// 直接返回可判定的错误。
			var re *RedirectError
			if errors.As(err, &re) {
				return err
			}
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = &HTTPError{Status: resp.StatusCode, Body: truncate(data)}
			continue // 可重试
		}
		if resp.StatusCode >= 400 {
			return &HTTPError{Status: resp.StatusCode, Body: truncate(data)}
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析响应: %w", err)
		}
		return nil
	}
	return lastErr
}

func (c *Client) checkURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("非法 URL: %w", err)
	}
	return c.checkParsedURL(u)
}

func (c *Client) checkParsedURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("仅允许 https，拒绝 %s", u.Scheme)
	}
	if !c.restricted {
		return nil
	}
	if len(c.allowed) == 0 {
		return fmt.Errorf("插件未在 Manifest.AllowedDomains 中声明任何域名")
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range c.allowed {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return nil
		}
	}
	return fmt.Errorf("域名 %s 未在插件 Manifest 中声明", host)
}

// checkRedirect 对**每一跳**重跑入口校验。
//
// 白名单是"这次请求可以去哪"的承诺，不是"第一个 URL 合法"的一次性盖章：
// 被声明的域名一被攻陷或存在开放重定向，就能把请求带去 127.0.0.1:8500
// 或 169.254.169.254，插件隔离随之失效（SSRF）。
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	var from *url.URL
	if len(via) > 0 {
		from = via[len(via)-1].URL
	}
	fromStr := ""
	if from != nil {
		fromStr = from.String()
	}
	if len(via) >= maxRedirects {
		return &RedirectError{
			From: fromStr, URL: req.URL.String(),
			Reason: fmt.Sprintf("重定向超过 %d 跳", maxRedirects),
		}
	}
	// 任何 scheme 变化都拒绝，包括 http→https：
	//   - "只允许 https" 是入口校验的一部分，一次重定向就改掉它，等于让被
	//     声明的域名替另一个协议背书；
	//   - 实际只可能发生 https→http（降级即明文出网），那本来也被
	//     checkParsedURL 拦住，这里显式判一次是为了让错误说清"是重定向改写了
	//     scheme"，而不是笼统的"非 https"。
	if from != nil && req.URL.Scheme != from.Scheme {
		return &RedirectError{
			From: fromStr, URL: req.URL.String(),
			Reason: fmt.Sprintf("拒绝 scheme 变化 %s→%s", from.Scheme, req.URL.Scheme),
		}
	}
	if err := c.checkParsedURL(req.URL); err != nil {
		return &RedirectError{From: fromStr, URL: req.URL.String(), Reason: err.Error(), Err: err}
	}
	return nil
}

func backoff(attempt int) time.Duration {
	base := time.Second << uint(attempt-1) // 1s,2s,4s...
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	// 全抖动：避免多个插件同时重试打崩上游
	return time.Duration(rand.Int63n(int64(base))) + base/2
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// bucketSet 是按域名分的进程级限流器集合。
//
// 构造完成后只读（map 不再增删），因此并发读无需加锁；每个 limiter 自带互斥。
// 克隆客户端只复制指针，不复制桶——同域名下的多个插件必须共享同一份额度。
type bucketSet struct {
	def    *limiter
	byHost map[string]*limiter
	rates  map[string]int // 归一化后的生效额度，供启动日志核对
}

func newBucketSet(defPerMin int, domainRatePerMin map[string]int, log *slog.Logger) *bucketSet {
	s := &bucketSet{
		def:    newLimiter(defPerMin),
		byHost: make(map[string]*limiter, len(domainRatePerMin)),
		rates:  make(map[string]int, len(domainRatePerMin)),
	}
	for host, perMin := range domainRatePerMin {
		h := strings.ToLower(strings.TrimSpace(host))
		if h == "" {
			if log != nil {
				log.Warn("出站限流：域名额度条目为空，已忽略", "rate_per_min", perMin, "default", defPerMin)
			}
			continue
		}
		if perMin <= 0 {
			// 不做"零等于无限"：一次笔误不该让限流失效，也不该把机器人卡死。
			if log != nil {
				log.Warn("出站限流：域名额度非法，回退默认值",
					"domain", h, "rate_per_min", perMin, "default", defPerMin)
			}
			continue
		}
		s.byHost[h] = newLimiter(perMin)
		s.rates[h] = perMin
	}
	return s
}

// wait 取该域名对应的桶。未配置的域名走默认桶；配置了独立额度的域名
// 只用独立桶，不与默认桶叠加。
func (s *bucketSet) wait(ctx context.Context, host string) error {
	l := s.def
	if hl, ok := s.byHost[host]; ok {
		l = hl
	}
	return l.wait(ctx)
}

func (s *bucketSet) limits() map[string]int {
	out := make(map[string]int, len(s.rates))
	for h, n := range s.rates {
		out[h] = n
	}
	return out
}

// limiter 是简单令牌桶，按每分钟速率匀速补充。
type limiter struct {
	mu     sync.Mutex
	rate   float64 // token / second
	burst  float64
	tokens float64
	last   time.Time
}

func newLimiter(perMin int) *limiter {
	rate := float64(perMin) / 60.0
	burst := float64(perMin) / 6.0
	if burst < 1 {
		burst = 1
	}
	return &limiter{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()

		if err := sleep(ctx, need); err != nil {
			return err
		}
	}
}
