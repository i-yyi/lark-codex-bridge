package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
)

func (app *daemon) handlePrompt(ctx context.Context, message inboundMessage) error {
	return app.startPromptTask(ctx, message.route, message.event.MessageID, message.text)
}

func (app *daemon) startPromptTask(ctx context.Context, route messageRoute, messageID string, text string) error {
	start := time.Now()
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	app.debug("prompt.dispatch", "msg", messageID, "kind", route.kind, "key", route.sessionKey, "status", runtime.status, "thread", runtime.threadID, "workdir", runtime.workDir, "text_len", len(text))
	switch runtime.status {
	case statusRunning:
		app.mu.Unlock()
		return app.steerRunningTask(ctx, route, messageID, text)
	case statusPending:
		summary := ""
		if runtime.pending != nil {
			summary = runtime.pending.Summary
		}
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, bridge.PickPhrase(messageID+":pending_blocked", bridge.PendingBlocked)+"\n\n"+summary)
	}

	taskCtx, cancel := context.WithCancel(ctx)
	threadID := runtime.threadID
	workDir := runtime.workDir
	defaults := app.defaultModelConfig(route.sessionKey)
	model := firstString(runtime.model, defaults.model)
	effort := firstString(runtime.effort, defaults.effort)
	tier := firstString(runtime.tier, defaults.tier)
	runtime.model = model
	runtime.effort = effort
	runtime.tier = tier
	runtime.status = statusRunning
	runtime.cancel = cancel
	runtime.pending = nil
	runtime.client = nil
	runtime.activeTurnID = ""
	app.log("runtime.status", "key", route.sessionKey, "from", statusIdle, "to", statusRunning)
	app.mu.Unlock()

	statusMessageID := ""
	if route.kind == kindTask {
		body := bridge.PickPhrasef(messageID+":preparing", bridge.TaskPreparing, model, effort, tier)
		card := app.taskCard("Codex 正在处理", "running", body, route, workDir, app.runningActions(route))
		if cardID, err := app.replyCard(ctx, messageID, route, card); err != nil {
			app.logError("card.running.fail", "msg", messageID, "key", route.sessionKey, "err", err)
		} else {
			statusMessageID = cardID
		}
	} else {
		app.reactForRoute(ctx, route, messageID, reactionProcessing)
	}
	app.debug("prompt.scheduled", "msg", messageID, "key", route.sessionKey, "status_card", statusMessageID, "total", elapsed(start))
	go app.runPromptTask(taskCtx, route, messageID, statusMessageID, threadID, workDir, model, effort, tier, text)
	return nil
}

func (app *daemon) runPromptTask(ctx context.Context, route messageRoute, messageID string, statusMessageID string, threadID string, workDir string, model string, effort string, tier string, text string) {
	taskStart := time.Now()
	app.log("codex.task.start", "msg", messageID, "kind", route.kind, "key", route.sessionKey, "saved_thread", threadID, "workdir", workDir, "model", model, "effort", effort, "tier", tier, "text_len", len(text))
	client := codex.NewClient(app.cfg.CodexCLI, model, effort, tier)
	clientStart := time.Now()
	if err := client.Start(ctx); err != nil {
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 启动失败", err, statusMessageID)
		return
	}
	app.debug("codex.client.ok", "msg", messageID, "key", route.sessionKey, "dur", elapsed(clientStart))
	if statusMessageID != "" {
		card := app.taskCard("Codex 正在处理", "running", bridge.PickPhrase(messageID+":started", bridge.CodexStarted), route, workDir, app.runningActions(route))
		_ = app.replyOrPatchCard(context.Background(), messageID, statusMessageID, route, card)
	}
	app.attachClient(route.sessionKey, client)

	threadStart := time.Now()
	activeThreadID, createdThread, err := app.ensureCodexThread(ctx, route.sessionKey, client, threadID, workDir)
	if err != nil {
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 创建 session 失败", err, statusMessageID)
		return
	}
	if createdThread && route.kind == kindChat {
		text = app.chatPromptWithInitialContext(text)
	}

	app.debug("codex.thread.ready", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "dur", elapsed(threadStart))
	if statusMessageID != "" {
		card := app.taskCard("Codex 正在运行", "running", bridge.PickPhrase(messageID+":ready", bridge.SessionReady), route, workDir, app.runningActions(route))
		_ = app.replyOrPatchCard(context.Background(), messageID, statusMessageID, route, card)
	}
	turnStart := time.Now()
	app.debug("codex.turn.start", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "created_thread", createdThread, "text_len", len(text))
	onUpdate, stopUpdates := app.liveCardUpdater(ctx, route, messageID, statusMessageID, workDir)
	onStarted := func(turnID string) {
		backlog := app.markActiveTurn(route.sessionKey, client, activeThreadID, turnID)
		if len(backlog) > 0 {
			go app.runSteerBacklog(context.Background(), route, client, activeThreadID, turnID, backlog)
		}
	}
	result, err := client.RunTurn(ctx, activeThreadID, workDir, text, onUpdate, onStarted)
	stopUpdates()
	if err != nil {
		var pendingErr *codex.PendingRequestError
		if errors.As(err, &pendingErr) {
			app.log("codex.turn.pending", "msg", messageID, "key", route.sessionKey, "method", pendingErr.Pending.Method, "turn", pendingErr.Pending.TurnID, "dur", elapsed(turnStart), "total", elapsed(taskStart))
			app.pauseForPending(context.Background(), route, messageID, statusMessageID, client, pendingErr.Pending)
			return
		}
		app.logError("codex.turn.fail", "msg", messageID, "key", route.sessionKey, "dur", elapsed(turnStart), "total", elapsed(taskStart), "err", err)
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 运行失败", err, statusMessageID)
		return
	}

	app.log("codex.turn.ok", "msg", messageID, "key", route.sessionKey, "thread", result.ThreadID, "turn", result.TurnID, "text_len", len(result.Text), "dur", elapsed(turnStart), "total", elapsed(taskStart))
	app.finishTaskSuccess(context.Background(), route, messageID, client, result, statusMessageID)
}

func (app *daemon) steerRunningTask(ctx context.Context, route messageRoute, messageID string, text string) error {
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	if runtime.status != statusRunning {
		app.mu.Unlock()
		return app.startPromptTask(ctx, route, messageID, text)
	}

	client := runtime.client
	threadID := runtime.threadID
	turnID := runtime.activeTurnID
	if client == nil || strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		runtime.steerBacklog = append(runtime.steerBacklog, steerMessage{messageID: messageID, text: text})
		backlog := len(runtime.steerBacklog)
		app.mu.Unlock()
		app.log("codex.turn.steer.defer", "msg", messageID, "key", route.sessionKey, "backlog", backlog, "text_len", len(text))
		if route.kind == kindTask {
			app.reactForRoute(ctx, route, messageID, reactionProcessing)
			return nil
		}
		return app.respond(ctx, messageID, route, bridge.PickPhrase(messageID+":steer_deferred", bridge.SteerDeferred))
	}
	app.mu.Unlock()

	app.log("codex.turn.steer", "msg", messageID, "key", route.sessionKey, "thread", threadID, "turn", turnID, "text_len", len(text))
	if route.kind == kindTask {
		app.reactForRoute(ctx, route, messageID, reactionProcessing)
	}
	go app.runSteer(context.Background(), route, messageID, client, threadID, turnID, text, "active")
	return nil
}

func (app *daemon) runSteer(ctx context.Context, route messageRoute, messageID string, client *codex.Client, threadID string, turnID string, text string, source string) {
	start := time.Now()
	steerCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	attempts, err := app.steerTurnWithRetry(steerCtx, client, threadID, turnID, text, func(attempt int, delay time.Duration, err error) {
		app.debug("codex.turn.steer.retry", "msg", messageID, "key", route.sessionKey, "thread", threadID, "turn", turnID, "source", source, "attempt", attempt, "delay", delay, "err", err)
	})
	if err != nil {
		app.logError("codex.turn.steer.fail", "msg", messageID, "key", route.sessionKey, "thread", threadID, "turn", turnID, "source", source, "attempts", attempts, "dur", elapsed(start), "err", err)
		if route.kind == kindTask {
			app.reactForRoute(context.Background(), route, messageID, reactionError)
			return
		}
		_ = app.respond(context.Background(), messageID, route, steerFailureMessage(err))
		return
	}

	app.debug("codex.turn.steer.ok", "msg", messageID, "key", route.sessionKey, "thread", threadID, "turn", turnID, "source", source, "attempts", attempts, "dur", elapsed(start))
	if route.kind == kindTask {
		app.reactForRoute(context.Background(), route, messageID, reactionDone)
		return
	}
	_ = app.respond(context.Background(), messageID, route, bridge.PickPhrase(messageID+":steer_inserted", bridge.SteerInserted))
}

func (app *daemon) steerTurnWithRetry(ctx context.Context, client *codex.Client, threadID string, turnID string, text string, onRetry func(int, time.Duration, error)) (int, error) {
	deadline := time.Now().Add(6 * time.Second)
	delay := 200 * time.Millisecond
	attempts := 0
	for {
		attempts++
		err := client.SteerTurn(ctx, threadID, turnID, text)
		if err == nil {
			return attempts, nil
		}
		if !isRetriableSteerError(err) || time.Now().Add(delay).After(deadline) {
			return attempts, err
		}
		if onRetry != nil {
			onRetry(attempts, delay, err)
		}
		select {
		case <-ctx.Done():
			return attempts, ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}

func isRetriableSteerError(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr *codex.RPCError
	if errors.As(err, &rpcErr) {
		message := strings.ToLower(rpcErr.Message)
		data := strings.ToLower(string(rpcErr.Data))
		return strings.Contains(message, "no active turn") ||
			strings.Contains(data, "no active turn") ||
			strings.Contains(data, "activeturnnotsteerable")
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no active turn") ||
		strings.Contains(message, "activeturnnotsteerable")
}

func steerFailureMessage(err error) string {
	if isRetriableSteerError(err) {
		return "插入修正失败：当前 Codex turn 暂时不能接收修正。可以等它产生第一段输出后再发，或用 /cancel 取消。"
	}
	return "插入修正失败：" + err.Error()
}

func (app *daemon) runSteerBacklog(ctx context.Context, route messageRoute, client *codex.Client, threadID string, turnID string, backlog []steerMessage) {
	for _, steer := range backlog {
		app.runSteer(ctx, route, steer.messageID, client, threadID, turnID, steer.text, "backlog")
	}
}

func (app *daemon) ensureCodexThread(ctx context.Context, sessionKey string, client *codex.Client, threadID string, workDir string) (string, bool, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID != "" {
		resumeStart := time.Now()
		app.debug("codex.resume.start", "key", sessionKey, "thread", threadID, "workdir", workDir)
		resumedThreadID, err := client.ResumeThread(ctx, threadID, workDir)
		if err != nil {
			app.logError("codex.resume.fail", "key", sessionKey, "thread", threadID, "dur", elapsed(resumeStart), "err", err)
			threadID = ""
		} else {
			app.setRuntimeThread(sessionKey, resumedThreadID)
			app.debug("codex.resume.ok", "key", sessionKey, "thread", resumedThreadID, "dur", elapsed(resumeStart))
			return resumedThreadID, false, nil
		}
	}

	createStart := time.Now()
	app.debug("codex.thread.create", "key", sessionKey, "workdir", workDir)
	newThreadID, err := client.StartThread(ctx, workDir, false)
	if err != nil {
		app.logError("codex.thread.fail", "key", sessionKey, "workdir", workDir, "dur", elapsed(createStart), "err", err)
		return "", false, err
	}
	if err := app.saveSessionThread(sessionKey, newThreadID); err != nil {
		return "", false, err
	}
	app.debug("codex.thread.ok", "key", sessionKey, "thread", newThreadID, "dur", elapsed(createStart))
	return newThreadID, true, nil
}

func (app *daemon) chatPromptWithInitialContext(text string) string {
	initialPrompt := strings.TrimSpace(app.cfg.ChatInitialPrompt)
	if initialPrompt == "" {
		return text
	}
	return initialPrompt + "\n\n---\n\n用户消息：\n" + strings.TrimSpace(text)
}
