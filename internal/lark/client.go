package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type Client struct {
	CLI CLI
}

func NewClient(bin string, noProxy ...bool) Client {
	return Client{CLI: NewCLI(bin, noProxy...)}
}

func (client Client) FetchMessageDetail(ctx context.Context, messageID string) (MessageDetail, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return MessageDetail{}, fmt.Errorf("message id is required")
	}

	var response messageDetailResponse
	if err := client.CLI.RunJSON(ctx, &response,
		"api", "GET", "/open-apis/im/v1/messages/"+messageID,
		"--params", `{"user_id_type":"open_id"}`,
		"--as", "bot",
	); err != nil {
		return MessageDetail{}, err
	}

	item, ok := response.Item()
	if !ok {
		return MessageDetail{}, fmt.Errorf("message %s not found in lark response", messageID)
	}
	return MessageDetail{
		MessageID: item.MessageID,
		ThreadID:  item.ThreadID,
		RootID:    item.RootID,
	}, nil
}

func (client Client) SendText(ctx context.Context, userID string, text string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", fmt.Errorf("user id is required")
	}

	var response messageIDResponse
	if err := client.CLI.RunJSON(ctx, &response,
		"im", "+messages-send",
		"--user-id", userID,
		"--text", text,
		"--as", "bot",
	); err != nil {
		return "", err
	}
	id := response.ID()
	if id == "" {
		return "", fmt.Errorf("message_id missing in lark response")
	}
	return id, nil
}

func (client Client) ReplyText(ctx context.Context, messageID string, text string, replyInThread bool) (string, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return "", fmt.Errorf("message id is required")
	}

	args := []string{
		"im", "+messages-reply",
		"--message-id", messageID,
		"--text", text,
	}
	if replyInThread {
		args = append(args, "--reply-in-thread")
	}
	args = append(args, "--as", "bot")

	var response messageIDResponse
	if err := client.CLI.RunJSON(ctx, &response, args...); err != nil {
		return "", err
	}
	id := response.ID()
	if id == "" {
		return "", fmt.Errorf("message_id missing in lark response")
	}
	return id, nil
}

func (client Client) AddReaction(ctx context.Context, messageID string, emoji string) error {
	messageID = strings.TrimSpace(messageID)
	emoji = strings.TrimSpace(emoji)
	if messageID == "" {
		return fmt.Errorf("message id is required")
	}
	if emoji == "" {
		return fmt.Errorf("emoji is required")
	}

	params, err := json.Marshal(map[string]string{"message_id": messageID})
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{
		"reaction_type": map[string]string{"emoji_type": emoji},
	})
	if err != nil {
		return err
	}

	_, err = client.CLI.Run(ctx,
		"im", "reactions", "create",
		"--params", string(params),
		"--data", string(data),
		"--as", "bot",
	)
	return err
}

type messageIDResponse struct {
	MessageID string `json:"message_id"`
	Data      struct {
		MessageID string `json:"message_id"`
		Message   struct {
			MessageID string `json:"message_id"`
		} `json:"message"`
	} `json:"data"`
}

func (response messageIDResponse) ID() string {
	if response.MessageID != "" {
		return response.MessageID
	}
	if response.Data.MessageID != "" {
		return response.Data.MessageID
	}
	return response.Data.Message.MessageID
}

type messageDetailItem struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	RootID    string `json:"root_id"`
}

type messageDetailResponse struct {
	Data struct {
		Item  messageDetailItem   `json:"item"`
		Items []messageDetailItem `json:"items"`
	} `json:"data"`
}

func (response messageDetailResponse) Item() (messageDetailItem, bool) {
	if len(response.Data.Items) > 0 {
		return response.Data.Items[0], true
	}
	if response.Data.Item.MessageID != "" || response.Data.Item.ThreadID != "" || response.Data.Item.RootID != "" {
		return response.Data.Item, true
	}
	return messageDetailItem{}, false
}
