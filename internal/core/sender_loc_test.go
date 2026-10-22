package core

import (
	"log/slog"
	"strings"
	"testing"
)

// TestNewSenderRejectsNilLocation 是 P2-4 的守卫：nil 时区若被"兜底成本地
// 时区"，就会在 UTC 机器上按 08:00 切日，2×limit 超发。必须 fail-fast。
func TestNewSenderRejectsNilLocation(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("loc 为 nil 时应 panic，而不是静默回退到本地时区")
		}
		msg, ok := rec.(string)
		if !ok || !strings.Contains(msg, "时区") {
			t.Fatalf("panic 信息要说明是时区问题，实际 %v", rec)
		}
	}()
	NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, nil)
}
