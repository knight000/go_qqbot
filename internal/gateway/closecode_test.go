package gateway

import (
	"strings"
	"testing"
	"time"
)

// 关闭码 → 动作的映射是这张表的核心：错一个数字就会变成
// "永远 Resume 死 session"（4006/4007）或"每 60s 静默重连"（4914/4915）。
// 平台口径见 bot.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/websocket.html。
func TestClassifyClose(t *testing.T) {
	cases := []struct {
		code   int
		action closeAction
		minDel time.Duration
	}{
		// 参数/载荷写错：重连一千次也一样，是部署期就该发现的 bug。
		{4001, actionFatal, 0},
		{4002, actionFatal, 0},
		// 鉴权失败：作废 token 后重连，不能继续拿缓存 token 撞。
		{4004, actionInvalidToken, 0},
		// session 不可用 / seq 错误：必须重新 Identify。
		{4006, actionReIdentify, 0},
		{4007, actionReIdentify, 0},
		// 发送过快：平台要求降频，等够 60s 再连。
		{4008, actionRateLimited, 60 * time.Second},
		// 连接过期：官方要求 Resume，不要顺手清 session。
		{4009, actionResume, 0},
		// 分片/权限/频率：永久性。
		{4010, actionFatal, 0},
		{4011, actionFatal, 0},
		{4012, actionFatal, 0},
		{4013, actionFatal, 0},
		{4014, actionFatal, 0},
		// 4900–4913 是平台内部错误：重新 Identify 是官方给的处置方式。
		{4900, actionReIdentify, 0},
		{4901, actionReIdentify, 0},
		{4902, actionReIdentify, 0},
		{4903, actionReIdentify, 0},
		{4904, actionReIdentify, 0},
		{4905, actionReIdentify, 0},
		{4906, actionReIdentify, 0},
		{4907, actionReIdentify, 0},
		{4908, actionReIdentify, 0},
		{4909, actionReIdentify, 0},
		{4910, actionReIdentify, 0},
		{4911, actionReIdentify, 0},
		{4912, actionReIdentify, 0},
		{4913, actionReIdentify, 0},
		// 下架 / 封禁：永久性。
		{4914, actionFatal, 0},
		{4915, actionFatal, 0},
		// 未知关闭码：按普通链路故障退避重连，不动 session。
		{1000, actionReconnect, 0},
		{4401, actionReconnect, 0},
		{-1, actionReconnect, 0},
	}

	for _, tc := range cases {
		d := classifyClose(tc.code)
		if d.action != tc.action {
			t.Errorf("关闭码 %d: 动作 = %v，期望 %v（原因 %q）", tc.code, d.action, tc.action, d.reason)
		}
		if d.delay < tc.minDel {
			t.Errorf("关闭码 %d: 最短等待 %v，期望不少于 %v", tc.code, d.delay, tc.minDel)
		}
		if d.action != actionReconnect && d.reason == "" {
			t.Errorf("关闭码 %d: 必须给出可读原因", tc.code)
		}
	}
}

// 4914/4915 的文案必须是"下架 / 封禁"，而不是把运维指到 intents 配置上——
// 这是审查报告点名的"日志指错方向"。
func TestFatalReasonsForDelistedAndBanned(t *testing.T) {
	for code, want := range map[int]string{4914: "下架", 4915: "封禁"} {
		got := classifyClose(code).reason
		if !strings.Contains(got, want) {
			t.Errorf("关闭码 %d 的原因应包含 %q，实际 %q", code, want, got)
		}
		if strings.Contains(got, "intents") {
			t.Errorf("关闭码 %d 的原因不该提 intents，实际 %q", code, got)
		}
	}
}

// 4900–4913 是平台内部错误，文案不能暗示"你的配置有问题"。
func TestReIdentifyReasonsAreNotBlamedOnConfig(t *testing.T) {
	for code := 4900; code <= 4913; code++ {
		got := classifyClose(code).reason
		if strings.Contains(got, "intents") || strings.Contains(got, "配置") {
			t.Errorf("关闭码 %d 的原因不该指向本地配置，实际 %q", code, got)
		}
	}
}

// 4008 的"至少 60s"要落到实际等待上，而不是只写在注释里。
func TestRateLimitedWaitsAtLeast60s(t *testing.T) {
	d := classifyClose(4008)
	var b backoff
	// 从小退避开始，确保是下限在起作用，而不是退避自己已经很大。
	for i := 0; i < 3; i++ {
		b.next(0)
	}
	b.d = time.Second
	wait := b.next(d.delay)
	if wait < 60*time.Second {
		t.Fatalf("4008 之后应至少等 60s，实际 %v", wait)
	}
	// 抖动只能加长等待，不能把下限削掉。
	for i := 0; i < 20; i++ {
		b.d = time.Second
		if got := b.next(60 * time.Second); got < 60*time.Second {
			t.Fatalf("第 %d 次采样低于 60s 下限: %v", i, got)
		}
	}
}

// "连上 200ms 就断"不能把退避复位成 1s：那会让平台拒绝 token / 握手被 reset
// 时退化成 1s 一次的快速撞墙。只有 READY 之后活够 stableReadyWindow 才复位。
func TestBackoffResetsOnlyAfterStableReady(t *testing.T) {
	var b backoff
	for i := 0; i < 10; i++ {
		b.next(0) // 一路涨到 60s 上限
	}
	grown := b.d
	if grown < maxBackoff {
		t.Fatalf("连续失败后退避应涨到 %v，实际 %v", maxBackoff, grown)
	}

	b.note(200 * time.Millisecond) // 连上就断
	if b.d != grown {
		t.Fatalf("短命连接不应复位退避：%v → %v", grown, b.d)
	}

	b.note(stableReadyWindow + time.Second)
	if b.d != minBackoff {
		t.Fatalf("稳定 READY 之后退避应复位成 %v，实际 %v", minBackoff, b.d)
	}
	// 复位后的第一次等待必须回到 1s 量级（带抖动），不能还粘着 60s。
	if wait := b.next(0); wait >= 2*time.Second {
		t.Fatalf("复位后本轮等待应回到 1s 量级，实际 %v", wait)
	}
}
