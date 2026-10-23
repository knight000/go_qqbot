package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshotter 是数据库快照能力，由 *store.DB 实现（SQLite 的 VACUUM INTO）。
//
// 这里用接口而不是 import internal/store：本包只负责"升级要留一份库备份"，
// 不负责怎么备份；也让 upgrade 的单测不需要真数据库。
type Snapshotter interface {
	VacuumInto(ctx context.Context, path string) error
}

// 保留最近几份快照。ADR-0006 的代价一节点名了这条：不清理的话，
// 升级路径本身会变成磁盘增长的来源。
const snapshotKeep = 2

// mode 是快照文件的命名布局，也是排序依据（字典序 == 时间序）。
const snapshotLayout = "20060102-150405.000"

// SnapshotDir 返回快照目录。
func SnapshotDir(dir string) string { return filepath.Join(dir, "snapshots") }

// TakeSnapshot 在做任何落位动作之前拍一份库快照，并顺手裁剪旧快照。
//
// 顺序很关键：快照必须在下载之前。回滚二进制不等于回滚 schema
// （ADR-0006 代价一节），所以这份快照是"跨破坏性迁移时人工还原"的唯一依靠，
// 它必须在磁盘被新版本改过之前就已经存在。
//
// snapshotter 为 nil 表示这次的装配没有数据库（单测、或 init 场景），
// 此时不拍快照也不算失败。
func TakeSnapshot(ctx context.Context, dir string, s Snapshotter, now time.Time) (string, error) {
	if s == nil {
		return "", nil
	}
	snapDir := SnapshotDir(dir)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return "", fmt.Errorf("创建快照目录: %w", err)
	}
	path := filepath.Join(snapDir, "qqbot-"+now.Format(snapshotLayout)+".db")
	if err := s.VacuumInto(ctx, path); err != nil {
		return "", fmt.Errorf("数据库快照失败: %w", err)
	}
	// 裁剪失败只影响磁盘占用，不该让一次已经成功的升级失败。
	if _, err := PruneSnapshots(snapDir, snapshotKeep); err != nil {
		return path, nil
	}
	return path, nil
}

// PruneSnapshots 只留最近 keep 份快照，返回删掉的数量。
//
// 按文件名排序：布局是定长的 "20060102-150405.000"，字典序就是时间序，
// 比读 mtime 稳（复制/恢复会重写 mtime）。
func PruneSnapshots(dir string, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取快照目录: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return 0, nil
	}
	sort.Strings(names)
	removed := 0
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return removed, fmt.Errorf("删除旧快照 %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}
