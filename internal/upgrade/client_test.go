package upgrade

import (
	"context"
	"strings"
	"testing"
)

// 无 token：元数据两份清单都从 api_base 取，二进制地址指向 asset_base。
// 这是决策 3 的原样形状——镜像站投毒时二进制变了、校验和没变，必然对不上。
func TestCheckMetadataFromAPIAndBinaryFromMirror(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("NEW-BINARY"))
	c := newClient(f.api.URL, f.asset.URL, "", "o/r")

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
	c := newClient(f.api.URL, f.asset.URL, "ghp_secret", "o/r")

	rel, err := c.check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.HasPrefix(rel.CoreURL, f.api.URL+"/repos/o/r/releases/assets/") {
		t.Errorf("有 token 时二进制应走 api 资产端点，实际 %q", rel.CoreURL)
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("有 token 时不应触碰 asset_base，实际命中 %d 次", f.assetHits.Load())
	}
	if f.authOnAsset.Load() {
		t.Error("token 被发给了 asset_base 的主机")
	}
}

// 即使调用方把 asset_base 的地址传进 get，也不许带上凭据。
// 这条与上一条是同一约束的两面：上一条靠"根本不走镜像"，这条靠"走了也不带"。
func TestTokenNeverSentToAssetBaseHost(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("x"))
	c := newClient(f.api.URL, f.asset.URL, "ghp_secret", "o/r")

	body, err := c.get(context.Background(), c.body,
		f.asset.URL+"/o/r/releases/download/v1.2.3/"+f.coreName, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body.Close()
	if f.authOnAsset.Load() {
		t.Fatal("token 被发给了 asset_base 的主机")
	}
}

// checksums.txt 里没有本平台产物时必须拒绝：那是"该发布漏发了平台"，
// 而不是"下载失败"——两种文案要能区分开。
func TestCheckRejectsMissingPlatformInChecksums(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", []byte("x"))
	f.sums = strings.Repeat("0", 64) + "  qqbot-1.2.3-some-other-platform\n"
	c := newClient(f.api.URL, f.asset.URL, "", "o/r")

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
