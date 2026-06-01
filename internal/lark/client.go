package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type Client struct {
	sdk *larksdk.Client
}

func NewClient(appID string, appSecret string) Client {
	return Client{sdk: larksdk.NewClient(appID, appSecret, larksdk.WithLogLevel(larkcore.LogLevelError))}
}

func (client Client) BotName(ctx context.Context) (string, error) {
	resp, err := client.sdk.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", err
	}
	if resp == nil || resp.StatusCode != 200 {
		return "", fmt.Errorf("lark bot info failed: status=%d", statusCode(resp))
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return "", fmt.Errorf("parse bot info: %w", err)
	}
	if result.Code != 0 {
		return "", larkAPIError("bot info", resp, result.Code, result.Msg)
	}
	name := strings.TrimSpace(result.Bot.AppName)
	if name == "" {
		return "", fmt.Errorf("bot app_name is empty")
	}
	return name, nil
}

func (client Client) FetchMessageDetail(ctx context.Context, messageID string) (MessageDetail, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return MessageDetail{}, fmt.Errorf("message id is required")
	}

	resp, err := client.sdk.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().
		MessageId(messageID).
		UserIdType("open_id").
		Build())
	if err != nil {
		return MessageDetail{}, err
	}
	if !resp.Success() {
		return MessageDetail{}, larkAPIError("get message", resp.ApiResp, resp.Code, resp.Msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 {
		return MessageDetail{}, fmt.Errorf("message %s not found in lark response", messageID)
	}
	item := resp.Data.Items[0]
	return MessageDetail{
		ThreadID: stringValue(item.ThreadId),
		RootID:   stringValue(item.RootId),
	}, nil
}

func (client Client) SendText(ctx context.Context, userID string, text string) (string, error) {
	return client.send(ctx, "open_id", userID, "text", mustTextContent(text))
}

func (client Client) SendCard(ctx context.Context, userID string, card string) (string, error) {
	return client.send(ctx, "open_id", userID, "interactive", card)
}

func (client Client) ReplyText(ctx context.Context, messageID string, text string, replyInThread bool) (string, error) {
	return client.reply(ctx, messageID, "text", mustTextContent(text), replyInThread)
}

func (client Client) ReplyCard(ctx context.Context, messageID string, card string, replyInThread bool) (string, error) {
	return client.reply(ctx, messageID, "interactive", card, replyInThread)
}

func (client Client) PatchCard(ctx context.Context, messageID string, card string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return fmt.Errorf("message id is required")
	}
	resp, err := client.sdk.Im.V1.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(card).Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return larkAPIError("patch card", resp.ApiResp, resp.Code, resp.Msg)
	}
	return nil
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

	resp, err := client.sdk.Im.V1.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return larkAPIError("add reaction", resp.ApiResp, resp.Code, resp.Msg)
	}
	return nil
}

func (client Client) send(ctx context.Context, receiveIDType string, receiveID string, msgType string, content string) (string, error) {
	receiveID = strings.TrimSpace(receiveID)
	if receiveID == "" {
		return "", fmt.Errorf("receive id is required")
	}
	resp, err := client.sdk.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", larkAPIError("send", resp.ApiResp, resp.Code, resp.Msg)
	}
	if resp.Data == nil || stringValue(resp.Data.MessageId) == "" {
		return "", fmt.Errorf("message_id missing in lark response")
	}
	return stringValue(resp.Data.MessageId), nil
}

func (client Client) reply(ctx context.Context, messageID string, msgType string, content string, replyInThread bool) (string, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return "", fmt.Errorf("message id is required")
	}

	resp, err := client.sdk.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			Content(content).
			MsgType(msgType).
			ReplyInThread(replyInThread).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", larkAPIError("reply", resp.ApiResp, resp.Code, resp.Msg)
	}
	if resp.Data == nil || stringValue(resp.Data.MessageId) == "" {
		return "", fmt.Errorf("message_id missing in lark response")
	}
	return stringValue(resp.Data.MessageId), nil
}

func mustTextContent(text string) string {
	data, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return `{"text":""}`
	}
	return string(data)
}

type APIError struct {
	Action     string
	Code       int
	Msg        string
	Scopes     []string
	ConsoleURL string
	RequestID  string
}

func (err *APIError) Error() string {
	if err == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("lark %s failed: code=%d msg=%s", err.Action, err.Code, err.Msg)}
	if len(err.Scopes) > 0 {
		parts = append(parts, "missing_scopes="+strings.Join(err.Scopes, ","))
	}
	if err.ConsoleURL != "" {
		parts = append(parts, "console_url="+err.ConsoleURL)
	}
	if err.RequestID != "" {
		parts = append(parts, "request_id="+err.RequestID)
	}
	return strings.Join(parts, " ")
}

func larkAPIError(action string, resp *larkcore.ApiResp, code int, msg string) error {
	apiErr := &APIError{
		Action: strings.TrimSpace(action),
		Code:   code,
		Msg:    strings.TrimSpace(msg),
	}
	if resp != nil {
		apiErr.RequestID = firstHeader(resp.Header, "X-Tt-Logid", "X-Request-Id", "X-Lark-Request-Id")
		apiErr.Scopes, apiErr.ConsoleURL = larkDiagnostics(resp.RawBody)
	}
	return apiErr
}

func larkDiagnostics(raw []byte) ([]string, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, ""
	}
	scopes := map[string]struct{}{}
	consoleURL := ""
	collectDiagnostics(payload, scopes, &consoleURL)
	scopeList := make([]string, 0, len(scopes))
	for scope := range scopes {
		scopeList = append(scopeList, scope)
	}
	sort.Strings(scopeList)
	return scopeList, consoleURL
}

func collectDiagnostics(value any, scopes map[string]struct{}, consoleURL *string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			switch {
			case lowerKey == "console_url":
				if *consoleURL == "" {
					*consoleURL = strings.TrimSpace(fmt.Sprint(child))
				}
			case lowerKey == "permission_violations":
				collectScopeValues(child, scopes)
			case strings.Contains(lowerKey, "scope"):
				collectScopeValues(child, scopes)
			}
			collectDiagnostics(child, scopes, consoleURL)
		}
	case []any:
		for _, child := range typed {
			collectDiagnostics(child, scopes, consoleURL)
		}
	}
}

func collectScopeValues(value any, scopes map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		addScope(typed, scopes)
	case []any:
		for _, child := range typed {
			collectScopeValues(child, scopes)
		}
	case map[string]any:
		for key, child := range typed {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if lowerKey == "scope" || lowerKey == "subject" || lowerKey == "permission" || strings.Contains(lowerKey, "scope") {
				collectScopeValues(child, scopes)
				continue
			}
			if key == "" {
				collectScopeValues(child, scopes)
			}
		}
	}
}

func addScope(value string, scopes map[string]struct{}) {
	for _, field := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}) {
		field = strings.Trim(field, ` "'[]`)
		if field == "" || !strings.Contains(field, ":") {
			continue
		}
		scopes[field] = struct{}{}
	}
}

func firstHeader(header map[string][]string, names ...string) string {
	for _, name := range names {
		values := header[name]
		if len(values) == 0 {
			continue
		}
		if value := strings.TrimSpace(values[0]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func statusCode(resp *larkcore.ApiResp) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
