package main

import (
	"context"
	"fmt"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
)

func (app *daemon) handleCommand(ctx context.Context, message inboundMessage) error {
	event := message.event
	route := message.route
	command := message.command
	switch command.Name {
	case bridge.CommandHelp:
		return app.respond(ctx, event.MessageID, route, "支持：/create [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast] [任务], /attach THREAD_ID [--project NAME | --cwd DIR] [--model MODEL] [--effort ...] [--tier fast], /history [--limit N], /import ID_OR_INDEX [--cwd DIR], /projects, /sessions, /status, /reset, /approve, /deny, /cancel")
	case bridge.CommandCreate:
		return app.handleCreateTask(ctx, event, route, command.Arg)
	case bridge.CommandAttach:
		return app.handleAttach(ctx, event, route, command.Arg)
	case bridge.CommandHistory:
		return app.handleHistory(ctx, event.MessageID, route, command.Arg)
	case bridge.CommandImport:
		return app.handleImport(ctx, event, route, command.Arg)
	case bridge.CommandProjects:
		return app.respond(ctx, event.MessageID, route, app.projectsText())
	case bridge.CommandSessions:
		return app.respond(ctx, event.MessageID, route, app.sessionsText(route))
	case bridge.CommandStatus:
		return app.respond(ctx, event.MessageID, route, app.statusText(route))
	case bridge.CommandReset:
		return app.handleReset(ctx, event.MessageID, route)
	case bridge.CommandApprove:
		return app.resolvePending(ctx, event.MessageID, route, codex.DecisionApprove)
	case bridge.CommandDeny:
		return app.resolvePending(ctx, event.MessageID, route, codex.DecisionDeny)
	case bridge.CommandCancel:
		return app.handleCancel(ctx, event.MessageID, route)
	case bridge.CommandUnknown:
		return app.respond(ctx, event.MessageID, route, "未知命令。当前支持：/create, /attach, /history, /import, /projects, /sessions, /status, /reset, /approve, /deny, /cancel")
	default:
		return app.respond(ctx, event.MessageID, route, fmt.Sprintf("/%s 还没实现", command.Name))
	}
}
