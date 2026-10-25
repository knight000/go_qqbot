package pluginproc

import (
	"log/slog"
	"testing"
)

// 没有 msg 的 JSON 行不是一条 slog 记录（#52 评审）。
//
// 从前这种行会被重放成"空消息 + 几个字段"：原文丢了，终端上只剩一条没有内容
// 的日志。它必须走与非 JSON 行同一条兜底路径（整行当 Info），至少不丢信息。
func TestParseLogLineRequiresMsg(t *testing.T) {
	for _, line := range []string{`{"x":1}`, `{"msg":"","x":1}`, `{"msg":"   ","x":1}`} {
		if rec, ok := parseLogLine([]byte(line)); ok {
			t.Errorf("%s 不该被当成日志记录，实际 %+v", line, rec)
		}
	}

	// 有 msg 的行照旧按真实等级重放，别把兜底做成了"JSON 全都不认"。
	rec, ok := parseLogLine([]byte(`{"level":"WARN","msg":"磁盘快满了","pct":91}`))
	if !ok {
		t.Fatal("有 msg 的 slog 行应当被解析")
	}
	if rec.level != slog.LevelWarn || rec.msg != "磁盘快满了" || len(rec.attrs) != 1 {
		t.Errorf("解析结果 = %+v，期望 WARN + 磁盘快满了 + 1 个 attr", rec)
	}
}
