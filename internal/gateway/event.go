package gateway

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/knight000/qqbot/internal/host"
)

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	ID string          `json:"id"`
}

type author struct {
	ID           string `json:"id"`
	MemberOpenID string `json:"member_openid"`
	UserOpenID   string `json:"user_openid"`
	UnionOpenID  string `json:"union_openid"`
}

type messageData struct {
	ID          string            `json:"id"`
	Content     string            `json:"content"`
	GroupOpenID string            `json:"group_openid"`
	Author      author            `json:"author"`
	Timestamp   string            `json:"timestamp"`
	Attachments []json.RawMessage `json:"attachments"`
}

type groupOpData struct {
	GroupOpenID    string          `json:"group_openid"`
	OpMemberOpenID string          `json:"op_member_openid"`
	Timestamp      json.RawMessage `json:"timestamp"`
}

// Decode 把平台事件转成归一化 Event。返回 false 表示未订阅/不支持。
// 只处理群消息、单聊消息、机器人被添加/移除三类；其余事件留给后续扩展。
func Decode(t string, raw json.RawMessage) (*host.Event, bool) {
	switch t {
	case host.EventGroupMessage:
		var d messageData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false
		}
		return &host.Event{
			ID:        d.ID,
			Type:      t,
			Scene:     host.SceneGroup,
			PeerID:    d.GroupOpenID,
			SenderID:  firstNonEmpty(d.Author.MemberOpenID, d.Author.ID),
			UnionID:   d.Author.UnionOpenID,
			MsgID:     d.ID,
			Content:   strings.TrimSpace(d.Content),
			HasMedia:  len(d.Attachments) > 0,
			Timestamp: parseTime(d.Timestamp),
		}, true

	case host.EventC2CMessage:
		var d messageData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false
		}
		return &host.Event{
			ID:        d.ID,
			Type:      t,
			Scene:     host.SceneC2C,
			PeerID:    d.Author.UserOpenID,
			SenderID:  firstNonEmpty(d.Author.UserOpenID, d.Author.ID),
			UnionID:   d.Author.UnionOpenID,
			MsgID:     d.ID,
			Content:   strings.TrimSpace(d.Content),
			HasMedia:  len(d.Attachments) > 0,
			Timestamp: parseTime(d.Timestamp),
		}, true

	case host.EventGroupAdd, host.EventGroupDel:
		var d groupOpData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false
		}
		return &host.Event{
			ID:        stableGroupOpID(t, d),
			Type:      t,
			Scene:     host.SceneGroup,
			PeerID:    d.GroupOpenID,
			SenderID:  d.OpMemberOpenID,
			Timestamp: time.Now(),
		}, true
	}
	return nil, false
}

func stableGroupOpID(t string, d groupOpData) string {
	ts := strings.Trim(string(d.Timestamp), `"`)
	if ts == "" {
		ts = "0"
	}
	return t + ":" + d.GroupOpenID + ":" + d.OpMemberOpenID + ":" + ts
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
