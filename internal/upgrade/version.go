package upgrade

import (
	"strconv"
	"strings"
)

// Compare 比较两个版本号，返回 -1 / 0 / 1。
//
// 只认「点分数字 + 可选前导 v」这一种形状：本仓库的版本号由 CI 从 tag
// （v1.2.3）去掉前缀后注入 -ldflags（ADR-0006 决策 9/11），没有预发布、
// 没有构建元数据。遇到别的形状（dev、v0.0.0-2026…、1.2.3-rc1）返回 ok=false，
// 由调用方决定是拒绝还是按「有变化」处理——不要在这里猜。
//
// 段数不同时短的那个按 0 补齐：1.2 == 1.2.0。
func Compare(a, b string) (int, bool) {
	x, ok := parseVersion(a)
	if !ok {
		return 0, false
	}
	y, ok := parseVersion(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		switch {
		case p < q:
			return -1, true
		case p > q:
			return 1, true
		}
	}
	return 0, true
}

// parseVersion 把 "v1.2.3" 拆成 [1 2 3]。
//
// 前导 v 允许缺失：tag 带 v、-ldflags 注入的值不带，两边都要能比。
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		// ParseUint 会接受 "001"，这里不额外限制——CI 不会产出那种形状，
		// 多一条规则只会多一处与 CI 不一致的地方。
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, false
		}
		out = append(out, int(n))
	}
	return out, true
}
