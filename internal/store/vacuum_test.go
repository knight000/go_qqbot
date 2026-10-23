package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 快照必须是"能当库打开"的完整副本：升级回滚时它就是唯一的还原来源。
// 用 WAL 模式下的在途写入来验证——正是"直接拷 .db"会漏掉的那种数据。
func TestVacuumIntoProducesUsableSnapshot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	admin := Admin{OpenID: "u1", Role: RoleOwner, SourceScene: "c2c"}
	if err := db.UpsertAdmin(ctx, admin); err != nil {
		t.Fatalf("写入管理员: %v", err)
	}

	path := filepath.Join(t.TempDir(), "snap", "qqbot.db")
	if err := db.VacuumInto(ctx, path); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("快照文件不完整: %v", err)
	}

	// 快照自己也是一份可用的库。
	snap, err := Open(path)
	if err != nil {
		t.Fatalf("打开快照: %v", err)
	}
	defer snap.Close()
	role, ok, err := snap.AdminRole(ctx, "u1", "")
	if err != nil || !ok || role != RoleOwner {
		t.Fatalf("快照里没有那条写入: role=%q ok=%v err=%v", role, ok, err)
	}
}

// 目标已存在时直接报错：VACUUM INTO 自己会失败，但错误文案要能指到路径。
func TestVacuumIntoRejectsExistingTarget(t *testing.T) {
	db := openTestDB(t)
	path := filepath.Join(t.TempDir(), "exists.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.VacuumInto(context.Background(), path); err == nil {
		t.Fatal("目标已存在时应报错")
	}
}
