package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSnapshotter 写一个占位文件，记录被调用的路径。
type fakeSnapshotter struct {
	path string
	err  error
}

func (f *fakeSnapshotter) VacuumInto(_ context.Context, path string) error {
	f.path = path
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(path, []byte("snapshot"), 0o644)
}

func TestTakeSnapshotWritesIntoUpgradeDir(t *testing.T) {
	dir := StateDir(t.TempDir())
	f := &fakeSnapshotter{}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)

	path, err := TakeSnapshot(context.Background(), dir, f, now)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if want := filepath.Join(dir, "snapshots", "qqbot-20261023-120000.000.db"); path != want {
		t.Errorf("快照路径 = %q want %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("快照文件不存在: %v", err)
	}
}

// 快照失败必须中止升级：没有库备份就换盘，跨破坏性迁移时回滚不了。
func TestTakeSnapshotAbortsOnError(t *testing.T) {
	dir := StateDir(t.TempDir())
	f := &fakeSnapshotter{err: os.ErrPermission}
	if _, err := TakeSnapshot(context.Background(), dir, f, time.Now()); err == nil {
		t.Fatal("快照失败应报错")
	}
}

// 没有数据库（装配期没有 store）时跳过快照，不算失败。
func TestTakeSnapshotWithoutDB(t *testing.T) {
	path, err := TakeSnapshot(context.Background(), StateDir(t.TempDir()), nil, time.Now())
	if err != nil || path != "" {
		t.Fatalf("无库时应直接返回: %q %v", path, err)
	}
}

// 只留最近 2 份：第 3 份落进来时最旧的一份要消失。
func TestPruneSnapshotsKeepsNewest(t *testing.T) {
	dir := SnapshotDir(StateDir(t.TempDir()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"qqbot-20261020-120000.000.db",
		"qqbot-20261021-120000.000.db",
		"qqbot-20261022-120000.000.db",
		"不是快照.txt",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneSnapshots(dir, 2)
	if err != nil {
		t.Fatalf("PruneSnapshots: %v", err)
	}
	if removed != 1 {
		t.Errorf("删除 %d 份，want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "qqbot-20261020-120000.000.db")); !os.IsNotExist(err) {
		t.Error("最旧的一份应被删除")
	}
	for _, keep := range []string{"qqbot-20261021-120000.000.db", "qqbot-20261022-120000.000.db", "不是快照.txt"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s 应保留: %v", keep, err)
		}
	}
}

func TestPruneSnapshotsToleratesMissingDir(t *testing.T) {
	if n, err := PruneSnapshots(filepath.Join(t.TempDir(), "nope"), 2); err != nil || n != 0 {
		t.Fatalf("目录不存在时应静默成功: %d %v", n, err)
	}
}
