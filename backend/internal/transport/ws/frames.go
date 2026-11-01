package ws

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 帧结构与 api/events/frames.schema.json 一致。

type inFrame struct {
	Type   string   `json:"type"`
	Token  string   `json:"token,omitempty"`
	Topics []string `json:"topics,omitempty"`
}

type authOK struct {
	Type      string `json:"type"`
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId"`
}

type authError struct {
	Type    string `json:"type"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rejected struct {
	Topic string `json:"topic"`
	Code  int    `json:"code"`
}

type subscribeOK struct {
	Type     string     `json:"type"`
	Topics   []string   `json:"topics"`
	Rejected []rejected `json:"rejected"`
}

type revokedFrame struct {
	Type       string   `json:"type"`
	OccurredAt string   `json:"occurredAt"`
	Topics     []string `json:"topics"`
	Reason     string   `json:"reason"`
}

func encodeRevoked(topics []string, reason string) ([]byte, error) {
	return json.Marshal(revokedFrame{
		Type:       "subscription.revoked",
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
		Topics:     topics,
		Reason:     reason,
	})
}

// Topic 是一个已解析的订阅目标。
type Topic struct {
	Kind string // project | workspace | generation | user
	ID   int64
	Raw  string
}

var validKinds = map[string]bool{
	"project": true, "workspace": true, "generation": true, "user": true,
}

// ParseTopic 解析 "project:123" 形态。
// 形态非法直接拒绝，不做猜测 —— 猜测会让越权检查有机会被绕过。
func ParseTopic(s string) (Topic, error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return Topic{}, fmt.Errorf("topic 格式非法: %q", s)
	}
	kind, rest := s[:i], s[i+1:]
	if !validKinds[kind] {
		return Topic{}, fmt.Errorf("未知 topic 类型: %q", kind)
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return Topic{}, fmt.Errorf("topic id 非法: %q", rest)
	}
	return Topic{Kind: kind, ID: id, Raw: s}, nil
}
