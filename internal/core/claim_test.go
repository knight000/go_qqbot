package core

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// testBotSecret 是测试里"配置文件里那个 secret"。真实部署里它来自
// config.Bot.Secret，由 internal/instance 装配时注入 core.Options。
const testBotSecret = "s3cret-for-tests"

// seedCoreOwners 模拟 `[core].owners`：进程级身份，与 admins 表无关。
func seedCoreOwners(c *Core, ids ...string) {
	c.cfg.Core.Owners = ids
}

func adminCount(t *testing.T, c *Core) int {
	t.Helper()
	n, err := c.store.CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("读管理员数量: %v", err)
	}
	return n
}

// 群里发 /claim 一律拒：不落 messages、不进幂等表、不回显任何凭据信息。
func TestClaimOnlyInPrivateChat(t *testing.T) {
	c, f := newStateCore(t)
	markActive(t, c.store)
	ctx := context.Background()

	c.Handle(ctx, adminEvent("/claim "+testBotSecret, "u1", "", host.SceneGroup, "g1"))

	if got := lastReply(t, f); !strings.Contains(got, "私聊") {
		t.Fatalf("群里认领应被拒绝并指路私聊，实际: %q", got)
	}
	if n := countRows(t, c.store, "messages"); n != 0 {
		t.Errorf("群里的认领不得落库，messages 实际 %d 行", n)
	}
	// 拦截点在去重之前：连"处理过这条消息"的痕迹都不该留。
	if n := countRows(t, c.store, "processed_events"); n != 0 {
		t.Errorf("群里的认领不得进幂等表，实际 %d 行", n)
	}
	if n := adminCount(t, c); n != 0 {
		t.Errorf("群里的认领不得写入管理员，实际 %d 条", n)
	}
}

// 平台重投同一条认领消息：只认领一次、只回一次。
func TestClaimRedeliveryClaimsOnce(t *testing.T) {
	c, f := newStateCore(t)
	ctx := context.Background()

	ev := adminEvent("/claim "+testBotSecret, "u1", "", host.SceneC2C, "u1")
	c.Handle(ctx, ev)
	c.Handle(ctx, ev)

	if n := adminCount(t, c); n != 1 {
		t.Fatalf("重投应只认领一次，admins 实际 %d 条", n)
	}
	if n := countRows(t, c.store, "messages"); n != 0 {
		t.Errorf("认领是控制动作，messages 应零行，实际 %d", n)
	}
	if n := countRows(t, c.store, "processed_events"); n != 1 {
		t.Errorf("去重应挡下第二次投递，processed_events 实际 %d 行", n)
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("重投不该重复回复，实际发送 %d 条", n)
	}
}

// 凭据不匹配只有一句话：不区分空/短/错，也不回显长度或前几位。
func TestClaimWrongSecretIsOpaque(t *testing.T) {
	c, f := newStateCore(t)
	ctx := context.Background()

	wrongs := []string{
		testBotSecret[:len(testBotSecret)-1], // 差一位
		"x",                                  // 长度完全不同
		testBotSecret + "x",                  // 多一位
		"完全无关的字符",
	}
	var replies []string
	for i, wrong := range wrongs {
		c.Handle(ctx, adminEvent("/claim "+wrong, "u"+string(rune('a'+i)), "", host.SceneC2C, "u"))
		got := lastReply(t, f)
		if got != claimBadSecret {
			t.Fatalf("第 %d 个错误凭据的回复应与其他情况一致，实际: %q", i, got)
		}
		for _, leak := range []string{wrong, testBotSecret, "长度"} {
			if leak != "" && strings.Contains(got, leak) {
				t.Errorf("回复泄露了信息 %q: %q", leak, got)
			}
		}
		replies = append(replies, got)
	}
	for i := 1; i < len(replies); i++ {
		if replies[i] != replies[0] {
			t.Fatalf("不同错误应得到完全相同的回复，否则能从措辞推断长度: %q vs %q",
				replies[0], replies[i])
		}
	}
	if n := adminCount(t, c); n != 0 {
		t.Errorf("比对失败不得写入管理员，实际 %d 条", n)
	}
}

// 已认领 → 拒绝，且不产生第二个所有者（不支持转移）。
func TestClaimRejectedWhenAlreadyClaimed(t *testing.T) {
	c, f := newAdminCore(t)
	c.secret = testBotSecret
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	c.Handle(context.Background(),
		adminEvent("/claim "+testBotSecret, "intruder", "", host.SceneC2C, "intruder"))

	if got := lastReply(t, f); !strings.Contains(got, "已被认领") {
		t.Fatalf("已认领应明确拒绝，实际: %q", got)
	}
	if n := adminCount(t, c); n != 1 {
		t.Fatalf("不得写入第二个所有者，admins 实际 %d 条", n)
	}
}

// 配置换了 secret 之后：旧值一定失败、新值一定成功（所有权重置路径）。
func TestClaimFollowsConfiguredSecret(t *testing.T) {
	c, f := newStateCore(t)
	ctx := context.Background()

	// 模拟"改配置文件后重开 Core"：同一个 Core 的凭证换成新值。
	c.secret = "rotated-secret-value"

	c.Handle(ctx, adminEvent("/claim "+testBotSecret, "u1", "", host.SceneC2C, "u1"))
	if got := lastReply(t, f); got != claimBadSecret {
		t.Fatalf("旧 secret 必须失效，实际: %q", got)
	}
	if n := adminCount(t, c); n != 0 {
		t.Fatal("旧 secret 不该认领成功")
	}

	c.Handle(ctx, adminEvent("/claim rotated-secret-value", "u1", "", host.SceneC2C, "u1"))
	if got := lastReply(t, f); !strings.Contains(got, "认领成功") {
		t.Fatalf("新 secret 应认领成功，实际: %q", got)
	}
	if st := stateOf(t, c); st != BotStateActive {
		t.Fatalf("认领成功应迁移到 active，实际 %q", st)
	}
}

// 权限矩阵：进程级命令只认 Core 所有者，Bot 所有者在任意 Bot 里都不够；
// Core 所有者不需要是那个 Bot 的 admin。
func TestProcessCommandsRequireCoreOwner(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "bot-owner", "", store.RoleOwner)
	seedCoreOwners(c, "core-1")
	ctx := context.Background()

	// Bot 所有者发 /bot → 拒。
	c.handleReserved(ctx, adminEvent("/bot status", "bot-owner", "", host.SceneC2C, "bot-owner"))
	if got := lastReply(t, f); !strings.Contains(got, "Core 所有者") {
		t.Fatalf("Bot 所有者不该能用 /bot，实际: %q", got)
	}

	// Core 所有者（不是这个 Bot 的 admin）发 /bot → 通过，走到 T8 占位回复。
	c.handleReserved(ctx, adminEvent("/bot status", "core-1", "", host.SceneC2C, "core-1"))
	got := lastReply(t, f)
	if strings.Contains(got, "仅 Core 所有者") || !strings.Contains(got, "T8") {
		t.Fatalf("Core 所有者应能使用 /bot（占位回复），实际: %q", got)
	}

	// 租户（Bot 管理员）发 /reload → 拒；Core 所有者 → 入队。
	seedAdmin(t, c, "tenant-admin", "", store.RoleAdmin)
	c.handleReserved(ctx, adminEvent("/reload", "tenant-admin", "", host.SceneC2C, "tenant-admin"))
	if got := lastReply(t, f); !strings.Contains(got, "Core 所有者") {
		t.Fatalf("租户不该能重启进程，实际: %q", got)
	}
	c.handleReserved(ctx, adminEvent("/reload", "core-1", "", host.SceneC2C, "core-1"))
	if n := len(c.restart); n != 1 {
		t.Fatalf("Core 所有者应能重启进程，队列 %d", n)
	}
}

// Bot 级命令仍按 admins 判定：Core 所有者不是这个 Bot 的 admin 时，
// /admin 不因进程级身份而放行（两类所有者不相交）。
func TestBotCommandsStillRequireBotAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	seedCoreOwners(c, "core-1")

	c.handleReserved(context.Background(),
		adminEvent("/admin list", "core-1", "", host.SceneC2C, "core-1"))
	if got := lastReply(t, f); !strings.Contains(got, "仅管理员") {
		t.Fatalf("Core 所有者不该自动成为 Bot 管理员，实际: %q", got)
	}
}

// 原始载荷（[debug].raw=true）打印平台下发的 JSON 原文，里面就有 content。
// 这条路径不经过 Received 的正文脱敏，是 /claim 正文仅剩的泄露面。
func TestRawDumpRedactsSecret(t *testing.T) {
	c, _ := newStateCore(t)
	var buf bytes.Buffer
	c.trace = NewTrace(&buf, LevelFull)

	c.PrintRaw("C2C_MESSAGE_CREATE",
		[]byte(`{"d":{"id":"e1","content":"/claim `+testBotSecret+`"}}`))

	got := buf.String()
	if strings.Contains(got, testBotSecret) {
		t.Fatalf("原始载荷里读到了 secret：\n%s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("应留下打码痕迹，而不是整条静默丢弃：\n%s", got)
	}
	// 除 secret 之外的结构必须原样保留，否则这条调试轨迹就没有价值了。
	if !strings.Contains(got, `"id": "e1"`) {
		t.Errorf("打码不该破坏载荷结构：\n%s", got)
	}
}
