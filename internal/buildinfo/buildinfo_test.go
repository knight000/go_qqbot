package buildinfo

import "testing"

// 重启原因从环境变量读，老版本写的是 "1"（#70 之前只有 /reload 会写它）。
//
// 这条兼容不是可有可无的：升级到 0.1.1 之后，第一次重启可能是被**旧**二进制
// 拉起的（换盘前的进程写 "1"），新核心必须照样认得出来。
func TestRestartReason(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"没设置（空值）", "", ""},
		{"0 当作没重启", "0", ""},
		{"老版本写的 bool", "1", RestartReload},
		{"/reload", RestartReload, RestartReload},
		{"升级换盘", RestartUpgrade, RestartUpgrade},
		{"升级回滚", RestartRollback, RestartRollback},
		{"未知原因原样返回", "hotfix", "hotfix"},
		{"前后空白要吃掉", "  upgrade\n", RestartUpgrade},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RestartEnv, tc.env)
			if got := RestartReason(); got != tc.want {
				t.Errorf("RestartReason() = %q want %q", got, tc.want)
			}
			if got, want := Restarted(), tc.want != ""; got != want {
				t.Errorf("Restarted() = %v want %v", got, want)
			}
		})
	}
}

// 展示口径只有一份：启动日志与启动通知都用它，两处说法不一致时排障的人
// 得先判断"这两句是不是同一个意思"。
func TestRestartCause(t *testing.T) {
	cases := map[string]string{
		RestartReload:   "/reload",
		RestartUpgrade:  "升级换盘",
		RestartRollback: "升级回滚",
		"hotfix":        "hotfix", // 未知原因不该被吞成空串
	}
	for reason, want := range cases {
		if got := RestartCause(reason); got != want {
			t.Errorf("RestartCause(%q) = %q want %q", reason, got, want)
		}
	}
}
