package main

import (
	"strings"

	"github.com/knight000/go_qqbot/internal/buildinfo"
)

// withRestartEnv 复制一份环境变量并写入 QQBOT_RESTARTED=1。
// 先删掉旧的同名项，避免多次 /reload 后堆出一串重复值。
func withRestartEnv(env []string) []string {
	prefix := buildinfo.RestartEnv + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+"1")
}
