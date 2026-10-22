package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// failingStateStore 让 DeleteState 恒失败，模拟"库已经关了 / 磁盘满"这类现场。
// 网关对它只能告警，不能把清 session 的流程中断在半路。
type failingStateStore struct {
	deleted []string
	fail    bool
}

func (f *failingStateStore) GetState(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (f *failingStateStore) PutState(context.Context, string, string) error { return nil }

func (f *failingStateStore) DeleteState(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	if f.fail {
		return errors.New("database is closed")
	}
	return nil
}

// TestClearSessionWarnsWhenDeleteFails 钉住 P1-1 的持久化层：内存清掉但库里
// 删不掉时，必须留下带 key 的 Warn——否则下次启动仍会 Resume 一个死 session，
// 而日志里只会看到"怎么又在 Resume"。
func TestClearSessionWarnsWhenDeleteFails(t *testing.T) {
	var buf bytes.Buffer
	fs := &failingStateStore{fail: true}
	c := New(Options{
		Store: fs,
		Log:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	c.sessionID = "sid-dead"
	c.seq = 42

	c.clearSession(context.Background())

	// 第一个键失败不能拦住第二个键：两条都要尝试，否则 seq 会留在库里。
	if len(fs.deleted) != 2 || fs.deleted[0] != "session_id" || fs.deleted[1] != "seq" {
		t.Fatalf("两个键都应尝试删除，实际 %v", fs.deleted)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "清理 session 持久化失败") {
		t.Fatalf("删除失败必须留 Warn，实际日志: %s", out)
	}
	if !strings.Contains(out, "session_id") || !strings.Contains(out, "database is closed") {
		t.Fatalf("Warn 要点明是哪个键、什么错误，实际日志: %s", out)
	}
	// 内存状态必须已经清掉：没有 session 就不该再 Resume。
	if c.sessionID != "" || c.seq != 0 {
		t.Fatalf("内存 session 应已清空，实际 sid=%q seq=%d", c.sessionID, c.seq)
	}
}

// TestClearSessionQuietWhenDeleteSucceeds 是上一条的反向守卫：正常路径不许有
// Warn，否则真出问题时会被噪音淹掉。
func TestClearSessionQuietWhenDeleteSucceeds(t *testing.T) {
	var buf bytes.Buffer
	fs := &failingStateStore{}
	c := New(Options{
		Store: fs,
		Log:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	c.sessionID = "sid"
	c.seq = 7

	c.clearSession(context.Background())

	if len(fs.deleted) != 2 {
		t.Fatalf("应删除两个键，实际 %v", fs.deleted)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("正常删除不应告警，实际日志: %s", buf.String())
	}
}
