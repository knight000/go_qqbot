package upgrade

import (
	"runtime"
	"strings"
	"testing"
)

const sampleIndex = `{
  "schema": 1,
  "core": {"version": "1.2.3"},
  "plugins": [
    {"name": "echo", "version": "0.2.0",
     "assets": {"windows-amd64": {"url": "https://example/echo.exe", "sha256": "aa"}}}
  ]
}`

func TestParseIndex(t *testing.T) {
	idx, err := ParseIndex(strings.NewReader(sampleIndex))
	if err != nil {
		t.Fatalf("解析样例失败: %v", err)
	}
	if idx.Core.Version != "1.2.3" {
		t.Errorf("core.version = %q", idx.Core.Version)
	}
	if len(idx.Plugins) != 1 || idx.Plugins[0].Name != "echo" {
		t.Fatalf("插件清单解析错: %+v", idx.Plugins)
	}
}

// schema 不认识时必须拒绝：把 v2 当 v1 读的表现是「所有插件都缺资产」，
// 而那已经是升级的中途。
func TestParseIndexRejectsUnknownSchema(t *testing.T) {
	_, err := ParseIndex(strings.NewReader(`{"schema":2,"core":{"version":"1.2.3"}}`))
	if err == nil || !strings.Contains(err.Error(), "schema=2") {
		t.Fatalf("应拒绝未知 schema，实际: %v", err)
	}
}

func TestParseIndexRequiresCoreVersion(t *testing.T) {
	if _, err := ParseIndex(strings.NewReader(`{"schema":1,"core":{}}`)); err == nil {
		t.Fatal("缺少 core.version 应报错")
	}
}

// 资产名必须与 CI 的产物名逐字一致（决策 11）；错一个字符就是在 404 上升级失败。
func TestCoreAssetName(t *testing.T) {
	if got := CoreAssetName("1.2.3", "linux", "arm64"); got != "qqbot-1.2.3-linux-arm64" {
		t.Errorf("linux 产物名 = %q", got)
	}
	if got := CoreAssetName("1.2.3", "windows", "amd64"); got != "qqbot-1.2.3-windows-amd64.exe" {
		t.Errorf("windows 产物名 = %q", got)
	}
}

func TestCurrentPlatform(t *testing.T) {
	if got := currentPlatform(); got != runtime.GOOS+"-"+runtime.GOARCH {
		t.Errorf("平台键 = %q", got)
	}
}

// 两种分隔符都要认：GNU 用两个空格、二进制模式带 *，CI 换个容器就会变。
func TestParseChecksums(t *testing.T) {
	const (
		s1 = "d2c3b4a5d2c3b4a5d2c3b4a5d2c3b4a5d2c3b4a5d2c3b4a5d2c3b4a5d2c3b4a5"
		s2 = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
		s3 = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	)
	in := s1 + "  qqbot-1.2.3-linux-amd64\n" +
		s2 + "  qqbot-1.2.3-windows-amd64.exe\n\n" +
		"# 注释与空行跳过\n" +
		s3 + " *qqbot-1.2.3-linux-arm64\n"
	m, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("应解析 3 条，实际 %d: %v", len(m), m)
	}
	if m["qqbot-1.2.3-windows-amd64.exe"] != s2 {
		t.Errorf("二进制模式的行没解析对: %v", m)
	}
	if m["qqbot-1.2.3-linux-arm64"] != s3 {
		t.Errorf("* 前缀没剥掉: %v", m)
	}
}

// 坏行必须报错而不是跳过：跳过它等于让"清单里根本没这个产物"变成运行时
// 的空指针，报错才能在升级开始前就停下来。
func TestParseChecksumsRejectsBadLine(t *testing.T) {
	if _, err := ParseChecksums(strings.NewReader("不是校验和\n")); err == nil {
		t.Fatal("坏行应报错")
	}
}
