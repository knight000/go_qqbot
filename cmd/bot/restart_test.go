package main

import (
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/upgrade"
)

// 写进子进程环境的原因必须替换旧值，而不是追加：进程可能已经被重启过几次，
// 堆出一串同名变量之后，新进程读到的可能是上一次的原因。
func TestWithRestartEnvReplacesOldReason(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		buildinfo.RestartEnv + "=reload",
		"HOME=/root",
		buildinfo.RestartEnv + "=1",
	}
	out := withRestartEnv(env, buildinfo.RestartUpgrade)

	var got []string
	for _, kv := range out {
		if strings.HasPrefix(kv, buildinfo.RestartEnv+"=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != buildinfo.RestartEnv+"=upgrade" {
		t.Fatalf("应恰好留下一条原因 upgrade，实际 %v", got)
	}
	// 其它变量一个都不能丢：env 是整份复制过去的。
	for _, want := range []string{"PATH=/usr/bin", "HOME=/root"} {
		found := false
		for _, kv := range out {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("丢了 %q", want)
		}
	}
}

// 判据是 Plan 而不是 req.Reason 里 owner 随手写的说明：/upgrade apply 与
// /reload 共用一条重启路径，只有 Plan 能区分"这次顺带换盘"（ADR-0006 决策 8）。
func TestRestartReason(t *testing.T) {
	cases := []struct {
		name string
		req  *core.RestartRequest
		want string
	}{
		{"换盘", &core.RestartRequest{Reason: "手滑", Plan: &upgrade.Plan{From: "0.1.0", To: "0.1.1"}}, buildinfo.RestartUpgrade},
		{"纯重载", &core.RestartRequest{Reason: "换二进制"}, buildinfo.RestartReload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartReason(tc.req); got != tc.want {
				t.Errorf("restartReason() = %q want %q", got, tc.want)
			}
		})
	}
}

// 启动日志那句话是 owner 升级后唯一能确认"换盘生效"的现场：换盘走
// syscall.Exec，PID 不变、NRestarts 不涨，只有这行能看出区别（#70）。
func TestRestartLine(t *testing.T) {
	cases := map[string]string{
		"":                        "", // 首次启动：没有这一行
		buildinfo.RestartReload:   "本次启动由/reload触发",
		buildinfo.RestartUpgrade:  "本次启动由升级换盘触发",
		buildinfo.RestartRollback: "本次启动由升级回滚触发",
	}
	for reason, want := range cases {
		if got := restartLine(reason); got != want {
			t.Errorf("restartLine(%q) = %q want %q", reason, got, want)
		}
	}
}
