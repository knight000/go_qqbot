package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// testDir 在仓库 .temp 下建临时目录。
// 不用 t.TempDir()：本机约定临时文件只放项目 .temp，不写 AppData\Local\Temp。
func testDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("..", "..", ".temp", "test"))
	if err != nil {
		t.Fatalf("解析测试目录: %v", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("创建测试目录: %v", err)
	}
	dir, err := os.MkdirTemp(base, "sysinfo-")
	if err != nil {
		t.Fatalf("建临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{20 * time.Second, "20秒"},
		{90 * time.Second, "1分30秒"},
		{2*time.Hour + 13*time.Minute, "2小时13分"},
		{25 * time.Hour, "1天1小时"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.in); got != c.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDirSize(t *testing.T) {
	dir := testDir(t)

	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	// .git 应被跳过：它不是运行时数据，且可能极大
	gitDir := filepath.Join(dir, ".git", "objects")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "big"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	u, err := DirSize(context.Background(), dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if u.Bytes != 300 {
		t.Errorf("Bytes = %d, want 300", u.Bytes)
	}
	if u.Files != 2 {
		t.Errorf("Files = %d, want 2", u.Files)
	}
	if !u.IgnoredGit {
		t.Error("IgnoredGit = false, want true")
	}
	if u.Partial {
		t.Error("Partial = true, 不应出现")
	}
}

func TestReadMemory(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("当前平台不采集内存")
	}
	m, err := ReadMemory()
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	if m.Total == 0 {
		t.Fatal("Total = 0")
	}
	if m.Used > m.Total {
		t.Errorf("Used(%d) > Total(%d)", m.Used, m.Total)
	}
	if m.Percent < 0 || m.Percent > 100 {
		t.Errorf("Percent = %f，越界", m.Percent)
	}
	if m.ProcessRSS == 0 {
		t.Error("ProcessRSS = 0，应能取到当前进程常驻内存")
	}
}

func TestReadCPU(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("当前平台不采集 CPU")
	}
	c, err := ReadCPU(context.Background())
	if err != nil {
		t.Fatalf("ReadCPU: %v", err)
	}
	if c.Percent < 0 || c.Percent > 100 {
		t.Errorf("Percent = %f，越界", c.Percent)
	}
	if c.Cores != runtime.NumCPU() {
		t.Errorf("Cores = %d, want %d", c.Cores, runtime.NumCPU())
	}
}

func TestReadDisk(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("当前平台不采集磁盘")
	}
	d, err := ReadDisk(".")
	if err != nil {
		t.Fatalf("ReadDisk: %v", err)
	}
	if d.Total == 0 {
		t.Fatal("Total = 0")
	}
	if d.Free > d.Total {
		t.Errorf("Free(%d) > Total(%d)", d.Free, d.Total)
	}
	if d.Used+d.Free != d.Total {
		t.Errorf("Used(%d)+Free(%d) != Total(%d)", d.Used, d.Free, d.Total)
	}
}
