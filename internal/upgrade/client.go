package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 默认地址。api_base 是元数据通道，asset_base 是二进制通道（决策 3）。
const (
	DefaultAPIBase   = "https://api.github.com"
	DefaultAssetBase = "https://github.com"
	DefaultRepo      = "knight000/go_qqbot"

	// MaxAssetBytes 是单个产物的上限。40 MB 的核心产物 + 交叉编译留足余量，
	// 但必须有天花板：没有它，一个被投毒/配错的元数据就能把磁盘写满。
	MaxAssetBytes = 128 << 20
)

// Release 是一次检查的结论：最新版本、插件清单，以及本平台核心产物的
// 名字、大小、校验和与**已经选好通道**的下载地址。
type Release struct {
	Tag      string
	Version  string
	Index    *Index
	CoreName string
	CoreSize int64
	CoreSHA  string
	CoreURL  string

	// coreID 是核心产物在 API 侧的资产 id。有 token 时走它。
	coreID int64
	// gh 是这次发布本身：插件产物要按资产名回查 id（有 token 时走 api 通道）。
	gh *ghRelease
}

const (
	// metaTimeout 覆盖元数据（三次 GET）。产物用 bodyTimeout。
	metaTimeout = 20 * time.Second
	// bodyTimeout 是单次产物传输的上限。它是"彻底卡死"的保险，
	// 不是速率限制：40 MB 在 56k 拨号上也能在这个期限内传完。
	bodyTimeout = 15 * time.Minute
)

// client 只认识 api_base / asset_base 两个地址，不共用 internal/httpx：
// 后者是插件出站通道（8 MiB 上限、只解 JSON、占插件的配额桶，见 ADR-0006
// 「为什么下载不走 httpx」）。
type client struct {
	apiBase   string
	assetBase string
	token     string
	repo      string
	meta      *http.Client
	body      *http.Client

	// tags 缓存按 tag 取过的发布：一次 /upgrade plugin all 里多个插件可能
	// 指向同一个旧 tag，只查一次（#52）。
	mu   sync.Mutex
	tags map[string]*ghRelease
}

func newClient(apiBase, assetBase, token, repo string) *client {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultAPIBase
	}
	if strings.TrimSpace(assetBase) == "" {
		assetBase = DefaultAssetBase
	}
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &client{
		apiBase:   strings.TrimRight(apiBase, "/"),
		assetBase: strings.TrimRight(assetBase, "/"),
		token:     strings.TrimSpace(token),
		repo:      strings.Trim(repo, "/"),
		meta:      &http.Client{Timeout: metaTimeout},
		body:      &http.Client{Timeout: bodyTimeout},
		tags:      map[string]*ghRelease{},
	}
}

// ghRelease 是 GitHub「最新发布」接口里本包用到的字段。
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (r *ghRelease) asset(name string) (id, size int64, ok bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.ID, a.Size, true
		}
	}
	return 0, 0, false
}

// checkIndex 只取发布 tag 与 index.json，不碰核心产物清单。
//
// 插件升级走这条：它只认 index.json（决策 11 说得很清楚——插件产物与
// index.json 的完整性由索引里的 sha256 提供，checksums.txt 只覆盖核心）。
// 用一个"发漏了本平台核心产物"的发布去拒绝插件升级，是把两条无关的路
// 硬绑在一起：那种发布恰恰是最需要先升插件的现场。
func (c *client) checkIndex(ctx context.Context) (*Release, error) {
	rel, err := c.latest(ctx)
	if err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("最新发布缺少 tag_name")
	}
	rawIndex, err := c.readAsset(ctx, rel, assetIndexName)
	if err != nil {
		return nil, err
	}
	idx, err := ParseIndex(strings.NewReader(rawIndex))
	if err != nil {
		return nil, err
	}
	return &Release{Tag: rel.TagName, Version: idx.Core.Version, Index: idx, gh: rel}, nil
}

// check 取元数据并拼出「要升到的那个版本」（核心升级用）。
//
// 通道语义（决策 3 + 私有仓扩展）：
//   - index.json 与 checksums.txt **永远**走 api_base；
//   - 没有 token 时二进制走 asset_base（可指向镜像）；
//   - 有 token 时三份产物全走 api_base 的 release asset 端点——私有仓的
//     资产无法在镜像上匿名取，而且 token 绝不能发给 asset_base 的主机。
func (c *client) check(ctx context.Context) (*Release, error) {
	out, err := c.checkIndex(ctx)
	if err != nil {
		return nil, err
	}
	tag, idx, rel := out.Tag, out.Index, out.gh

	rawSums, err := c.readAsset(ctx, rel, assetChecksumsName)
	if err != nil {
		return nil, err
	}
	sums, err := ParseChecksums(strings.NewReader(rawSums))
	if err != nil {
		return nil, err
	}

	name := CoreAssetName(idx.Core.Version, runtime.GOOS, runtime.GOARCH)
	sha, ok := sums[name]
	if !ok {
		return nil, fmt.Errorf("checksums.txt 里没有本平台的产物 %s：该发布可能漏发了平台", name)
	}
	id, size, ok := rel.asset(name)
	if !ok {
		return nil, fmt.Errorf("发布 %s 没有资产 %s：该发布可能漏发了平台", tag, name)
	}

	out.CoreName, out.CoreSize, out.CoreSHA, out.coreID = name, size, sha, id
	if c.token != "" {
		out.CoreURL = c.apiAssetURL(id)
	} else {
		out.CoreURL = c.mirrorURL(tag, name)
	}
	return out, nil
}

func (c *client) latest(ctx context.Context) (*ghRelease, error) {
	u := c.apiBase + "/repos/" + c.repo + "/releases/latest"
	body, err := c.get(ctx, c.meta, u, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析最新发布: %w", err)
	}
	return &rel, nil
}

// readAsset 按名字取发布里的一个小资产（index.json / checksums.txt）。
//
// 走 API 的资产端点而不是 browser_download_url：后者对私有仓是 302 到
// 一次性签名地址，绕过 api_base 就等于绕过了决策 3 的结构约束。
func (c *client) readAsset(ctx context.Context, rel *ghRelease, name string) (string, error) {
	id, _, ok := rel.asset(name)
	if !ok {
		return "", fmt.Errorf("发布 %s 没有资产 %s：请确认 CI 上传了它", rel.TagName, name)
	}
	body, err := c.get(ctx, c.meta, c.apiAssetURL(id), "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer body.Close()

	buf, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取 %s: %w", name, err)
	}
	return string(buf), nil
}

func (c *client) apiAssetURL(id int64) string {
	return fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.apiBase, c.repo, id)
}

// mirrorURL 是镜像通道的地址。owner/repo 必须出现在路径里：镜像站
// 只是把 https://github.com 换掉的前缀（决策 3）。
func (c *client) mirrorURL(tag, asset string) string {
	return c.assetBase + "/" + c.repo + "/releases/download/" + tag + "/" + asset
}

// get 发一次 GET，把非 2xx 变成能读懂的错误。
//
// Authorization 只加给 api_base 的主机：token 是私有仓凭据，asset_base
// 是用户可配置的镜像，把凭据发给它等于泄露（本包唯一的硬性约束）。
func (c *client) get(ctx context.Context, hc *http.Client, rawURL, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "qqbot-upgrade")
	if c.token != "" && c.sameHost(rawURL, c.apiBase) {
		req.Header.Set("Authorization", "Bearer "+c.token)
		// GitHub 对 release asset 端点要求这个头才返回二进制，
		// 否则回一段 JSON 元数据——两者都是 200，只有内容不对。
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s: %w", rawURL, err)
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("请求 %s 返回 %s%s", rawURL, resp.Status, hintFor(resp.StatusCode))
	}
	return resp.Body, nil
}

func (c *client) sameHost(rawURL, base string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, b.Host)
}

// hintFor 把最常见的几个状态码翻译成人话。失败文案是 owner 唯一能看到的
// 现场（决策「代价」一节：每条失败模式都要有明确的用户可见文案）。
func hintFor(code int) string {
	switch code {
	case http.StatusNotFound:
		return "（发布或资产不存在：检查私有仓的 token 权限、tag 与产物名）"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "（凭据无效或无权访问：检查 [upgrade].token）"
	case http.StatusTooManyRequests:
		return "（触发上游限流，稍后再试）"
	default:
		return ""
	}
}

// releaseByTag 取某个 tag 的发布（按 tag 缓存）。
//
// 只给"索引条目指向旧 tag"这一条路用（#52）：插件没更新时沿用上一条目，
// 有 token 的客户端要按旧 tag 的资产列表回查 id。缓存是必须的——一次
// /upgrade plugin all 里可能有一半插件都指向同一个旧 tag。
func (c *client) releaseByTag(ctx context.Context, tag string) (*ghRelease, error) {
	c.mu.Lock()
	if rel, ok := c.tags[tag]; ok {
		c.mu.Unlock()
		return rel, nil
	}
	c.mu.Unlock()

	u := c.apiBase + "/repos/" + c.repo + "/releases/tags/" + url.PathEscape(tag)
	body, err := c.get(ctx, c.meta, u, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("取发布 %s: %w", tag, err)
	}
	defer body.Close()

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析发布 %s: %w", tag, err)
	}
	if rel.TagName == "" {
		rel.TagName = tag
	}
	c.mu.Lock()
	c.tags[tag] = &rel
	c.mu.Unlock()
	return &rel, nil
}
