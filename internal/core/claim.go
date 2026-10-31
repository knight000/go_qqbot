package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 认领（ADR-0008 决策 4/5）：openid 是相对机器人加密的，注册前不可预知，
// 因此首个管理员不能写进配置。凭证是**配置文件里这个 Bot 的 secret**——
// 谁知道 secret 谁就是所有者。Core 不再生成令牌、不再把任何凭据写进日志。
//
// 一次性、不转移：admins 非空即关闭。所有权重置的唯一路径是「平台重置
// secret → 改配置文件 → 重启该 Bot → 重新 /claim」。
//
// 拦截点在 Core.Handle 里，排在去重之后、落库之前：平台重投同一条消息只
// 认领一次，secret 也永远不会进 messages 表。入口只对私聊开放；这里再判一次
// 场景，是为了让直接调用 handleReserved 的路径同样拒绝群聊。
const (
	// claimGroupReject 是群里发 /claim 的统一回复：群里任何人都能看到回复，
	// 所以只说"去哪认领"，不提凭据形态。
	claimGroupReject = "认领只能在私聊里进行。"
	// claimBadSecret 是凭据不匹配的唯一答复：不区分空/短/错、不回显长度或
	// 前几位，避免把 secret 变成可以被逐位试探的预言机。
	claimBadSecret = "认领失败：凭据不正确。"
)

// announceClaimState 在启动时提示认领入口，且只提示"怎么认领"。
//
// 旧实现把随机令牌打进启动日志；新模型下 secret 本来就在配置文件里、由
// Bot 所有者自己持有，Core 不派发任何东西，因此这里绝不打印 secret。
func (c *Core) announceClaimState(ctx context.Context) error {
	if c.store == nil {
		return nil
	}
	n, err := c.store.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("检查管理员: %w", err)
	}
	if n > 0 {
		return nil
	}
	if c.secret == "" {
		c.log.Warn("该 Bot 尚未认领，但配置里没有 secret，认领无法完成",
			"bot", c.botID)
		return nil
	}
	c.log.Info("该 Bot 尚未认领：Bot 所有者可在私聊发送 /claim <secret>",
		"bot", c.botID, "hint", "secret 在配置文件里，由 Bot 所有者持有")
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
	case "/bot":
		// 进程级的 Bot 管理（ADR-0008 决策 9/15）：只认 Core 所有者，
		// 与 /reset（管一个 Bot 的数据）是两条不相交的线。
		c.trace.CoreCommand(cmd)
		c.handleBot(ctx, ev)
		return true
	}
	return false
}

func (c *Core) handleClaim(ctx context.Context, ev *host.Event) {
	if ev.Scene != host.SceneC2C {
		c.replyText(ctx, ev, claimGroupReject)
		return
	}

	// 串行化认领，避免两个用户同时通过 "admins 为空" 检查而双双成为 owner
	c.claimMu.Lock()
	defer c.claimMu.Unlock()

	// 先验格式与凭据，再看"是否已认领"：顺序反了的话，任何人对一个已认领的
	// Bot 发一句 /claim 就会得到"已被认领"，这条回复本身成了状态预言机。
	fields := strings.Fields(ev.Content)
	if len(fields) < 2 {
		c.replyText(ctx, ev, "用法：/claim <secret>（secret 在配置文件里，由 Bot 所有者持有）")
		return
	}

	if !c.claimSecretMatches(fields[1]) {
		// 日志里只有发送者与 Bot，没有提交值、没有长度、没有前缀。
		c.log.Warn("认领失败：secret 不匹配", "bot", c.botID, "sender", ev.SenderID)
		c.replyText(ctx, ev, claimBadSecret)
		return
	}

	n, err := c.store.CountAdmins(ctx)
	if err != nil {
		c.log.Error("读取管理员失败", "err", err)
		return
	}

	rotated := false
	if n > 0 {
		// 已认领：唯一能重开认领入口的情况是**配置里的 secret 在认领之后被换过**
		// （ADR-0008 决策 4 的恢复路径）。判据是认领时记下的指纹，不看时间、
		// 不看状态——平台重置后旧 secret 立刻失效，Bot 会掉进 failing，那时
		// 靠状态判断已经晚了。
		rotated, err = c.secretRotated(ctx)
		if err != nil {
			c.log.Error("读取认领指纹失败", "err", err)
			c.replyText(ctx, ev, "认领失败，请查看日志。")
			return
		}
		if !rotated {
			c.replyText(ctx, ev, "该 Bot 已被认领，认领入口已关闭（不支持转移）。\n"+
				"需要换人：在平台重置 secret → 改配置文件 → /bot reload <appid> → 重新 /claim。")
			return
		}
	}

	admin := store.Admin{
		OpenID:      ev.SenderID,
		UnionID:     ev.UnionID,
		Role:        store.RoleOwner,
		GrantedBy:   "claim",
		SourceScene: string(ev.Scene),
	}
	if rotated {
		// secret 换过 = 托管方或租户主动重置过所有权，旧管理员一律作废。
		// 这是 /admin 永远不会做的事，所以只能由这条证明过的路径来删。
		removed, err := c.store.ReplaceAdmins(ctx, admin)
		if err != nil {
			c.log.Error("重置管理员失败", "err", err)
			c.replyText(ctx, ev, "认领失败，请查看日志。")
			return
		}
		c.log.Warn("secret 已轮换，所有权已重置",
			"bot", c.botID, "openid", ev.SenderID, "removed_admins", removed)
	} else if err := c.store.UpsertAdmin(ctx, admin); err != nil {
		c.log.Error("写入管理员失败", "err", err)
		c.replyText(ctx, ev, "认领失败，请查看日志。")
		return
	}

	if err := c.saveSecretFingerprint(ctx); err != nil {
		// 指纹写不进去只影响"下一次能不能重置"，不该让这次认领失败：
		// 此时管理员已经落库，回滚反而是更坏的结果。
		c.log.Warn("认领指纹未落库，下次 secret 轮换将无法自动重置",
			"bot", c.botID, "err", err)
	}

	// 认领是 pending_claim → active 的唯一迁移点：从这一刻起闸门才放行普通事件。
	c.transition(ctx, BotStateActive, "认领成功")
	c.log.Info("Bot 认领成功", "bot", c.botID, "openid", ev.SenderID,
		"scene", ev.Scene, "reset", rotated)
	if rotated {
		c.replyText(ctx, ev, "认领成功：secret 已轮换，原管理员已被替换，你是这个 Bot 唯一的所有者。\n"+
			"用 /admin add <openid> [备注] 重新添加辅助管理员。")
		return
	}
	c.replyText(ctx, ev, "认领成功，你已成为这个 Bot 的所有者。\n"+
		"用 /admin add <openid> [备注] 添加辅助管理员，/admin list 查看，/admin whoami 查自己的身份。\n"+
		"群准入默认开启：新群需用 /group allow <group_openid> 加入白名单，或由你在群内发 /group allow。\n"+
		"所有权不可转移：需要换人时在平台重置 secret、改配置文件后重新认领。")
}

// saveSecretFingerprint 记下这次认领所用 secret 的指纹（不存明文）。
func (c *Core) saveSecretFingerprint(ctx context.Context) error {
	if c.secret == "" {
		return nil
	}
	return c.store.PutState(ctx, StateKeyClaimSecretFP, secretFingerprint(c.secret))
}

// secretRotated 判断配置里的 secret 是否与认领时记录的不一样。
//
// 没有指纹记录（升级前就认领过的库）返回 false：宁可让运维手工处理，也不让
// "缺指纹"变成一个可以绕过"已认领"的缺口。
func (c *Core) secretRotated(ctx context.Context) (bool, error) {
	if c.secret == "" {
		return false, nil
	}
	recorded, ok, err := c.store.GetState(ctx, StateKeyClaimSecretFP)
	if err != nil {
		return false, err
	}
	if !ok || recorded == "" {
		return false, nil
	}
	return recorded != secretFingerprint(c.secret), nil
}

// secretFingerprint 是 secret 的 sha256 十六进制。
//
// 只比指纹不存原文：bot_state 会被 R3/备份带出去，存明文等于把凭据另存一份。
func secretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// claimSecretMatches 与配置里的 secret 做常量时间比对。
//
// 空 secret（配置缺失）与空提交都直接判否：把 "" 当合法凭据等于任何人都能认领。
// 返回 false 的三种情况（空、长度不同、内容不同）对外共用同一句回复。
func (c *Core) claimSecretMatches(got string) bool {
	if c.secret == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(c.secret)) == 1
}

// redactSecret 把认领凭证从任何将要落盘的字节里抹掉。
//
// 轨迹的原始载荷（`[debug].raw=true`）打印的是平台下发的 JSON 原文，其中就有
// 这条消息的 content——一次 /claim 等于把 secret 抄进 stdout / journald，还会
// 被日志轮转长期保留。按字面量替换而不是"解析 JSON 再看前缀"，是因为字段名、
// 层级、编解码方式以后都可能变，而"这个字符串不该出现在日志里"不会变。
//
// 代价是 secret 若出现在别处（比如用户把它贴进某条普通消息）也会被打码——
// 那种情况本来就是泄露，打码反而是对的。
func (c *Core) redactSecret(b []byte) []byte {
	if c.secret == "" || len(b) == 0 {
		return b
	}
	s := []byte(c.secret)
	if !bytes.Contains(b, s) {
		return b
	}
	return bytes.ReplaceAll(b, s, []byte("***"))
}
