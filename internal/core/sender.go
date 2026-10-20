package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/httpx"
)

var (
	ErrPassiveExhausted = errors.New("被动回复次数已用尽")
	ErrActiveDisabled   = errors.New("主动消息未开启")
	// ErrActiveQuota 是本地主动消息日额度用尽。平台每用户每天上限 1000 条，
	// 本地把预算压到个位数并**落库**（§4.1 active_usage）：重启不清零，
	// 否则"重启循环不会烧额度"就只是注释里的一句空话。
	// 额度账本不可用时也返回它（fail-closed），调用方只需判断这一个错误。
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

// messageAPI 抽出来是为了让频控与发图逻辑可单测（fake 掉 HTTP）。
// 发文本与发图共用同一个出口：上传也走这里，测试才能验证"先传后发"的顺序。
type messageAPI interface {
	SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error
	UploadMedia(ctx context.Context, scene, peer string, up api.MediaUpload) (api.MediaUploadResult, error)
}

// activeUsageStore 是主动消息日预算的落库入口。核心不能写 plugin_kv
// （那是插件命名空间），所以由 store 单独维护一张 active_usage 表。
//
// 抽成接口而不是直接用 *store.DB，是为了让"账本坏了"这条路径可测：
// 关掉库、或者换一个只会报错的实现，就能验证 fail-closed 的行为。
type activeUsageStore interface {
	// TakeActiveBudget 原子地占一格当日额度；false 表示当日已满。
	// 返回 error 表示账本不可用，调用方必须拒发（见 sendActiveOne）。
	TakeActiveBudget(ctx context.Context, day, scene, peer string, limit int) (bool, error)
}

// Sender 负责 passive/active 两类出站。
//
// 平台约束（§1.2）：
//   - 群：同一 msg_id 5 分钟内最多 5 次；
//   - 单聊：60 分钟内最多 4 次；
//   - msg_seq 在同一 msg_id 下单调递增，window 过期只重置剩余次数、不回退序号；
//   - msg_id + msg_seq 全局唯一，重试不得复用同一 seq。
type Sender struct {
	api    messageAPI
	log    *slog.Logger
	active ActivePolicy
	trace  *Trace
	usage  activeUsageStore
	// now 可注入：日切、窗口过期、序号保留期都要靠时间推进来测。
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*passiveWindow
	lastGC  time.Time
}

type passiveWindow struct {
	// used 是当前窗口里已经用掉的次数；seq 是这个 msg_id 的累计序号。
	// 两者必须分开：窗口过期只把 used 归零，seq 继续往上走。
	// 合成一个计数器（旧的写法）会让同一 msg_id 的 seq 每 5 分钟从 1 重来，
	// 而平台判重看的是 (msg_id, msg_seq) 全局唯一。
	used    int
	seq     int
	limit   int
	ttl     time.Duration
	expires time.Time
	// lastUsed 是 seqRetention 的依据：窗口过期了也只是"额度归零"，
	// 条目本身要留到 msg_id 不可能再被重试之后才删。
	lastUsed time.Time
}

func NewSender(api messageAPI, log *slog.Logger, active ActivePolicy, usage activeUsageStore) *Sender {
	return &Sender{
		api:     api,
		log:     log,
		active:  active,
		usage:   usage,
		now:     time.Now,
		windows: make(map[string]*passiveWindow),
	}
}

// Reply 发送被动回复。一条 Reply 可能被拆成两条消息（图文），
// 拆分规则见 splitReply。
func (s *Sender) Reply(ctx context.Context, ev *host.Event, msg host.Reply) error {
	parts, err := splitReply(msg)
	if err != nil {
		return err
	}
	for i, part := range parts {
		start := time.Now()
		seq, err := s.sendPassive(ctx, ev, part)
		// 无论成败都留一条轨迹：这是"插件回了什么"的唯一出口。
		s.trace.Sent("被动回复", ev.Scene, ev.PeerID, seq, replySummary(part), err, time.Since(start))
		if err != nil {
			if i == 0 {
				return err
			}
			return fmt.Errorf("%w（前 %d 条已发出）", err, i)
		}
	}
	return nil
}

// splitReply 把一条 Reply 变成平台真正能发的一条或多条消息。
//
// 平台一条消息只有一种 msg_type，图文合并表达不出来，只能拆成两条：
// 先文本、后图片。代价是一次 Reply 消耗两次被动回复配额（群 5 次/5 分钟、
// 单聊 4 次/小时），顺序固定是为了让"说明在上、图在下"符合阅读习惯。
func splitReply(msg host.Reply) ([]host.Reply, error) {
	if !msg.Image.Valid() {
		// 富媒体消息没有 media 字段只会被平台拒绝，在本地拦掉更好排障。
		if msg.MsgType == 7 {
			return nil, fmt.Errorf("msg_type=7 必须带图片，请设置 Reply.Image")
		}
		if msg.Content == "" && msg.MsgType == 0 {
			return nil, fmt.Errorf("回复内容为空")
		}
		return []host.Reply{msg}, nil
	}
	img := host.Reply{MsgType: 7, Image: msg.Image}
	if msg.Content == "" {
		return []host.Reply{img}, nil
	}
	// 文本部分保留插件声明的 msg_type（比如 2 markdown）；7 是图片自己的
	// 类型，带过去会变成"一条没有 media 的富媒体消息"。
	text := host.Reply{Content: msg.Content, MsgType: msg.MsgType}
	if text.MsgType == 7 {
		text.MsgType = 0
	}
	return []host.Reply{text, img}, nil
}

func (s *Sender) sendPassive(ctx context.Context, ev *host.Event, msg host.Reply) (int, error) {
	if ev.MsgID == "" {
		return 0, fmt.Errorf("缺少 msg_id，无法被动回复")
	}

	// 先上传再占位：图片不存在、格式不对、超限都不该吃掉一格被动配额。
	body, err := s.bodyFor(ctx, ev.Scene, ev.PeerID, msg)
	if err != nil {
		return 0, err
	}

	seq, err := s.reserve(ev)
	if err != nil {
		return 0, err
	}
	body.MsgID = ev.MsgID
	body.MsgSeq = seq

	if err := s.api.SendMessage(ctx, string(ev.Scene), ev.PeerID, body); err != nil {
		if httpx.IsStatus(err, 429) {
			return seq, fmt.Errorf("%w (seq=%d): %v", ErrRateLimited, seq, err)
		}
		return seq, fmt.Errorf("发送消息(seq=%d): %w", seq, err)
	}
	return seq, nil
}

// bodyFor 组装请求体；带图时先把图片上传换成 file_info。
func (s *Sender) bodyFor(ctx context.Context, scene host.Scene, peer string, msg host.Reply) (api.MessageBody, error) {
	body := api.MessageBody{Content: msg.Content, MsgType: msg.MsgType}
	if !msg.Image.Valid() {
		return body, nil
	}
	info, err := s.uploadImage(ctx, scene, peer, msg.Image)
	if err != nil {
		return api.MessageBody{}, err
	}
	body.Content = "" // 拆分后图片消息本就没有正文
	body.MsgType = 7
	body.Media = &api.MediaRef{FileInfo: info}
	return body, nil
}

func (s *Sender) reserve(ev *host.Event) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.gcLocked(now)

	w, ok := s.windows[ev.MsgID]
	if !ok {
		if len(s.windows) >= seqCap {
			s.evictOldestLocked()
		}
		limit, ttl := 4, 60*time.Minute
		if ev.Scene == host.SceneGroup {
			limit, ttl = 5, 5*time.Minute
		}
		w = &passiveWindow{limit: limit, ttl: ttl, expires: now.Add(ttl)}
		s.windows[ev.MsgID] = w
	}
	if now.After(w.expires) {
		// 只重置剩余次数。seq 不回退——见 passiveWindow 的注释。
		w.used = 0
		w.expires = now.Add(w.ttl)
	}
	if w.used >= w.limit {
		return 0, ErrPassiveExhausted
	}
	w.used++
	w.seq++
	w.lastUsed = now
	return w.seq, nil
}

// 序号条目的保留期与上限。窗口本身 5 分钟（群）/ 60 分钟（单聊）就过期，
// 但序号必须活得更久：删早了，窗口过期后同一个 msg_id 的 seq 就从 1 重来。
//
// 这两个常量是"内存上界"的全部答案：
//   - seqRetention 覆盖了平台任何合理的重试跨度（远大于 60 分钟窗口）；
//   - seqCap 给条目数一个硬上界（每条 ~80 字节，8192 条约 0.6 MB）。
//
// 代价写清楚：超过上界时淘汰最久未用的 msg_id，它若再出现，seq 会从 1 重来。
// 这个代价是刻意选的——无界增长换来的"绝对单调"会在长期运行里变成 OOM，
// 而 8192 个活跃 msg_id 对当前规模（一天几十到几百条消息）来说够用两个数量级。
const (
	seqRetention = 24 * time.Hour
	seqCap       = 8192
)

// gcLocked 清掉久未使用的窗口。调用方需持锁。
//
// 注意判据是 lastUsed + seqRetention，而不是 expires：
// 过期窗口还要留着序号，删了序号就丢了。
func (s *Sender) gcLocked(now time.Time) {
	if now.Sub(s.lastGC) < time.Minute {
		return
	}
	s.lastGC = now
	for k, w := range s.windows {
		if now.Sub(w.lastUsed) > seqRetention {
			delete(s.windows, k)
		}
	}
}

// evictOldestLocked 在条目数达到 seqCap 时淘汰最久未用的那一条。
// 只在新 msg_id 进来且已满时调用，所以 O(n) 的扫描是可控的（n=8192）。
func (s *Sender) evictOldestLocked() {
	var (
		oldestKey string
		oldestAt  time.Time
		first     bool
	)
	for k, w := range s.windows {
		if first || w.lastUsed.Before(oldestAt) {
			oldestKey, oldestAt, first = k, w.lastUsed, false
		}
	}
	if oldestKey != "" {
		delete(s.windows, oldestKey)
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
	parts, err := splitReply(msg)
	if err != nil {
		return err
	}
	for i, part := range parts {
		start := time.Now()
		err := s.sendActiveOne(ctx, scene, peerID, part)
		s.trace.Sent("主动消息", scene, peerID, 0, replySummary(part), err, time.Since(start))
		if err != nil {
			if i == 0 {
				return err
			}
			return fmt.Errorf("%w（前 %d 条已发出）", err, i)
		}
	}
	return nil
}

func (s *Sender) sendActiveOne(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	if !s.active.Enabled {
		return ErrActiveDisabled
	}
	if peerID == "" {
		return fmt.Errorf("缺少 peer openid")
	}
	// 主动消息的日预算比被动窗口稀缺得多（按天计），所以先扣预算再上传：
	// 额度已尽时不该再浪费一次上传。被动回复反过来——窗口 5 分钟就过期。
	//
	// 扣额度是库里的一次原子 upsert（§4.1 active_usage），因此不存在
	// "内存清零"或"重启后从 0 重来"的问题：进程只是账本的读者之一。
	if ok, err := s.takeActiveBudget(ctx, scene, peerID); err != nil {
		// fail-closed：账本不可信时不发。宁可少发一条，也不能把平台
		// 每天 1000 条的配额当成无限量烧掉——额度超发是不可撤回的。
		return fmt.Errorf("%w（额度记账失败，按保守策略拒发）: %v", ErrActiveQuota, err)
	} else if !ok {
		return fmt.Errorf("%w（%s %s）", ErrActiveQuota, scene, peerID)
	}

	body, err := s.bodyFor(ctx, scene, peerID, msg)
	if err != nil {
		return err
	}
	if err := s.api.SendMessage(ctx, string(scene), peerID, body); err != nil {
		if httpx.IsStatus(err, 429) {
			return fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
		return fmt.Errorf("发送主动消息: %w", err)
	}
	return nil
}

// takeActiveBudget 占一格日预算；ok=false 表示当日已用尽。
// error 表示账本本身不可用（库没打开、写失败、ctx 取消），调用方必须拒发。
func (s *Sender) takeActiveBudget(ctx context.Context, scene host.Scene, peerID string) (bool, error) {
	// 不限量时不必记账：DailyPerPeer<=0 是测试与"临时全开"的用法，
	// 写库只会给一个永不读取的桶添行。注意这与"额度用尽"是两回事。
	if s.active.DailyPerPeer <= 0 {
		return true, nil
	}
	if s.usage == nil {
		// 没有账本就不发：默认值不是"放行"，因为放行的代价是平台配额。
		return false, errors.New("未配置主动消息额度账本")
	}
	day := s.now().Format("2006-01-02")
	return s.usage.TakeActiveBudget(ctx, day, string(scene), peerID, s.active.DailyPerPeer)
}
