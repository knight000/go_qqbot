package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 认领机制（§8.2）：openid 是相对机器人加密的，注册前不可预知，
// 因此不能在配置里硬编码首个管理员。启动时若 admins 为空，
// 生成一次性令牌打印到 journald，由第一个 /claim 的用户成为 owner。
//
// owner 是唯一且全局的：openid 跨场景同值，在单聊里认领的人进群后同样是 owner
// （store.AdminRole 不再按 scene 限定 openid）。首个认领同时关闭入口——
// /admin 没有任何"再造一个 owner"的路径。
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
	case "/group":
		c.trace.CoreCommand(cmd)
		c.handleGroup(ctx, ev)
		return true
	case "/reload", "/restart":
		c.trace.CoreCommand(cmd)
		c.handleReload(ctx, ev)
		return true
	case "/reset":
		// 分级重置由核心独占，插件不得声明（§8.3）。
		c.trace.CoreCommand(cmd)
		c.handleReset(ctx, ev)
		return true
	case "/upgrade":
		// 自更新的唯一入口（ADR-0006 决策 2/10）：换二进制等于远程代码执行，
		// 只能由核心独占，插件声明同名指令一律拒绝加载。
		c.trace.CoreCommand(cmd)
		c.handleUpgrade(ctx, ev)
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
		OpenID:      ev.SenderID,
		UnionID:     ev.UnionID,
		Role:        store.RoleOwner,
		GrantedBy:   "claim",
		SourceScene: string(ev.Scene),
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
		"用 /admin add <openid> [备注] 添加辅助管理员，/admin list 查看，/admin whoami 查自己的身份。\n"+
		"群准入默认开启：新群需用 /group allow <group_openid> 加入白名单，或由你在群内发 /group allow。")
}
