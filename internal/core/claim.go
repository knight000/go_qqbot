package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// 认领机制（§8.2）：openid 是相对机器人加密的，注册前不可预知，
// 因此不能在配置里硬编码首个管理员。启动时若 admins 为空，
// 生成一次性令牌打印到 journald，由第一个 /claim 的用户成为 owner。
const claimTTL = 10 * time.Minute

func (c *Core) ensureOwner(ctx context.Context) error {
	n, err := c.store.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("检查管理员: %w", err)
	}
	if n > 0 {
		return nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("生成认领令牌: %w", err)
	}
	c.claimToken = hex.EncodeToString(buf)
	c.claimExp = time.Now().Add(claimTTL)

	c.log.Warn("未检测到管理员，已生成一次性认领令牌",
		"ttl", claimTTL.String(),
		"token", c.claimToken,
		"用法", "在目标群或单聊发送 /claim <token>")
	return nil
}

func (c *Core) handleReserved(ctx context.Context, ev *host.Event) bool {
	if !ev.IsMessage() {
		return false
	}
	content := strings.TrimSpace(ev.Content)
	if !strings.HasPrefix(content, "/") {
		return false
	}
	cmd := strings.ToLower(strings.Fields(content)[0])

	switch cmd {
	case "/claim":
		c.trace.CoreCommand(cmd)
		c.handleClaim(ctx, ev)
		return true
	case "/admin":
		c.trace.CoreCommand(cmd)
		c.handleAdmin(ctx, ev)
		return true
	case "/reset":
		// 分级重置由核心独占，插件不得声明；具体逻辑属于下一步实现。
		c.trace.CoreCommand(cmd)
		c.replyText(ctx, ev, "该指令属于核心保留区，当前脚手架版本尚未启用。")
		return true
	case "/grant", "/revoke":
		c.trace.CoreCommand(cmd)
		c.replyText(ctx, ev, "已改为 /admin add <openid> 与 /admin rm <openid>（仅 owner）。")
		return true
	}
	return false
}

func (c *Core) handleClaim(ctx context.Context, ev *host.Event) {
	// 串行化认领，避免两个用户同时通过 "admins 为空" 检查而双双成为 owner
	c.claimMu.Lock()
	defer c.claimMu.Unlock()

	n, err := c.store.CountAdmins(ctx)
	if err != nil {
		c.log.Error("读取管理员失败", "err", err)
		return
	}
	if n > 0 {
		c.replyText(ctx, ev, "已存在管理员，认领入口已关闭。")
		return
	}

	fields := strings.Fields(ev.Content)
	if len(fields) < 2 {
		c.replyText(ctx, ev, "用法：/claim <token>（令牌见启动日志）")
		return
	}

	if c.claimToken == "" || time.Now().After(c.claimExp) {
		c.replyText(ctx, ev, "认领令牌已失效。请重启进程后从日志重新获取。")
		return
	}
	if subtle.ConstantTimeCompare([]byte(fields[1]), []byte(c.claimToken)) != 1 {
		c.replyText(ctx, ev, "令牌不正确。")
		return
	}

	admin := store.Admin{
		OpenID:    ev.SenderID,
		UnionID:   ev.UnionID,
		Scene:     string(ev.Scene),
		PeerScope: "", // owner 在该场景内全局有效
		Role:      store.RoleOwner,
		GrantedBy: "claim",
	}
	if err := c.store.UpsertAdmin(ctx, admin); err != nil {
		c.log.Error("写入管理员失败", "err", err)
		c.replyText(ctx, ev, "认领失败，请查看日志。")
		return
	}

	c.claimToken = ""
	c.claimExp = time.Time{}
	c.log.Info("管理员认领成功", "openid", ev.SenderID, "union", ev.UnionID, "scene", ev.Scene)
	c.replyText(ctx, ev, "认领成功，你已成为 owner。\n"+
		"用 /admin add <openid> [备注] 添加辅助管理员，/admin list 查看，/admin whoami 查自己的身份。")
}
