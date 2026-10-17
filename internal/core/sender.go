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
	ErrActiveDisabled   = errors.New("主动消息默认关闭")
	// ErrRateLimited 表示平台返回 429。官方只给状态码 + err_code + trace_id，
	// 不提供剩余额度，也不承诺 Retry-After，因此退避策略必须由本地按维度决定。
	ErrRateLimited = errors.New("平台限流(HTTP 429)")
)

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
	api messageAPI
	log *slog.Logger

	mu      sync.Mutex
	windows map[string]*passiveWindow
	lastGC  time.Time
}

type passiveWindow struct {
	used    int
	limit   int
	ttl     time.Duration
	expires time.Time
}

func NewSender(api messageAPI, log *slog.Logger) *Sender {
	return &Sender{api: api, log: log, windows: make(map[string]*passiveWindow)}
}

func (s *Sender) Reply(ctx context.Context, ev *host.Event, msg host.Reply) error {
	if ev.MsgID == "" {
		return fmt.Errorf("缺少 msg_id，无法被动回复")
	}
	if msg.Content == "" && msg.MsgType == 0 {
		return fmt.Errorf("回复内容为空")
	}

	seq, err := s.reserve(ev)
	if err != nil {
		return err
	}

	body := api.MessageBody{
		Content: msg.Content,
		MsgType: msg.MsgType,
		MsgID:   ev.MsgID,
		MsgSeq:  seq,
	}
	if err := s.api.SendMessage(ctx, string(ev.Scene), ev.PeerID, body); err != nil {
		if httpx.IsStatus(err, 429) {
			return fmt.Errorf("%w (seq=%d): %v", ErrRateLimited, seq, err)
		}
		return fmt.Errorf("发送消息(seq=%d): %w", seq, err)
	}
	return nil
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

// SendActive 主动消息默认全关（§1.2 / §7）。若要开启，
// 必须按 (插件, 场景) 白名单逐项配置，当前脚手架刻意不提供开关。
func (s *Sender) SendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	return ErrActiveDisabled
}
