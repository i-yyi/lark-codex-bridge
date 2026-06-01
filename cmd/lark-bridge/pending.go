package main

import (
	"context"
	"errors"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
)

func (app *daemon) resolvePending(ctx context.Context, messageID string, route messageRoute, decision string) error {
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	if runtime.status != statusPending || runtime.pending == nil || runtime.client == nil {
		app.mu.Unlock()
		app.log("pending.skip", "msg", messageID, "key", route.sessionKey, "decision", decision, "reason", "no_pending")
		return app.respond(ctx, messageID, route, "当前会话没有等待确认的 Codex 操作。")
	}

	pending := *runtime.pending
	client := runtime.client
	workDir := runtime.workDir
	taskCtx, cancel := context.WithCancel(ctx)
	runtime.status = statusRunning
	runtime.pending = nil
	runtime.cancel = cancel
	app.log("pending.resolve", "msg", messageID, "key", route.sessionKey, "decision", decision, "method", pending.Method, "turn", pending.TurnID)
	app.mu.Unlock()

	statusMessageID := cardStatusMessageID(route, messageID)
	app.markActiveTurn(route.sessionKey, client, pending.ThreadID, pending.TurnID)
	if statusMessageID != "" {
		card := app.taskCard("Codex 继续运行", "running", bridge.PickPhrase(messageID+":pending_resume", bridge.PendingResume), route, workDir, app.runningActions(route))
		if err := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); err != nil {
			app.logError("pending.running.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
		}
	} else {
		app.reactForRoute(ctx, route, messageID, reactionProcessing)
	}
	go app.respondPendingTask(taskCtx, route, messageID, statusMessageID, workDir, client, pending, decision)
	return nil
}

func (app *daemon) respondPendingTask(ctx context.Context, route messageRoute, messageID string, statusMessageID string, workDir string, client *codex.Client, pending codex.PendingRequest, decision string) {
	start := time.Now()
	onUpdate, stopUpdates := app.liveCardUpdater(ctx, route, messageID, statusMessageID, workDir)
	result, err := client.RespondToPending(ctx, pending, decision, onUpdate)
	stopUpdates()
	if err != nil {
		var pendingErr *codex.PendingRequestError
		if errors.As(err, &pendingErr) {
			app.debug("pending.next", "msg", messageID, "key", route.sessionKey, "method", pendingErr.Pending.Method, "dur", elapsed(start))
			app.pauseForPending(context.Background(), route, messageID, statusMessageID, client, pendingErr.Pending)
			return
		}
		app.logError("pending.respond.fail", "msg", messageID, "key", route.sessionKey, "decision", decision, "dur", elapsed(start), "err", err)
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 继续运行失败", err, statusMessageID)
		return
	}
	app.debug("pending.respond.ok", "msg", messageID, "key", route.sessionKey, "decision", decision, "thread", result.ThreadID, "turn", result.TurnID, "dur", elapsed(start))
	app.finishTaskSuccess(context.Background(), route, messageID, client, result, statusMessageID)
}

func cardStatusMessageID(route messageRoute, messageID string) string {
	if route.reason == "card_action" {
		return messageID
	}
	return ""
}

func (app *daemon) handleReset(ctx context.Context, messageID string, route messageRoute) error {
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	switch runtime.status {
	case statusRunning:
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "当前会话正在运行，先 /cancel 再 /reset。")
	case statusPending:
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "当前会话正在等待确认，先 /approve、/deny 或 /cancel 再 /reset。")
	}

	oldThreadID := runtime.threadID
	runtime.threadID = ""
	runtime.client = nil
	runtime.cancel = nil
	runtime.pending = nil
	app.store.SetSessionThread(route.sessionKey, "")
	err := app.store.Save(app.cfg.StatePath)
	app.mu.Unlock()
	if err != nil {
		app.logError("session.reset.fail", "msg", messageID, "key", route.sessionKey, "old_thread", oldThreadID, "err", err)
		return err
	}

	app.log("session.reset.ok", "msg", messageID, "key", route.sessionKey, "old_thread", oldThreadID)
	return app.respond(ctx, messageID, route, "当前会话已 reset，下一条消息会创建新的 Codex session。")
}

func (app *daemon) handleCancel(ctx context.Context, messageID string, route messageRoute) error {
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	switch runtime.status {
	case statusPending:
		app.mu.Unlock()
		app.log("session.cancel", "state", "pending", "msg", messageID, "key", route.sessionKey)
		return app.resolvePending(ctx, messageID, route, codex.DecisionCancel)
	case statusRunning:
		cancel := runtime.cancel
		app.mu.Unlock()
		app.log("session.cancel", "state", "running", "msg", messageID, "key", route.sessionKey)
		if cancel != nil {
			cancel()
		}
		if route.reason == "card_action" {
			return nil
		}
		return app.respond(ctx, messageID, route, "已请求取消当前 Codex 任务。")
	default:
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "当前会话没有运行中的 Codex 任务。")
	}
}

func (app *daemon) pauseForPending(ctx context.Context, route messageRoute, messageID string, statusMessageID string, client *codex.Client, pending codex.PendingRequest) {
	pendingCopy := pending
	var cancel context.CancelFunc
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	cancel = runtime.cancel
	runtime.status = statusPending
	runtime.client = client
	runtime.cancel = nil
	runtime.pending = &pendingCopy
	runtime.activeTurnID = pending.TurnID
	app.log("runtime.status", "key", route.sessionKey, "from", statusRunning, "to", statusPending, "pending", pending.Method, "turn", pending.TurnID, "item", pending.ItemID)
	app.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if route.kind == kindTask {
		body := bridge.PickPhrase(messageID+":pending_intro", bridge.PendingIntro) + "\n\n" + pending.Summary
		card := app.taskCard("Codex 等待确认", "pending", body, route, "", app.pendingActions(route, pending))
		if err := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); err != nil {
			app.logError("pending.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
		}
		return
	}
	_ = app.respond(ctx, messageID, route, bridge.PickPhrase(messageID+":pending_intro", bridge.PendingIntro)+"\n\n"+pending.Summary+"\n\n回复 /approve 允许，/deny 拒绝，或 /cancel 取消。")
}

func (app *daemon) finishTaskSuccess(ctx context.Context, route messageRoute, messageID string, client *codex.Client, result codex.TurnResult, statusMessageID string) {
	app.log("codex.task.ok", "msg", messageID, "key", route.sessionKey, "thread", result.ThreadID, "turn", result.TurnID, "text_len", len(result.Text))
	app.clearRuntimeClient(route.sessionKey, client)
	_ = client.Close()
	if err := app.replyTurnResult(ctx, messageID, route, result, statusMessageID); err != nil {
		app.logError("result.reply.fail", "msg", messageID, "key", route.sessionKey, "err", err)
	}
}

func (app *daemon) finishTaskError(ctx context.Context, route messageRoute, messageID string, client *codex.Client, prefix string, err error, statusMessageID string) {
	app.logError("codex.task.fail", "msg", messageID, "key", route.sessionKey, "prefix", prefix, "err", err)
	app.clearRuntimeClient(route.sessionKey, client)
	if client != nil {
		_ = client.Close()
	}

	isCanceled := errors.Is(err, context.Canceled)
	if !isCanceled {
		app.reactForRoute(ctx, route, messageID, reactionError)
	}
	message := prefix + "：" + err.Error()
	title := "Codex 运行失败"
	status := "error"
	if isCanceled {
		message = "Codex 任务已取消。"
		title = "Codex 已取消"
		status = "warning"
	}
	if route.kind == kindTask {
		card := app.taskCard(title, status, message, route, "", nil)
		if replyErr := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); replyErr != nil {
			app.logError("error.card.fail", "msg", messageID, "key", route.sessionKey, "err", replyErr)
		}
		return
	}
	if replyErr := app.respond(ctx, messageID, route, message); replyErr != nil {
		app.logError("error.reply.fail", "msg", messageID, "key", route.sessionKey, "err", replyErr)
	}
}
