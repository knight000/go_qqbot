package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/api"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/httpx"
)

var (
	ErrPassiveExhausted = errors.New("被动回复次数已用尽")
	ErrActiveDisabled   = errors.New("主动消息未开启")
	// ErrActiveQuota 是本地主动消息日额度用尽。平台每用户每天上限 1000 条，
	// 本地把预算压到个位数，避免重启循环之类的异常把额度烧掉。
	ErrActiveQuota = errors.New("主动消息本地额度已用尽")
	// ErrRateLimited 表示平台返回 429。官方只给状态码 + err_code + trace_id，
	// 不提供剩余额度，也不承诺 Retry-After，因此退避策略必须由本地按维度决定。
	ErrRateLimited = errors.New("平台限流(HTTP 429)")
)

// ActivePolicy 是主动消息的本地闸门：总开关 + 插件白名单 + 每 peer 每日预算。
// 平台侧没有额度查询接口，只能靠本地记账（见 README「频控边界」）。
type ActivePolicy struct {
	Enabled      bool
	Plugins      map[string]bool
	DailyPerPeer int
}

// messageAPI 抽出来是为了让频控逻辑可单测（fake 掉 HTTP）。
type messageAPI interface {
	SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error
}

// Sender 负责 passive/active 两类出站。
//
// 平台约束（§1.2）：
//   - 群：同一 msg_id 5 分钟内最多 5 次；
//   - 单聊：60 分钟内最多 4 次；
//   - msg_id + msg_seq 全局唯一，重试不得复用同一 seq。
type Sender struct {
	api    messageAPI
	log    *slog.Logger
	active ActivePolicy
	trace  *Trace

	mu      sync.Mutex
	windows map[string]*passiveWindow
	lastGC  time.Time
	// 主动消息日预算：key 为 scene|peer，日切时整体重置
	activeUse map[string]int
	activeDay string
}

type passiveWindow struct {
	used    int
	limit   int
	ttl     time.Duration
	expires time.Time
}

func NewSender(api messageAPI, log *slog.Logger, active ActivePolicy) *Sender {
	return &Sender{
		api:       api,
		log:       log,
		active:    active,
		windows:   make(map[string]*passiveWindow),
		activeUse: make(map[string]int),
	}
}

func (s *Sender) Reply(ctx context.Context, ev *host.Event, msg host.Reply) error {
	start := time.Now()
	seq, err := s.sendPassive(ctx, ev, msg)
	// 无论成败都留一条轨迹：这是"插件回了什么"的唯一出口。
	s.trace.Sent("被动回复", ev.Scene, ev.PeerID, seq, msg.Content, err, time.Since(start))
	return err
}

func (s *Sender) sendPassive(ctx context.Context, ev *host.Event, msg host.Reply) (int, error) {
	if ev.MsgID == "" {
		return 0, fmt.Errorf("缺少 msg_id，无法被动回复")
	}
	if msg.Content == "" && msg.MsgType == 0 {
		return 0, fmt.Errorf("回复内容为空")
	}

	seq, err := s.reserve(ev)
	if err != nil {
		return 0, err
	}

	body := api.MessageBody{
		Content: msg.Content,
		MsgType: msg.MsgType,
		MsgID:   ev.MsgID,
		MsgSeq:  seq,
	}
	if err := s.api.SendMessage(ctx, string(ev.Scene), ev.PeerID, body); err != nil {
		if httpx.IsStatus(err, 429) {
			return seq, fmt.Errorf("%w (seq=%d): %v", ErrRateLimited, seq, err)
		}
		return seq, fmt.Errorf("发送消息(seq=%d): %w", seq, err)
	}
	return seq, nil
}

func (s *Sender) reserve(ev *host.Event) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.gcLocked(now)

	w, ok := s.windows[ev.MsgID]
	if !ok {
		limit, ttl := 4, 60*time.Minute
		if ev.Scene == host.SceneGroup {
			limit, ttl = 5, 5*time.Minute
		}
		w = &passiveWindow{limit: limit, ttl: ttl, expires: now.Add(ttl)}
		s.windows[ev.MsgID] = w
	}
	if now.After(w.expires) {
		w.used = 0
		w.expires = now.Add(w.ttl)
	}
	if w.used >= w.limit {
		return 0, ErrPassiveExhausted
	}
	w.used++
	return w.used, nil
}

// gcLocked 清掉过期窗口，避免 map 随会话数无限增长。调用方需持锁。
func (s *Sender) gcLocked(now time.Time) {
	if now.Sub(s.lastGC) < time.Minute {
		return
	}
	s.lastGC = now
	for k, w := range s.windows {
		if now.After(w.expires) {
			delete(s.windows, k)
		}
	}
}

// ForPlugin 返回带插件身份的 Sender：Reply 与全局一致，
// SendActive 只有在 [active].plugins 白名单里才放行。
func (s *Sender) ForPlugin(name string) host.Sender {
	return &pluginSender{parent: s, name: name}
}

type pluginSender struct {
	parent *Sender
	name   string
}

func (p *pluginSender) Reply(ctx context.Context, ev *host.Event, msg host.Reply) error {
	return p.parent.Reply(ctx, ev, msg)
}

func (p *pluginSender) SendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	if !p.parent.active.Enabled || !p.parent.active.Plugins[p.name] {
		err := fmt.Errorf("%w: 插件 %s 不在 [active].plugins 白名单内", ErrActiveDisabled, p.name)
		p.parent.trace.Sent("主动消息", scene, peerID, 0, msg.Content, err, 0)
		return err
	}
	return p.parent.SendActive(ctx, scene, peerID, msg)
}

// SendActive 发送主动消息（无 msg_id，不受被动窗口约束）。
// 只有白名单插件能走到这里，且先扣本地日预算再发 HTTP，
// 保证"额度用尽"时不会把请求打到平台。
func (s *Sender) SendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	start := time.Now()
	err := s.sendActive(ctx, scene, peerID, msg)
	s.trace.Sent("主动消息", scene, peerID, 0, msg.Content, err, time.Since(start))
	return err
}

func (s *Sender) sendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	if !s.active.Enabled {
		return ErrActiveDisabled
	}
	if peerID == "" {
		return fmt.Errorf("缺少 peer openid")
	}
	if msg.Content == "" && msg.MsgType == 0 {
		return fmt.Errorf("主动消息内容为空")
	}
	if !s.takeActiveBudget(scene, peerID) {
		return fmt.Errorf("%w（%s %s）", ErrActiveQuota, scene, peerID)
	}

	body := api.MessageBody{Content: msg.Content, MsgType: msg.MsgType}
	if err := s.api.SendMessage(ctx, string(scene), peerID, body); err != nil {
		if httpx.IsStatus(err, 429) {
			return fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
		return fmt.Errorf("发送主动消息: %w", err)
	}
	return nil
}

// takeActiveBudget 扣一次日预算；返回 false 表示已用尽。
func (s *Sender) takeActiveBudget(scene host.Scene, peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	day := time.Now().Format("2006-01-02")
	if s.activeDay != day {
		s.activeDay = day
		s.activeUse = make(map[string]int)
	}
	key := string(scene) + "|" + peerID
	if s.active.DailyPerPeer > 0 && s.activeUse[key] >= s.active.DailyPerPeer {
		return false
	}
	s.activeUse[key]++
	return true
}
