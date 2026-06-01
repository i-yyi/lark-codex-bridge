package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type EventConsumer struct {
	client *larkws.Client
	events chan Event
	errs   chan error
}

func StartEventConsumer(ctx context.Context, appID string, appSecret string) (*EventConsumer, error) {
	consumer := &EventConsumer{
		events: make(chan Event, 64),
		errs:   make(chan error, 4),
	}

	ready := make(chan struct{})
	var readyOnce sync.Once
	dispatch := dispatcher.NewEventDispatcher("", "")
	dispatch.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		message, ok := messageEventFromSDK(event)
		if !ok {
			return nil
		}
		return consumer.enqueue(ctx, Event{Kind: EventKindMessage, Message: message})
	})
	dispatch.OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
		return nil
	})
	dispatch.OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
		return nil
	})
	dispatch.OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
		return nil
	})
	dispatch.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		action := cardActionEventFromSDK(event)
		if action.Action == "" {
			return toast("error", "未知操作"), nil
		}
		if err := consumer.enqueue(ctx, Event{Kind: EventKindCardAction, CardAction: action}); err != nil {
			return toast("error", "服务忙，请稍后重试"), nil
		}
		return toast("success", "收到，正在处理"), nil
	})

	client := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(dispatch),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithOnReady(func() {
			readyOnce.Do(func() { close(ready) })
		}),
		larkws.WithOnError(func(err error) {
			consumer.reportError(fmt.Errorf("lark websocket error: %w", err))
		}),
	)
	consumer.client = client

	go func() {
		if err := client.Start(ctx); err != nil {
			consumer.reportError(fmt.Errorf("lark websocket start: %w", err))
		}
	}()

	select {
	case <-ready:
		return consumer, nil
	case err := <-consumer.errs:
		consumer.Close()
		return nil, err
	case <-ctx.Done():
		consumer.Close()
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		consumer.Close()
		return nil, fmt.Errorf("lark websocket ready timeout")
	}
}

func (consumer *EventConsumer) Receive(ctx context.Context) (Event, error) {
	select {
	case event := <-consumer.events:
		return event, nil
	case err := <-consumer.errs:
		return Event{}, err
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

func (consumer *EventConsumer) Close() {
	if consumer == nil || consumer.client == nil {
		return
	}
	consumer.client.Close()
}

func (consumer *EventConsumer) enqueue(ctx context.Context, event Event) error {
	select {
	case consumer.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (consumer *EventConsumer) reportError(err error) {
	select {
	case consumer.errs <- err:
	default:
	}
}

func messageEventFromSDK(event *larkim.P2MessageReceiveV1) (MessageEvent, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return MessageEvent{}, false
	}
	message := event.Event.Message
	sender := event.Event.Sender
	result := MessageEvent{
		MessageID:   stringValue(message.MessageId),
		RootID:      stringValue(message.RootId),
		ParentID:    stringValue(message.ParentId),
		CreateTime:  stringValue(message.CreateTime),
		ChatID:      stringValue(message.ChatId),
		ThreadID:    stringValue(message.ThreadId),
		ChatType:    stringValue(message.ChatType),
		MessageType: stringValue(message.MessageType),
		Content:     textFromContent(stringValue(message.Content)),
	}
	if event.EventV2Base.Header != nil {
		result.EventID = strings.TrimSpace(event.EventV2Base.Header.EventID)
		result.Timestamp = strings.TrimSpace(event.EventV2Base.Header.CreateTime)
	}
	if sender.SenderId != nil {
		result.SenderID = stringValue(sender.SenderId.OpenId)
	}
	return result, true
}

func cardActionEventFromSDK(event *callback.CardActionTriggerEvent) CardActionEvent {
	result := CardActionEvent{Value: map[string]any{}}
	if event == nil {
		return result
	}
	if event.EventV2Base.Header != nil {
		result.EventID = strings.TrimSpace(event.EventV2Base.Header.EventID)
	}
	if event.Event == nil {
		return result
	}
	if event.Event.Context != nil {
		result.MessageID = strings.TrimSpace(event.Event.Context.OpenMessageID)
		result.ChatID = strings.TrimSpace(event.Event.Context.OpenChatID)
	}
	if event.Event.Operator != nil {
		result.OperatorOpenID = strings.TrimSpace(event.Event.Operator.OpenID)
	}
	if event.Event.Action != nil {
		result.Action = valueString(event.Event.Action.Value, "action")
		if result.Action == "" {
			result.Action = strings.TrimSpace(event.Event.Action.Name)
		}
		result.SessionKey = valueString(event.Event.Action.Value, "session_key")
		result.TurnID = valueString(event.Event.Action.Value, "turn_id")
		result.ItemID = valueString(event.Event.Action.Value, "item_id")
		result.Value = event.Event.Action.Value
	}
	return result
}

func textFromContent(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err == nil && strings.TrimSpace(payload.Text) != "" {
		return strings.TrimSpace(payload.Text)
	}
	return content
}

func valueString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func toast(kind string, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: kind, Content: content},
	}
}
