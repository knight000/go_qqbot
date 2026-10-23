package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// peHeader 造一段"看起来像可执行文件"的内容：MZ 头足以通过文件头判定，
// 两个平台都被接受（Unix 上不会真的去执行它）。
func peHeader(tag string) []byte {
	return []byte("MZ" + strings.Repeat("-", 60) + tag)
}

// fakeExe 在临时目录里放一份"旧二进制"，返回它的路径。
func fakeExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "qqbot.exe")
	if err := os.WriteFile(exe, peHeader("old"), 0o755); err != nil {
		t.Fatalf("写假二进制: %v", err)
	}
	return exe
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// 验收 4 的核心：.new 的内容不是可执行文件时，Stage 必须在进程内失败，
// 并且不留残渣——旧二进制原样、没有 .old、没有 .new。
func TestStageRejectsNonExecutableContent(t *testing.T) {
	exe := fakeExe(t)
	before := readFile(t, exe)
	staged := StagedPath(exe)
	if err := os.WriteFile(staged, []byte("<html>404 Not Found</html>"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Stage(exe); err == nil {
		t.Fatal("非可执行内容应被拒绝")
	} else if !strings.Contains(err.Error(), "不是可执行文件") {
		t.Fatalf("错误文案应说明内容不对: %v", err)
	}

	if got := readFile(t, exe); got != before {
		t.Error("旧二进制被改动了")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("失败后应删掉 .new")
	}
	if _, err := os.Stat(OldPath(exe)); !os.IsNotExist(err) {
		t.Error("失败后不应产生 .old")
	}
}

func TestStageRejectsEmptyFile(t *testing.T) {
	exe := fakeExe(t)
	if err := os.WriteFile(StagedPath(exe), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(exe); err == nil || !strings.Contains(err.Error(), "空文件") {
		t.Fatalf("空文件应被拒绝，实际: %v", err)
	}
}

func TestStageRequiresDownloadedFile(t *testing.T) {
	exe := fakeExe(t)
	if _, err := Stage(exe); err == nil {
		t.Fatal("没有 .new 时应报错")
	}
}

// 成功路径要给出可执行权限（Unix）并把大小报出来——预览与日志都用它。
func TestStageAcceptsExecutable(t *testing.T) {
	exe := fakeExe(t)
	body := peHeader("new")
	if err := os.WriteFile(StagedPath(exe), body, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Stage(exe)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if st.Size != int64(len(body)) {
		t.Errorf("大小 = %d want %d", st.Size, len(body))
	}
	info, err := os.Stat(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("权限位没有执行位: %v", info.Mode())
	}
}

// 落位后的换盘与回滚：exe 换成新的、旧内容留在 .old，Restore 能换回来。
// 这是验收 1「apply 完成落位」与验收 3「回滚到 .old」共用的机制。
func TestSwapAndRestore(t *testing.T) {
	exe := fakeExe(t)
	old := readFile(t, exe)
	if err := os.WriteFile(StagedPath(exe), peHeader("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(exe); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if err := Swap(exe); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readFile(t, exe); !strings.HasSuffix(got, "new") {
		t.Errorf("exe 没有换成新内容: %q", got)
	}
	if got := readFile(t, OldPath(exe)); got != old {
		t.Errorf(".old 不是旧内容: %q", got)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error(".new 应该已经被 rename 走")
	}

	if err := Restore(exe); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, exe); got != old {
		t.Errorf("回滚后 exe 不是旧内容: %q", got)
	}
	if _, err := os.Stat(OldPath(exe)); !os.IsNotExist(err) {
		t.Error(".old 应该已经被 rename 走")
	}
}

// 没有 .new 时换盘必须拒绝，且绝不改动 exe：
// 这条防的是"计划没落好就开始关停"，那会导致目录里没有可执行的二进制。
func TestSwapWithoutStagedKeepsExe(t *testing.T) {
	exe := fakeExe(t)
	old := readFile(t, exe)
	if err := Swap(exe); err == nil {
		t.Fatal("没有 .new 时应拒绝换盘")
	}
	if got := readFile(t, exe); got != old {
		t.Error("exe 被改动了")
	}
}

func TestRestoreWithoutOldFails(t *testing.T) {
	exe := fakeExe(t)
	if err := Restore(exe); err == nil {
		t.Fatal("没有 .old 时应报错，而不是把 exe 弄没")
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("exe 必须还在: %v", err)
	}
}

func TestCleanOld(t *testing.T) {
	exe := fakeExe(t)
	if err := os.WriteFile(OldPath(exe), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanOld(exe); err != nil {
		t.Fatalf("CleanOld: %v", err)
	}
	if _, err := os.Stat(OldPath(exe)); !os.IsNotExist(err) {
		t.Error(".old 应被删除")
	}
	// 已经不在了也算成功：启动期清理每次都跑。
	if err := CleanOld(exe); err != nil {
		t.Fatalf("重复清理不应报错: %v", err)
	}
}

func TestRemoveStaged(t *testing.T) {
	exe := fakeExe(t)
	if err := os.WriteFile(StagedPath(exe), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStaged(exe); err != nil {
		t.Fatalf("RemoveStaged: %v", err)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error(".new 应被删除")
	}
}

// 下载器：边写边算 sha256，写对了才算成功。
func TestDownloadVerifiesSHA(t *testing.T) {
	body := peHeader("payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	exe := fakeExe(t)
	c := newClient("", "", "", "o/r")
	sum := sha256Hex(body)

	st, err := c.download(context.Background(), srv.URL, StagedPath(exe), sum, MaxAssetBytes)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if st.Size != int64(len(body)) || st.SHA != sum {
		t.Errorf("结果不对: %+v", st)
	}
	if got := readFile(t, st.Path); got != string(body) {
		t.Error("内容与源不一致")
	}
}

// 验收 2：sha 不符 → 拒绝、临时文件删除、旧二进制不动。
func TestDownloadRejectsBadSHAAndRemovesTemp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(peHeader("payload"))
	}))
	defer srv.Close()

	exe := fakeExe(t)
	before := readFile(t, exe)
	c := newClient("", "", "", "o/r")
	wrong := strings.Repeat("0", 64)

	_, err := c.download(context.Background(), srv.URL, StagedPath(exe), wrong, MaxAssetBytes)
	if err == nil || !strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("应因校验和不匹配而失败，实际: %v", err)
	}
	if _, statErr := os.Stat(StagedPath(exe)); !os.IsNotExist(statErr) {
		t.Error("校验失败后必须删掉临时文件")
	}
	if got := readFile(t, exe); got != before {
		t.Error("旧二进制被改动了")
	}
}

// 没有上限的下载器迟早会把磁盘写满：被投毒/配错的元数据指一个 10 GB 的
// 地址，表现会是"升级中磁盘满了"，而不是"升级失败"。
func TestDownloadEnforcesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()

	exe := fakeExe(t)
	c := newClient("", "", "", "o/r")
	_, err := c.download(context.Background(), srv.URL, StagedPath(exe), strings.Repeat("0", 64), 1024)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("应因超限而失败，实际: %v", err)
	}
	if _, statErr := os.Stat(StagedPath(exe)); !os.IsNotExist(statErr) {
		t.Error("超限后必须删掉半成品")
	}
}

// 恰好等于上限必须成功：边界算错 1 字节，正常产物就永远升不上去。
func TestDownloadAcceptsExactlyAtLimit(t *testing.T) {
	body := peHeader("x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	exe := fakeExe(t)
	c := newClient("", "", "", "o/r")
	if _, err := c.download(context.Background(), srv.URL, StagedPath(exe), sha256Hex(body), int64(len(body))); err != nil {
		t.Fatalf("恰好等于上限应成功: %v", err)
	}
}
