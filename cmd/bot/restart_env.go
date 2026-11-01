package main

import (
	"strings"

	"github.com/knight000/go_qqbot/internal/buildinfo"
)

// withRestartEnv 复制一份环境变量并把 QQBOT_RESTARTED 写成这次重启的原因。
//
// 先删掉旧的同名项：进程可能已经被重启过若干次，不清掉就会堆出一串同名变量，
// 读到的还可能是上一次的原因。
func withRestartEnv(env []string, reason string) []string {
	prefix := buildinfo.RestartEnv + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+reason)
}
