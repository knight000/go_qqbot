package upgrade

import (
	"context"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/assetsrc"
)

// 无 token：元数据两份清单都从 api_base 取，二进制地址由通道模板拼出。
// 这是决策 3 的原样形状——镜像站投毒时二进制变了、校验和没变，必然对不上。
func TestCheckMetadataFromAPIAndBinaryFromChannel(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("NEW-BINARY"))
	c := newClient(f.api.URL, f.assetTemplate(), "", "o/r")

	rel, err := c.check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rel.Version != "1.2.3" || rel.Tag != "v1.2.3" {
		t.Fatalf("版本/tag 解析错: %+v", rel)
	}
	if rel.CoreName != f.coreName {
		t.Errorf("核心产物名 = %q want %q", rel.CoreName, f.coreName)
	}
	if rel.CoreSize != int64(len(f.binary)) {
		t.Errorf("产物大小 = %d want %d", rel.CoreSize, len(f.binary))
	}
	if len(rel.CoreSHA) != 64 {
		t.Errorf("校验和长度不对: %q", rel.CoreSHA)
	}
	want := f.asset.URL + "/o/r/releases/download/v1.2.3/" + f.coreName
	if rel.CoreURL != want {
		t.Errorf("二进制地址 = %q want %q", rel.CoreURL, want)
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("检查阶段不应触碰镜像通道，实际命中 %d 次", f.assetHits.Load())
	}
}

// 有 token（私有仓）：三份产物全走 api_base，镜像一次都不碰。
// 私有资产无法在镜像上匿名取，而 token 更不能发给镜像主机。
func TestCheckWithTokenRoutesEverythingThroughAPI(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("NEW-BINARY"))
	c := newClient(f.api.URL, f.assetTemplate(), "ghp_secret", "o/r")

	rel, err := c.check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.HasPrefix(rel.CoreURL, f.api.URL+"/repos/o/r/releases/assets/") {
		t.Errorf("有 token 时二进制应走 api 资产端点，实际 %q", rel.CoreURL)
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("有 token 时不应触碰二进制通道，实际命中 %d 次", f.assetHits.Load())
	}
	if f.authOnAsset.Load() {
		t.Error("token 被发给了二进制通道的主机")
	}
}

// 即使调用方把二进制通道的地址传进 get，也不许带上凭据。
// 这条与上一条是同一约束的两面：上一条靠"根本不走镜像"，这条靠"走了也不带"。
func TestTokenNeverSentToBinaryChannelHost(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("x"))
	c := newClient(f.api.URL, f.assetTemplate(), "ghp_secret", "o/r")

	body, err := c.get(context.Background(), c.body,
		f.asset.URL+"/o/r/releases/download/v1.2.3/"+f.coreName, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body.Close()
	if f.authOnAsset.Load() {
		t.Fatal("token 被发给了二进制通道的主机")
	}
}

// #43 的发布实测口径：release 里的清单资产名是**顶层的** index.json。
// 仓库内的逻辑路径是 plugins/index.json，但 GitHub 的资产名不允许含 "/"，
// 所以客户端只能按文件名匹配，不能拼接路径、也不能依赖下载地址的末段形状。
func TestCheckMatchesTopLevelIndexAsset(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("x"))
	if err := func() error {
		c := newClient(f.api.URL, f.assetTemplate(), "", "o/r")
		_, err := c.check(context.Background())
		return err
	}(); err != nil {
		t.Fatalf("顶层 index.json 应能取到: %v", err)
	}

	// 反证：资产名按路径形状命名时，客户端**不该**认出它。
	f.indexAssetName = "plugins/index.json"
	c := newClient(f.api.URL, f.assetTemplate(), "", "o/r")
	_, err := c.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "没有资产 index.json") {
		t.Fatalf("只认顶层的 index.json，实际: %v", err)
	}
}

// checksums.txt 里没有本平台产物时必须拒绝：那是"该发布漏发了平台"，
// 而不是"下载失败"——两种文案要能区分开。
func TestCheckRejectsMissingPlatformInChecksums(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("x"))
	f.sums = strings.Repeat("0", 64) + "  qqbot-1.2.3-some-other-platform\n"
	c := newClient(f.api.URL, f.assetTemplate(), "", "o/r")

	_, err := c.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "漏发了平台") {
		t.Fatalf("应提示漏发平台，实际: %v", err)
	}
}

// 404 要能指向最可能的三个原因（token 权限 / tag / 产物名）。
func TestCheckTranslates404(t *testing.T) {
	c := newClient("http://127.0.0.1:1", "", "", "o/r")
	_, err := c.check(context.Background())
	if err == nil {
		t.Fatal("连不上时应报错")
	}
}

// 客户端按模板拼地址：镜像预设那种"把整个 github 地址塞进路径"的形状也走
// 同一份实现（#70 的动机就是它，只填域名是 404）。
func TestBinaryURLRendersTemplateForRepo(t *testing.T) {
	c := newClient("https://api.invalid", assetsrc.Presets()[0].Template, "", "o/r")
	want := "https://gh-proxy.com/https://github.com/o/r/releases/download/v1.2.3/a"
	if got := c.binaryURL("v1.2.3", "a"); got != want {
		t.Errorf("binaryURL = %q want %q", got, want)
	}

	// 上游插件是另一个仓：模板不变，owner/repo 跟着 forRepo 走（#53）。
	up := c.forRepo("u/p")
	want = "https://gh-proxy.com/https://github.com/u/p/releases/download/v1.2.3/a"
	if got := up.binaryURL("v1.2.3", "a"); got != want {
		t.Errorf("上游 binaryURL = %q want %q", got, want)
	}
}

// 404 的通道建议只加给**二进制通道**的请求：元数据 404（仓名写错、没有
// release）与镜像形状无关，把 owner 引到镜像上去只会让人查错方向。
func TestNotFoundHintOnlyForBinaryChannel(t *testing.T) {
	mirror := newClient("https://api.invalid", assetsrc.Presets()[0].Template, "", "o/r")
	if h := mirror.hintFor("https://api.invalid/repos/o/r/releases/latest", 404); strings.Contains(h, "镜像") {
		t.Errorf("元数据 404 不该带通道建议: %s", h)
	}
	got := mirror.hintFor("https://gh-proxy.com/https://github.com/o/r/releases/download/v1/a", 404)
	if !strings.Contains(got, "镜像") || !strings.Contains(got, "https://github.com") {
		t.Errorf("自定义通道的产物 404 应给出通道建议: %s", got)
	}

	// 官方直连没有"镜像形状"可提示，别硬塞一句。
	official := newClient("https://api.invalid", "", "", "o/r")
	if h := official.hintFor("https://github.com/o/r/releases/download/v1/a", 404); strings.Contains(h, "镜像") {
		t.Errorf("官方通道的 404 不该提镜像: %s", h)
	}
}
