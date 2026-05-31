package lark

import "strings"

type MessageEvent struct {
	EventID     string `json:"event_id"`
	ChatID      string `json:"chat_id"`
	ChatType    string `json:"chat_type"`
	Content     string `json:"content"`
	CreateTime  string `json:"create_time"`
	Timestamp   string `json:"timestamp"`
	MessageID   string `json:"message_id"`
	MessageType string `json:"message_type"`
	SenderID    string `json:"sender_id"`
}

type MessageDetail struct {
	MessageID string
	ThreadID  string
	RootID    string
}

func SessionKey(detail MessageDetail) string {
	key, _ := SessionKeyReason(detail)
	return key
}

func SessionKeyReason(detail MessageDetail) (string, string) {
	if threadID := strings.TrimSpace(detail.ThreadID); threadID != "" {
		return "thread:" + threadID, "thread_id"
	}
	if rootID := strings.TrimSpace(detail.RootID); rootID != "" {
		return "thread:" + rootID, "root_id"
	}
	return "default", "no_thread_or_root"
}
