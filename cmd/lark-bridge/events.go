package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	"lark-bridge/internal/lark"
)

func (app *daemon) handleEvent(ctx context.Context, event lark.MessageEvent) error {
	start := time.Now()
	if event.SenderID != app.cfg.OwnerOpenID {
		return nil
	}
	if event.MessageType != "text" {
		app.debug("event.ignore", "reason", "non_text", "event", event.EventID, "msg", event.MessageID, "type", event.MessageType, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if event.MessageID == "" {
		app.debug("event.ignore", "reason", "missing_msg", "event", event.EventID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if !app.markSeen(event.MessageID) {
		app.debug("event.ignore", "reason", "duplicate", "event", event.EventID, "msg", event.MessageID, "total", elapsed(start))
		return nil
	}

	text := strings.TrimSpace(event.Content)
	if strings.TrimSpace(text) == "" {
		app.debug("event.ignore", "reason", "empty_text", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}

	app.debug("event.accept", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "chat_type", event.ChatType, "text_len", len(text), "create_time", event.CreateTime, "event_ts", event.Timestamp, "bus_lag", eventLag(event.CreateTime, event.Timestamp), "recv_lag", recvLag(event.CreateTime, start))
	classifyStart := time.Now()
	message := app.classifyMessage(event, text)
	app.debug("event.classify", "msg", event.MessageID, "inbound", message.kind, "route_kind", message.route.kind, "key", message.route.sessionKey, "reason", message.route.reason, "command", message.command.Name, "dur", elapsed(classifyStart))

	dispatchStart := time.Now()
	err := app.dispatchInbound(ctx, message)
	status := "ok"
	if err != nil {
		status = "error"
	}
	app.debug("event.done", "msg", event.MessageID, "inbound", message.kind, "route_kind", message.route.kind, "key", message.route.sessionKey, "command", message.command.Name, "status", status, "dispatch", elapsed(dispatchStart), "total", elapsed(start), "err", err)
	return err
}

func (app *daemon) markSeen(messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false
	}
	if _, ok := app.seen[messageID]; ok {
		return false
	}
	app.seen[messageID] = struct{}{}
	app.seenOrder = append(app.seenOrder, messageID)
	if len(app.seenOrder) > maxSeenMessages {
		oldest := app.seenOrder[0]
		delete(app.seen, oldest)
		copy(app.seenOrder, app.seenOrder[1:])
		app.seenOrder = app.seenOrder[:len(app.seenOrder)-1]
	}
	return true
}

func (app *daemon) classifyMessage(event lark.MessageEvent, text string) inboundMessage {
	route := app.eventRoute(event)
	command := bridge.ParseCommand(text)
	kind := inboundPrompt
	if command.IsCommand {
		kind = inboundCommand
	}
	return inboundMessage{
		kind:    kind,
		event:   event,
		text:    text,
		route:   route,
		command: command,
	}
}

func (app *daemon) dispatchInbound(ctx context.Context, message inboundMessage) error {
	switch message.kind {
	case inboundCommand:
		return app.receiveCommand(ctx, message)
	case inboundPrompt:
		return app.receivePrompt(ctx, message)
	default:
		return fmt.Errorf("unknown inbound kind: %s", message.kind)
	}
}

func (app *daemon) receiveCommand(ctx context.Context, message inboundMessage) error {
	app.log("command.recv", "name", message.command.Name, "arg_len", len(message.command.Arg), "msg", message.event.MessageID, "kind", message.route.kind, "key", message.route.sessionKey)
	go app.react(ctx, message.event.MessageID, reactionCommand)
	return app.handleCommand(ctx, message)
}

func (app *daemon) receivePrompt(ctx context.Context, message inboundMessage) error {
	return app.handlePrompt(ctx, message)
}

func (app *daemon) eventRoute(event lark.MessageEvent) messageRoute {
	detail := lark.DetailFromEvent(event)
	sessionKey, reason := app.taskSessionKey(detail)
	kind := kindTask
	if reason == "no_thread_or_root" {
		sessionKey = chatSessionKey
		kind = kindChat
	}
	if kind == kindTask && !app.knownSession(sessionKey) {
		app.log("route.unknown", "msg", event.MessageID, "thread", detail.ThreadID, "root", detail.RootID, "candidate", sessionKey, "reason", reason, "fallback", chatSessionKey)
		return messageRoute{sessionKey: chatSessionKey, kind: kindChat, reason: "unknown_thread", detail: detail}
	}

	route := messageRoute{sessionKey: sessionKey, kind: kind, reason: reason, detail: detail}
	app.debug("route.selected", "msg", event.MessageID, "thread", detail.ThreadID, "root", detail.RootID, "kind", route.kind, "key", route.sessionKey, "reason", route.reason)
	return route
}

func (app *daemon) handleCardAction(ctx context.Context, action lark.CardActionEvent) error {
	if action.OperatorOpenID != app.cfg.OwnerOpenID {
		app.log("card.ignore", "reason", "owner_mismatch", "event", action.EventID, "operator", action.OperatorOpenID, "msg", action.MessageID, "action", action.Action)
		return nil
	}
	route := messageRoute{sessionKey: strings.TrimSpace(action.SessionKey), kind: kindTask, reason: "card_action"}
	if route.sessionKey == "" {
		app.log("card.ignore", "reason", "missing_session", "event", action.EventID, "msg", action.MessageID, "action", action.Action)
		return nil
	}
	app.log("card.action", "event", action.EventID, "msg", action.MessageID, "key", route.sessionKey, "action", action.Action, "turn", action.TurnID, "item", action.ItemID)
	switch action.Action {
	case codex.DecisionApprove:
		return app.resolvePending(ctx, action.MessageID, route, codex.DecisionApprove)
	case codex.DecisionDeny:
		return app.resolvePending(ctx, action.MessageID, route, codex.DecisionDeny)
	case codex.DecisionCancel:
		return app.handleCancel(ctx, action.MessageID, route)
	default:
		return app.respond(ctx, action.MessageID, route, "未知卡片操作："+action.Action)
	}
}
