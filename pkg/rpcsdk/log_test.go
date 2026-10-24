package rpcsdk_test

// 插件日志通道的形状（#48）。
//
// 断言的是**进程边界上的事实**：夹具真的往 stderr 写了一行 slog JSON，而不是
// 「我们设置过某个 handler」。核心靠这条约定把插件的 WARN/ERROR 按真实等级
// 重放；换成 TextHandler 的输出，核心只能整行当 Info 打出来。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPluginStderrIsJSONLog(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)

	var lines []string
	for _, line := range strings.Split(p.stderrAfterStop(), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatal("夹具在 stderr 上什么都没写：用例没有可断言的对象")
	}

	var sawWarn bool
	for _, line := range lines {
		var rec struct {
			Time  string `json:"time"`
			Level string `json:"level"`
			Msg   string `json:"msg"`
			N     int64  `json:"n"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("插件 stderr 不是一行一条 JSON：%q（%v）", line, err)
		}
		if rec.Time == "" || rec.Level == "" {
			t.Fatalf("JSON 行缺 time/level：%q", line)
		}
		if rec.Level != "WARN" || rec.Msg != "testplugin 已初始化" {
			continue
		}
		// 字段也要真的在：结构化 attrs 是这次修复的另一半，只有等级对了不算完。
		if rec.N != 7 {
			t.Fatalf("WARN 行的 attrs 丢了：%q", line)
		}
		sawWarn = true
	}
	if !sawWarn {
		t.Fatalf("没有一行是 WARN 的初始化日志：\n%s", strings.Join(lines, "\n"))
	}
}
