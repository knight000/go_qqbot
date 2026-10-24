package upgrade

// #52 的 B4：index.json 里的条目可能指向**上一版 tag**（插件没更新时沿用）。
// assetURL 不能再用"当前发布的 tag"重新拼地址，否则沿用条目必然 404。
// 这里把三条通道的 tag 口径钉住：无 token 换 host 不换 tag；有 token 按
// URL 里的 tag 取发布；同一个 tag 在一次升级里只查一次。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 无 token：索引里的 url 已经是 github 直链，只把 host 换成 asset_base。
// tag 必须原样保留——它可能比当前发布旧。
func TestAssetURLKeepsTagFromIndexURLWithoutToken(t *testing.T) {
	c := newClient("https://api.invalid", "https://mirror.example", "", "o/r")
	rel := &Release{Tag: "v1.2.4"}
	raw := "https://github.com/o/r/releases/download/v1.2.3/qqbot-plugin-echo-0.2.0-linux-amd64"

	got, err := c.assetURL(context.Background(), rel, raw)
	if err != nil {
		t.Fatalf("assetURL: %v", err)
	}
	want := "https://mirror.example/o/r/releases/download/v1.2.3/qqbot-plugin-echo-0.2.0-linux-amd64"
	if got != want {
		t.Fatalf("assetURL = %q，期望 %q（host 换成镜像、tag 保留旧值）", got, want)
	}
}

// 有 token：按 URL 里的 tag 取该 tag 的发布，再按资产名回查 id。
// 一次升级里多个插件指向同一个旧 tag 时只查一次（按 tag 缓存）。
func TestAssetURLFetchesReleaseByIndexTagAndCaches(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	asset := f.addPluginAtTag("echo", "0.2.0", "v1.2.3", []byte("plugin-bytes"))

	c := newClient(f.api.URL, f.asset.URL, "secret", "o/r")
	rel := &Release{Tag: f.tag}
	raw := "https://github.com/o/r/releases/download/v1.2.3/" + asset

	first, err := c.assetURL(context.Background(), rel, raw)
	if err != nil {
		t.Fatalf("assetURL: %v", err)
	}
	second, err := c.assetURL(context.Background(), rel, raw)
	if err != nil {
		t.Fatalf("assetURL（第二次）: %v", err)
	}
	if first != second {
		t.Errorf("同一个 tag 的两次解析结果不一致: %q vs %q", first, second)
	}
	if !strings.HasSuffix(first, "/releases/assets/101") {
		t.Errorf("应按旧 tag 的资产 id 拼地址，实际 %q", first)
	}
	if n := f.tagHits.Load(); n != 1 {
		t.Errorf("同一个 tag 应只查一次发布，实际 %d 次", n)
	}
}

// 索引里的 tag 与当前发布一致时直接用本次发布的资产列表，不多发请求。
func TestAssetURLUsesLatestReleaseWhenTagMatches(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	asset := f.addPlugin("echo", "0.2.0", []byte("plugin-bytes"))

	c := newClient(f.api.URL, f.asset.URL, "secret", "o/r")
	// 只给 gh，不给可用的 tag 端点：走错了会失败。
	rel := &Release{Tag: f.tag, gh: mustGHRelease(t, f.tag, asset, 101)}

	got, err := c.assetURL(context.Background(), rel, "https://github.com/o/r/releases/download/"+f.tag+"/"+asset)
	if err != nil {
		t.Fatalf("assetURL: %v", err)
	}
	if !strings.HasSuffix(got, "/releases/assets/101") {
		t.Errorf("应复用本次发布的资产 id，实际 %q", got)
	}
	if n := f.tagHits.Load(); n != 0 {
		t.Errorf("tag 与当前发布一致时不该再查发布，实际 %d 次", n)
	}
}

// 端到端（无 token）：沿用条目指向旧 tag，下载必须落在旧 tag 的路径上。
// fakeRelease 的镜像通道只在该 tag 下提供产物，拼错 tag 会 404。
func TestUpgradePluginsDownloadsCarriedForwardAsset(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	blob := fakePluginAsset(t, "echo", "0.2.0")
	f.addPluginAtTag("echo", "0.2.0", "v1.2.3", blob)

	r, tgt := pluginTarget(t, "echo")
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if got := rep.Results[0].Action; got != "升" {
		t.Fatalf("沿用条目应当能升级，实际 %q（%s）", got, rep.Text())
	}
	if len(r.calls) != 1 {
		t.Fatalf("应换一次文件，实际 %d", len(r.calls))
	}
	if string(r.got) != string(blob) {
		t.Error("换上去的字节与旧 tag 上的产物不一致")
	}
}

// 端到端（有 token）：走 api 资产端点，且必须为旧 tag 查一次发布。
func TestUpgradePluginsWithTokenFetchesCarriedTag(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	blob := fakePluginAsset(t, "echo", "0.2.0")
	f.addPluginAtTag("echo", "0.2.0", "v1.2.3", blob)

	r, tgt := pluginTarget(t, "echo")
	u, _ := newTestUpgrader(t, f, "1.2.3", func(o *Options) { o.Token = "secret" })

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if got := rep.Results[0].Action; got != "升" {
		t.Fatalf("有 token 时也应能升级沿用条目，实际 %q（%s）", got, rep.Text())
	}
	if n := f.tagHits.Load(); n != 1 {
		t.Errorf("应恰好为旧 tag 查一次发布，实际 %d 次", n)
	}
	if string(r.got) != string(blob) {
		t.Error("换上去的字节与发布里的产物不一致")
	}
}

// mustGHRelease 造一份「本次发布」的资产列表，供 token 通道复用。
func mustGHRelease(t *testing.T, tag, asset string, id int64) *ghRelease {
	t.Helper()
	raw := fmt.Sprintf(`{"tag_name":%q,"assets":[{"id":%d,"name":%q,"size":1}]}`, tag, id, asset)
	var rel ghRelease
	if err := json.Unmarshal([]byte(raw), &rel); err != nil {
		t.Fatalf("构造 ghRelease: %v", err)
	}
	return &rel
}
