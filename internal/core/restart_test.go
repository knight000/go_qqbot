package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /reload 重启整个进程：Bot 管理员也不够，只认 Core 所有者（ADR-0008 决策 9）。
func TestReloadRejectsNonCoreOwner(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "bot-owner", "", store.RoleOwner)
	if !c.handleReserved(context.Background(), adminEvent("/reload", "stranger", "", host.SceneC2C, "stranger")) {
		t.Fatal("/reload 应被核心消费，不能落到插件")
	}
	if got := lastReply(t, f); !strings.Contains(got, "Core 所有者") {
		t.Fatalf("非 Core 所有者应被拒绝，实际: %q", got)
	}

	// Bot 所有者同样被拒：他管的是这个 Bot，不是整台机器。
	c.handleReserved(context.Background(), adminEvent("/reload", "bot-owner", "", host.SceneC2C, "bot-owner"))
	if got := lastReply(t, f); !strings.Contains(got, "Core 所有者") {
		t.Fatalf("Bot 所有者也不该重启进程，实际: %q", got)
	}
	select {
	case req := <-c.restart:
		t.Fatalf("非 Core 所有者不应产生重启请求: %+v", req)
	default:
	}
}

// Core 所有者在任意 Bot 里都能重启，不要求是这个 Bot 的 admin。
func TestReloadEnqueuesForCoreOwner(t *testing.T) {
	c, f := newAdminCore(t)
	seedCoreOwners(c, "core-1")

	c.handleReserved(context.Background(),
		adminEvent("/reload 升级到新构建", "core-1", "", host.SceneC2C, "core-1"))

	got := lastReply(t, f)
	for _, want := range []string{"正在重启", "版本", "已运行", "备注 升级到新构建"} {
		if !strings.Contains(got, want) {
			t.Errorf("回复缺少 %q:\n%s", want, got)
		}
	}

	select {
	case req := <-c.restart:
		if req.By != "core-1" || req.Role != "core_owner" {
			t.Fatalf("重启请求的身份不对: %+v", req)
		}
		if req.Reason != "升级到新构建" {
			t.Fatalf("备注未透传: %q", req.Reason)
		}
	default:
		t.Fatal("应产生一条重启请求")
	}
}

// 同一个进程只接受一条重启请求：第二条只回提示，不再入队。
func TestReloadIsIdempotentWithinProcess(t *testing.T) {
	c, f := newAdminCore(t)
	seedCoreOwners(c, "u1")
	ctx := context.Background()

	c.handleReserved(ctx, adminEvent("/reload", "u1", "", host.SceneC2C, "u1"))
	c.handleReserved(ctx, adminEvent("/reload again", "u1", "", host.SceneC2C, "u1"))

	if got := lastReply(t, f); !strings.Contains(got, "正在重启") {
		// 第二条会被去重逻辑挡住吗？事件 ID 不同，所以会走到这里
		if !strings.Contains(got, "已有重启请求") {
			t.Fatalf("重复请求应提示已有重启，实际: %q", got)
		}
	}
	if n := len(c.restart); n != 1 {
		t.Fatalf("队列里应只有 1 条请求，实际 %d", n)
	}
}

func TestReloadHelpDoesNotRestart(t *testing.T) {
	c, f := newAdminCore(t)
	seedCoreOwners(c, "u1")

	c.handleReserved(context.Background(), adminEvent("/reload help", "u1", "", host.SceneC2C, "u1"))

	if got := lastReply(t, f); !strings.Contains(got, "/restart") {
		t.Fatalf("应返回用法，实际: %q", got)
	}
	if n := len(c.restart); n != 0 {
		t.Fatalf("help 不应入队，实际 %d", n)
	}
}

// /restart 是 /reload 的别名，两者行为一致。
func TestRestartAliasConsumed(t *testing.T) {
	c, _ := newAdminCore(t)
	seedCoreOwners(c, "u1")

	if !c.handleReserved(context.Background(), adminEvent("/restart", "u1", "", host.SceneC2C, "u1")) {
		t.Fatal("/restart 应被核心消费")
	}
	if n := len(c.restart); n != 1 {
		t.Fatalf("/restart 应入队，实际 %d", n)
	}
}

// 插件不能声明 /reload：否则一个第三方插件就能让机器人反复下线。
func TestPluginCannotClaimReloadPrefix(t *testing.T) {
	for _, prefix := range []string{"/reload", "/restart", "/RELOAD"} {
		if _, err := NewRouter([]host.Plugin{&fakePlugin{m: manifest("evil", 1, prefix)}}); err == nil {
			t.Fatalf("插件声明 %s 应被拒绝加载", prefix)
		}
	}
}

func TestHumanUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{3 * time.Second, "3秒"},
		{59 * time.Second, "59秒"},
		{5 * time.Minute, "5分"},
		{time.Hour + 2*time.Minute, "1小时2分"},
		{3 * time.Hour, "3小时"},
		{25*time.Hour + 3*time.Hour, "1天4小时"},
		{48 * time.Hour, "2天"},
	}
	for _, c := range cases {
		if got := humanUptime(c.d); got != c.want {
			t.Errorf("humanUptime(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestIsGoRunBinary(t *testing.T) {
	if !isGoRunBinary(`C:\Users\x\AppData\Local\Temp\go-build123\b001\exe\bot.exe`) {
		t.Error("应识别 go run 的临时构建")
	}
	if !isGoRunBinary("/tmp/go-build998/b001/exe/bot") {
		t.Error("应识别 go run 的临时构建（unix）")
	}
	if isGoRunBinary("/opt/qqbot/bin/qqbot") {
		t.Error("普通部署路径不应误判")
	}
}

// 版本信息至少要能给出平台与版本，commit 缺失时也不能 panic。
func TestBuildInfoString(t *testing.T) {
	bi := buildinfo.Get()
	if bi.Platform == "" || bi.Version == "" || bi.GoVer == "" {
		t.Fatalf("版本信息不完整: %+v", bi)
	}
	if !strings.HasPrefix(bi.String(), bi.Version) {
		t.Fatalf("String() 应以版本号开头: %q", bi.String())
	}
}
