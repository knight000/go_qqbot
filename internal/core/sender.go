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
	"github.com/knight000/go_qqbot/internal/store"
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
	TakeActiveBudget(ctx context.Context, key store.ActiveUsageKey, limit int) (bool, error)
	// RefundActiveBudget 归还一格：只在平台明确拒绝（err_code 非 0）时调用。
	RefundActiveBudget(ctx context.Context, key store.ActiveUsageKey) error
}

// Sender 负责 passive/active 两类出站。
//
// 平台约束（§1.2）：
//   - 群：同一 msg_id 5 分钟内最多 5 次；
//   - 单聊：60 分钟内最多 4 次；
//   - msg_seq 在同一 msg_id 下单调递增，窗口过期只重置剩余次数、不回退序号；
//   - msg_id + msg_seq 全局唯一，重试不得复用同一 seq。
type Sender struct {
	api    messageAPI
	log    *slog.Logger
	active ActivePolicy
	trace  *Trace
	usage  activeUsageStore
	// loc 是"一天"的口径。它必须与 cron 用同一个 location（由 Scheduler
	// 注入），不能读环境变量、更不能跟进程本地时区走：部署单元默认 UTC，
	// 而平台配额按北京时间自然日重置，两者分叉时同一个平台自然日能花掉
	// "前一天桶里的余额 + 08:00 后的新满额"，最多 2×limit。
	loc *time.Location
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
}

// NewSender 的 loc 是日预算的切日时区，由 Scheduler 提供（与 cron 同源）。
// 不设默认值：一个"忘了注入就用本地时区"的兜底正是 P2-4 那个 bug 本身。
func NewSender(api messageAPI, log *slog.Logger, active ActivePolicy, usage activeUsageStore, loc *time.Location) *Sender {
	return &Sender{
		api:     api,
		log:     log,
		active:  active,
		usage:   usage,
		loc:     loc,
		now:     time.Now,
		windows: make(map[string]*passiveWindow),
	}
}

// 回复的发送预算。纯文本 5s 够用；含上传时必须给 30s ——api 层的 30s
// 只放宽了 http.Client.Timeout，放不宽 ctx，而 5 MB 图 base64 后约 6.7 MB，
// 10 Mbps 上行就要 5s 以上，小预算会让图永远发不出去。
const (
	replyTextTimeout   = 5 * time.Second
	replyUploadTimeout = 30 * time.Second
)

// replyBudget 按这一条消息的内容决定预算。
//
// 判断与降级都必须在核心内完成：host.Sender.Reply 不返回错误，插件拿不到
// "图没发出去"这件事，只能由这里给定时间并在失败时留下 Warn。
func replyBudget(msg host.Reply) time.Duration {
	if msg.Image.Valid() {
		return replyUploadTimeout
	}
	return replyTextTimeout
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
		// 预算按条算：图文拆成文本 + 图片两条，各拿各的，上传慢不连累文本。
		pctx, cancel := context.WithTimeout(ctx, replyBudget(part))
		seq, err := s.sendPassive(pctx, ev, part)
		cancel()
		// 无论成败都留一条轨迹：这是"插件回了什么"的唯一出口。
		s.trace.Sent("被动回复", ev.Scene, ev.PeerID, seq, replySummary(part), err, time.Since(start))
		if err != nil {
			// 拆成多条时前几条可能已经发出去了。只把错误往上返回不够：
			// dispatch 的日志没有 peer 也没有"发到第几条"，排障时对不上。
			s.log.Warn("被动回复未完整发出",
				"scene", ev.Scene, "peer", ev.PeerID, "msg_id", ev.MsgID,
				"sent", i, "total", len(parts), "err", err)
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
		// 平台明确拒绝（200 + err_code）说明这条没发出去，把占掉的那一格还回去：
		// 否则连着几条被风控挡掉，正常回复也会发不出去。
		s.releasePassiveOnBusinessError(ev.MsgID, err)
		if httpx.IsStatus(err, 429) {
			return seq, fmt.Errorf("%w (seq=%d): %v", ErrRateLimited, seq, err)
		}
		return seq, fmt.Errorf("发送消息(seq=%d): %w", seq, err)
	}
	return seq, nil
}

// releasePassiveOnBusinessError 把一格被动配额还回去。
//
// 只认业务错误：网络失败/超时时请求可能已经到达平台，还回去等于允许实际发送
// 超过窗口上限（群 5 次/5 分钟、单聊 4 次/小时都是平台硬约束）。
//
// seq 不回退：被拒的那次可能已经占用平台的 (msg_id, msg_seq) 判重记录，
// 回退会让下一条撞上同一个序号。
func (s *Sender) releasePassiveOnBusinessError(msgID string, cause error) {
	var be *api.BusinessError
	if !errors.As(cause, &be) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.windows[msgID]; ok && w.used > 0 {
		w.used--
	}
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
			s.evictExpiredLocked(now)
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
	return w.seq, nil
}

// 序号条目的保留期与上限。窗口本身 5 分钟（群）/ 60 分钟（单聊）就过期，
// 但序号必须活得更久：删早了，窗口过期后同一个 msg_id 的 seq 就从 1 重来。
//
// seqRetention 是窗口过期后再留多久，覆盖平台任何合理的重试跨度
// （远大于 60 分钟的窗口上限）。
//
// seqCap 是**软上界**：条目数达到它时只清理已过期的窗口（见
// evictExpiredLocked）。若一条过期的都没有，就允许暂时超出——超出的部分
// 只可能来自"仍在窗口有效期内的活跃 msg_id"，群 5 分钟、单聊 60 分钟，
// 本规模（一天几十到几百条消息）下离 8192 还有几个数量级；每条 ~64 字节，
// 多占这一点内存远比淘汰一个仍在用的窗口便宜——后者会把 used 清零（同一
// msg_id 可以超发）、把 seq 打回 1（(msg_id, msg_seq) 重复）。
const (
	seqRetention = 24 * time.Hour
	seqCap       = 8192
)

// gcLocked 清掉久未使用的窗口。调用方需持锁。
//
// 判据是 expires + seqRetention 而不是 expires：过期窗口还要留着序号，
// 按 expires 删等于把 seq 一起删掉，下一个同 msg_id 的回复又从 1 开始。
func (s *Sender) gcLocked(now time.Time) {
	if now.Sub(s.lastGC) < time.Minute {
		return
	}
	s.lastGC = now
	for k, w := range s.windows {
		if now.After(w.expires.Add(seqRetention)) {
			delete(s.windows, k)
		}
	}
}

// evictExpiredLocked 在条目数达到 seqCap 时清掉一条**窗口已过期**的条目。
//
// 判据是"窗口过期"本身（now.After(expires)）。gcLocked 用的是更严的条件
// （expires+seqRetention），那是为了连序号一起回收——拿它当淘汰判据就一条
// 都删不掉，到了 seqCap 反而只能踢活跃窗口。不能只看 lastOrder：
// 最久未用的那条往往仍在 5 分钟/60 分钟的被动窗口内，淘汰它等于把 used 清零
// 并把 seq 打回 1，同一 msg_id 随后就会重复占用 (msg_id, msg_seq)。
// 一条过期的都没有时什么也不删——seqCap 是软上界，见常量注释。
//
// 候选里挑 expires 最小的那条：过期最久的窗口最不可能再被重试，若它连
// seqRetention 也过了，删除它连"序号重来"的代价都不存在。
//
// 只在新 msg_id 进来且已满时调用，所以 O(n) 的扫描是可控的（n≈8192）。
func (s *Sender) evictExpiredLocked(now time.Time) {
	var (
		victimKey string
		victimExp time.Time
		found     bool
	)
	for k, w := range s.windows {
		if !now.After(w.expires) {
			continue // 仍在被动窗口内：绝不淘汰
		}
		if !found || w.expires.Before(victimExp) {
			victimKey, victimExp, found = k, w.expires, true
		}
	}
	if found {
		delete(s.windows, victimKey)
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
		// 带上场景与 peer：日志里只有"记账失败"时定位不到是哪个会话
		// 把账本写坏了（规则 7：错误必须指得回它发生在谁身上）。
		return fmt.Errorf("%w（额度记账失败，按保守策略拒发；%s %s）: %v", ErrActiveQuota, scene, peerID, err)
	} else if !ok {
		return fmt.Errorf("%w（%s %s）", ErrActiveQuota, scene, peerID)
	}

	body, err := s.bodyFor(ctx, scene, peerID, msg)
	if err != nil {
		// 上传被平台拒绝时消息还没发出去，那一格预算不该记在账上。
		s.refundActiveBudgetOnBusinessError(ctx, scene, peerID, err)
		return err
	}
	if err := s.api.SendMessage(ctx, string(scene), peerID, body); err != nil {
		s.refundActiveBudgetOnBusinessError(ctx, scene, peerID, err)
		if httpx.IsStatus(err, 429) {
			return fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
		return fmt.Errorf("发送主动消息: %w", err)
	}
	return nil
}

// refundActiveBudgetOnBusinessError 在平台明确拒绝时归还一格主动消息日预算。
//
// 只认业务错误：网络失败/超时时消息可能已经发出，归还等于超发。这是既定
// 取舍——宁可少发一条，也不能把平台每天 1000 条的配额当成无限量烧掉。
//
// 归还本身失败只告警：额度账本坏了是运维问题，不该让一次发送错误再叠一层
// 无从处理的错误；但必须留日志，否则"账多了/少了"永远查不到原因。
func (s *Sender) refundActiveBudgetOnBusinessError(ctx context.Context, scene host.Scene, peerID string, cause error) {
	var be *api.BusinessError
	if !errors.As(cause, &be) {
		return
	}
	if s.active.DailyPerPeer <= 0 || s.usage == nil {
		return
	}
	key := store.ActiveUsageKey{
		Day:   s.now().Format("2006-01-02"),
		Scene: string(scene),
		Peer:  peerID,
	}
	if err := s.usage.RefundActiveBudget(ctx, key); err != nil {
		s.log.Warn("归还主动消息额度失败", "scene", scene, "peer", peerID, "err", err)
	}
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
	key := store.ActiveUsageKey{
		// 与 cron 同一个时区：平台配额按这个自然日重置，换桶也必须按它。
		Day:   s.now().In(s.loc).Format("2006-01-02"),
		Scene: string(scene),
		Peer:  peerID,
	}
	return s.usage.TakeActiveBudget(ctx, key, s.active.DailyPerPeer)
}
