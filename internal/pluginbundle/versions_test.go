package pluginbundle

import (
	"testing"
	"testing/fstest"
)

// 版本清单是启动期对齐的唯一版本来源（#52）。格式必须简单到一眼能核对：
// 一行一个 name=version，允许注释与空行。
func TestVersionsReadsNameVersionLines(t *testing.T) {
	src := fstest.MapFS{
		VersionsFile: {Data: []byte(
			"# 由 go run ./tools/build plugins 生成\n" +
				"\n" +
				"echo=0.1.0\n" +
				" md = 0.3.0 \r\n" +
				"adminping=0.1.0\n")},
		"echo": {Data: []byte("x")},
	}

	got := versionsOf(src)
	want := map[string]string{"echo": "0.1.0", "md": "0.3.0", "adminping": "0.1.0"}
	if len(got) != len(want) {
		t.Fatalf("versionsOf = %v，期望 %v", got, want)
	}
	for name, ver := range want {
		if got[name] != ver {
			t.Errorf("%s 的版本 = %q，期望 %q", name, got[name], ver)
		}
	}
}

// 清单缺失（干净克隆、或本地没跑 tools/build plugins）不是故障：对齐是
// 尽力而为，返回空 map 让启动照常进行。
func TestVersionsMissingFileIsEmpty(t *testing.T) {
	if got := versionsOf(fstest.MapFS{"echo": {Data: []byte("x")}}); len(got) != 0 {
		t.Fatalf("没有 %s 时应返回空 map，实际 %v", VersionsFile, got)
	}
	if got := versionsOf(nil); len(got) != 0 {
		t.Fatalf("FS 为 nil 时应返回空 map，实际 %v", got)
	}
}

func TestVersionsEmptyFileIsEmpty(t *testing.T) {
	for _, body := range []string{"", "\n\n", "# 只有注释\n"} {
		if got := versionsOf(fstest.MapFS{VersionsFile: {Data: []byte(body)}}); len(got) != 0 {
			t.Errorf("内容 %q 应返回空 map，实际 %v", body, got)
		}
	}
}

// 格式坏一律降级成空 map，而不是"能读几行算几行"：一份被写坏一半的清单
// 会把某些插件当成"没有版本"而跳过对齐，症状是升级核心后插件没跟上，
// 却没有任何错误。宁可整轮不对齐，也不要半轮。
func TestVersionsMalformedFileIsEmpty(t *testing.T) {
	bodies := []string{
		"echo\n",              // 没有 =
		"echo=\n",             // 版本为空
		"=0.1.0\n",            // 名字为空
		"echo=0.1.0\ngarbage", // 一行坏掉整份作废
	}
	for _, body := range bodies {
		if got := versionsOf(fstest.MapFS{VersionsFile: {Data: []byte(body)}}); len(got) != 0 {
			t.Errorf("内容 %q 应返回空 map，实际 %v", body, got)
		}
	}
}
