package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pendingState(dir string, t *testing.T) {
	t.Helper()
	if err := SaveState(dir, &State{
		Tag: "v1.2.4", From: "1.2.3", To: "1.2.4",
		Exe: filepath.Join(dir, "qqbot"), Attempts: 0,
	}); err != nil {
		t.Fatalf("写 pending 标记: %v", err)
	}
}

func TestLoadStateAbsentIsNotAnError(t *testing.T) {
	st, err := LoadState(t.TempDir())
	if err != nil || st != nil {
		t.Fatalf("没有标记时应返回 (nil,nil)，实际 %v %v", st, err)
	}
}

// 没有标记时启动期什么都不做：不建目录、不写文件。
func TestCheckPendingWithoutMarkerDoesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "upgrade")
	d, err := CheckPending(dir)
	if err != nil {
		t.Fatalf("CheckPending: %v", err)
	}
	if d.Pending || d.Rollback {
		t.Fatalf("不该有结论: %+v", d)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Error("不该创建标记文件")
	}
}

// 升级后的第一次启动：attempts 0 → 1，不回滚（新版本总要有机会跑起来）。
func TestCheckPendingFirstStartDoesNotRollback(t *testing.T) {
	dir := StateDir(t.TempDir())
	pendingState(dir, t)

	d, err := CheckPending(dir)
	if err != nil {
		t.Fatalf("CheckPending: %v", err)
	}
	if !d.Pending || d.Rollback {
		t.Fatalf("第一次启动不应回滚: %+v", d)
	}
	st, err := LoadState(dir)
	if err != nil || st == nil {
		t.Fatalf("标记应仍在: %v %v", st, err)
	}
	if st.Attempts != 1 {
		t.Errorf("attempts = %d want 1", st.Attempts)
	}
}

// 验收 3：模拟"新版本启动即退出"——第二次启动在同一份标记上做出回滚结论，
// 并且原因写清楚（日志要用它）。
func TestCheckPendingSecondStartRollsBack(t *testing.T) {
	dir := StateDir(t.TempDir())
	pendingState(dir, t)

	first, err := CheckPending(dir)
	if err != nil || first.Rollback {
		t.Fatalf("第一次启动: %+v %v", first, err)
	}
	second, err := CheckPending(dir)
	if err != nil {
		t.Fatalf("第二次 CheckPending: %v", err)
	}
	if !second.Rollback {
		t.Fatalf("第二次启动应判定回滚: %+v", second)
	}
	if !strings.Contains(second.Reason, "1.2.4") || !strings.Contains(second.Reason, "1.2.3") {
		t.Errorf("原因应说清版本与方向: %q", second.Reason)
	}
	if !strings.Contains(second.Reason, "未能稳定运行") {
		t.Errorf("原因应说清为什么回滚: %q", second.Reason)
	}
	if second.State == nil || second.State.Attempts != 2 {
		t.Errorf("attempts 应累加到 2: %+v", second.State)
	}
}

// 删标记之后又回到"什么都没有"的状态：回滚完必须能重新开始，
// 否则每一次启动都会继续 attempts++。
func TestClearStateResetsTheCounter(t *testing.T) {
	dir := StateDir(t.TempDir())
	pendingState(dir, t)
	if _, err := CheckPending(dir); err != nil {
		t.Fatal(err)
	}
	if err := ClearState(dir); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	d, err := CheckPending(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Pending {
		t.Fatalf("清掉之后不应再有 pending: %+v", d)
	}
	// 幂等：启动成功路径每次都会调一次。
	if err := ClearState(dir); err != nil {
		t.Fatalf("重复 ClearState 不应报错: %v", err)
	}
}

// 坏掉的标记：既不回滚也不静默删除，交回一个错误让人去看。
func TestCheckPendingReportsCorruptMarker(t *testing.T) {
	dir := StateDir(t.TempDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, stateFileName)
	if err := os.WriteFile(path, []byte("{半份 json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckPending(dir)
	if err == nil {
		t.Fatal("坏标记应报错")
	}
	if d.Rollback {
		t.Error("不知道内容时不能贸然回滚")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Error("坏标记应原样留着")
	}
}
