package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// pending 标记（ADR-0006 决策 6）：<datadir>/upgrade/state.json。
//
// 它解决的问题只有一个：新版本起来了但立刻死掉，而进程已经没法自救。
// 所以标记必须在换盘**之前**落下，由下一次启动来读——那是唯一一个
// "我活着，并且知道上次换过盘"的时刻。
const stateFileName = "state.json"

// StateDir 返回 pending 标记与快照所在的目录。
// datadir = filepath.Dir(cfg.Store.Path)，全仓唯一口径，由调用方传进来。
func StateDir(dataDir string) string { return filepath.Join(dataDir, "upgrade") }

// State 是 state.json 的内容。字段只增不改：旧版本读到新字段要能忽略。
type State struct {
	Tag       string    `json:"tag"`
	From      string    `json:"from_version"`
	To        string    `json:"to_version"`
	Exe       string    `json:"exe"`
	Snapshot  string    `json:"snapshot,omitempty"`
	Attempts  int       `json:"attempts"`
	StartedAt time.Time `json:"started_at"`
}

// LoadState 读取 pending 标记。文件不存在时返回 (nil, nil)——"没有未确认的升级"
// 是正常状态，不是错误。
func LoadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取升级标记: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", filepath.Join(dir, stateFileName), err)
	}
	return &st, nil
}

// SaveState 原子写入 pending 标记。
//
// 必须先写临时文件再 rename：崩溃发生在写状态的中途时，留下的必须是
// "旧的完整标记"或"新的完整标记"，不能是半份 JSON——那会让下一次启动
// 既不敢回滚也不敢继续。
func SaveState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, stateFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("写升级标记: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换升级标记: %w", err)
	}
	return nil
}

// ClearState 删掉 pending 标记。文件本来就不存在也算成功（幂等）。
func ClearState(dir string) error {
	err := os.Remove(filepath.Join(dir, stateFileName))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("删除升级标记: %w", err)
}

// Decision 是启动期对 pending 标记的结论。
type Decision struct {
	Pending  bool
	Rollback bool
	Reason   string
	State    *State
}

// CheckPending 在启动早期调用一次（配置已加载、拿单实例锁之前）：
//
//	没有标记  → 什么都不做；
//	有标记    → attempts+1 并落盘；attempts > 1 判定回滚。
//
// 为什么是 "> 1" 而不是 ">= 1"：升级后第一次启动本来就是"新版本的第一次
// 亮相"，直接回滚等于永不上新版。只有它没能活到删标记（核心起来 + 网关
// 存活 30 秒）才说明这版有问题。
//
// 已知边角：30 秒内有人敲 /reload，新进程会把 attempts 再 +1，于是下一次
// 启动误判为"该回滚了"。ADR-0006 接受这个代价——误回滚的后果是退回上一版
// 并且升级标记被清掉，owner 再敲一次 /upgrade 即可；反过来放宽判定则会让
// "新版本一启动就崩"永远回滚不了。
func CheckPending(dir string) (Decision, error) {
	st, err := LoadState(dir)
	if err != nil {
		// 标记坏了就既不回滚也不继续猜：交给调用方记一条 Warn，
		// 文件原样留着，人能去看。
		return Decision{}, err
	}
	if st == nil {
		return Decision{}, nil
	}
	st.Attempts++
	if err := SaveState(dir, st); err != nil {
		return Decision{Pending: true, State: st}, err
	}
	d := Decision{Pending: true, State: st}
	if st.Attempts > 1 {
		d.Rollback = true
		d.Reason = fmt.Sprintf("上次升级到 %s 后进程未能稳定运行（这是第 %d 次尝试启动），回滚到 %s",
			st.To, st.Attempts, st.From)
	}
	return d, nil
}

// MarkHealthy 表示"这次启动成功了"，删掉标记。
// 只在 core.Start 完成且网关存活 30 秒后调用（ADR-0006 决策 6）。
func MarkHealthy(dir string) error { return ClearState(dir) }
