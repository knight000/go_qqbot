package store

import (
	"errors"
	"strings"
	"testing"
)

type failingResult struct{}

func (failingResult) LastInsertId() (int64, error) { return 0, errors.New("不可用") }
func (failingResult) RowsAffected() (int64, error) { return 0, errors.New("不可用") }

// 计数失败被吞成 (0, nil) 的后果不是"少统计一行"：GC 用 n < batch 判断
// "这一批没删满就是删完了"，0 会被读成删完了，剩余行要等下一个 10 分钟，
// 而且日志里一行错误都没有。
func TestPurgeReportsRowsAffectedError(t *testing.T) {
	n, err := purgeResult(failingResult{})
	if err == nil {
		t.Fatal("RowsAffected 失败必须返回错误")
	}
	if n != 0 {
		t.Errorf("失败时行数应为 0，实际 %d", n)
	}
	if !strings.Contains(err.Error(), "行数") {
		t.Errorf("错误应说明是读取行数失败，实际: %v", err)
	}
}
