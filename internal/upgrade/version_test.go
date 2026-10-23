package upgrade

import "testing"

// 版本比较只服务三件事：判「是不是已经最新」、判插件「该不该升」、给预览
// 排个前后。任何一条判错都会让 /upgrade 要么空转、要么把用户单独升过的
// 插件降级（ADR-0006 决策 7）。
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.2.3", "1.2.3", 0, true},
		{"v1.2.3", "1.2.3", 0, true},
		{"1.2", "1.2.0", 0, true},
		{"1.2.3", "1.2.4", -1, true},
		{"1.2.10", "1.2.9", 1, true},
		{"2.0.0", "1.99.99", 1, true},
		{"0.1.0", "0.1.0", 0, true},
		// 非点分数字的形状一律不参与比较，交回调用方。
		{"dev", "1.0.0", 0, false},
		{"1.0.0", "dev", 0, false},
		{"v0.0.0-20261017073004-cc908d875f31", "1.0.0", 0, false},
		{"1.2.3-rc1", "1.2.3", 0, false},
		{"", "1.0.0", 0, false},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if ok != c.ok || got != c.want {
			t.Errorf("Compare(%q, %q) = %d,%v want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
