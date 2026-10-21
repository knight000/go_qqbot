package main

import (
	"testing"
	"time"
)

// 第二个实例不该白等：只有 /reload 的交接场景才需要给旧进程留余量。
func TestLockWait(t *testing.T) {
	if got := lockWait(false); got != 0 {
		t.Fatalf("普通启动应当立刻判失败，实际等了 %s", got)
	}
	if got := lockWait(true); got != 15*time.Second {
		t.Fatalf("/reload 交接应当留 15s 余量，实际 %s", got)
	}
}

// 启动日志要把实际生效的域名额度打出来，且顺序稳定（map 遍历是随机的）。
func TestFormatDomainLimits(t *testing.T) {
	got := formatDomainLimits(map[string]int{
		"api.torn.com": 800,
		"example.com":  50,
		"torn.com":     100,
	})
	want := "api.torn.com=800, example.com=50, torn.com=100"
	if got != want {
		t.Fatalf("formatDomainLimits=%q，期望 %q", got, want)
	}
	if got := formatDomainLimits(nil); got != "" {
		t.Fatalf("空清单应为空串，实际 %q", got)
	}
}
