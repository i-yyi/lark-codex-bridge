package lark

import "strings"

type MessageEvent struct {
	EventID     string `json:"event_id"`
	ChatID      string `json:"chat_id"`
	ChatType    string `json:"chat_type"`
	Content     string `json:"content"`
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
	if threadID := strings.TrimSpace(detail.ThreadID); threadID != "" {
		return "thread:" + threadID
	}
	if rootID := strings.TrimSpace(detail.RootID); rootID != "" {
		return "thread:" + rootID
	}
	return "default"
}
