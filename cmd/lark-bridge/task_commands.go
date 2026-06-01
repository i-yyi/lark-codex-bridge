package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	"lark-bridge/internal/lark"
)

type boundSession struct {
	workDir string
	model   string
	effort  string
	tier    string
	status  sessionStatus
	bound   bool
}

func (app *daemon) handleCreateTask(ctx context.Context, event lark.MessageEvent, route messageRoute, arg string) error {
	start := time.Now()
	if route.kind == kindTask {
		return app.respond(ctx, event.MessageID, route, "当前已经在 task 话题里。直接在这个话题继续发任务即可。")
	}

	parseStart := time.Now()
	request, err := app.parseCreateRequest(arg)
	if err != nil {
		app.log("task.create.invalid", "msg", event.MessageID, "dur", elapsed(parseStart), "err", err)
		return app.respond(ctx, event.MessageID, route, err.Error())
	}
	app.debug("task.create.parsed", "msg", event.MessageID, "project", request.project, "workdir", request.workDir, "model", request.model, "effort", request.effort, "tier", request.tier, "prompt_len", len(request.prompt), "dur", elapsed(parseStart))

	taskDefaults := app.defaultModelConfig("thread:default")
	message := bridge.PickPhrase(event.MessageID+":create_wait", bridge.TaskCreatedWait)
	if request.prompt != "" {
		message = bridge.PickPhrase(event.MessageID+":create_start", bridge.TaskCreatedStart)
	}
	message += "\nworkdir: " + firstString(request.workDir, app.cfg.DefaultWorkDir)
	if request.project != "" {
		message += "\nproject: " + request.project
	}
	message += "\nmodel: " + firstString(request.model, taskDefaults.model)
	message += "\neffort: " + firstString(request.effort, taskDefaults.effort)
	message += "\ntier: " + firstString(request.tier, taskDefaults.tier)

	replyStart := time.Now()
	createdMessageID, err := app.lark.ReplyText(ctx, event.MessageID, message, true)
	if err != nil {
		app.logError("task.create.reply.fail", "msg", event.MessageID, "dur", elapsed(replyStart), "err", err)
		return err
	}
	app.debug("task.create.reply.ok", "msg", event.MessageID, "reply", createdMessageID, "dur", elapsed(replyStart))

	taskRoute := app.taskRouteForCreatedThread(ctx, event.MessageID, createdMessageID)
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(taskRoute.sessionKey)
	if request.workDir != "" {
		runtime.workDir = request.workDir
		app.store.SetSessionWorkDir(taskRoute.sessionKey, request.workDir)
	}
	defaults := app.defaultModelConfig(taskRoute.sessionKey)
	model := firstString(request.model, runtime.model, defaults.model)
	effort := firstString(request.effort, runtime.effort, defaults.effort)
	tier := firstString(request.tier, runtime.tier, defaults.tier)
	runtime.model = model
	runtime.effort = effort
	runtime.tier = tier
	app.store.SetSessionModelConfig(taskRoute.sessionKey, model, effort, tier)
	status := runtime.status
	workDir := runtime.workDir
	model = runtime.model
	effort = runtime.effort
	tier = runtime.tier
	saveStart := time.Now()
	saveErr := app.store.Save(app.cfg.StatePath)
	app.mu.Unlock()
	if saveErr != nil {
		app.logError("task.create.save.fail", "msg", event.MessageID, "key", taskRoute.sessionKey, "dur", elapsed(saveStart), "err", saveErr)
		return saveErr
	}
	app.log("task.create.ok", "msg", event.MessageID, "reply", createdMessageID, "key", taskRoute.sessionKey, "reason", taskRoute.reason, "status", status, "workdir", workDir, "model", model, "effort", effort, "tier", tier, "prompt_len", len(request.prompt), "save", elapsed(saveStart), "total", elapsed(start))
	if request.prompt == "" {
		return nil
	}
	return app.startPromptTask(ctx, taskRoute, createdMessageID, request.prompt)
}

func (app *daemon) handleAttach(ctx context.Context, event lark.MessageEvent, route messageRoute, arg string) error {
	request, err := app.parseAttachRequest(arg)
	if err != nil {
		app.log("session.attach.invalid", "msg", event.MessageID, "err", err)
		return app.respond(ctx, event.MessageID, route, err.Error())
	}

	taskRoute, ok := app.attachRoute(route)
	if !ok {
		return app.respond(ctx, event.MessageID, route, "/attach 需要在飞书话题里使用。先 /create 创建一个 task 话题，或在已有话题里执行 /attach。")
	}

	binding, err := app.bindExistingCodexSession(taskRoute, request)
	if !binding.bound {
		return app.respond(ctx, event.MessageID, taskRoute, fmt.Sprintf("当前会话状态是 %s，不能 attach。", binding.status))
	}
	if err != nil {
		app.logError("session.attach.save.fail", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "err", err)
		return err
	}

	app.log("session.attach.ok", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "project", request.project, "workdir", binding.workDir, "model", binding.model, "effort", binding.effort, "tier", binding.tier)
	projectLine := ""
	if request.project != "" {
		projectLine = "\nproject: " + request.project
	}
	return app.respond(ctx, event.MessageID, taskRoute, fmt.Sprintf("已 attach 到 Codex session。\nkey: %s\ncodex_thread: %s\nworkdir: %s%s\nmodel: %s\neffort: %s\ntier: %s", taskRoute.sessionKey, request.threadID, binding.workDir, projectLine, binding.model, binding.effort, binding.tier))
}

func (app *daemon) bindExistingCodexSession(route messageRoute, request attachRequest) (boundSession, error) {
	app.mu.Lock()
	defer app.mu.Unlock()

	runtime := app.ensureRuntimeLocked(route.sessionKey)
	if runtime.status == statusRunning || runtime.status == statusPending {
		return boundSession{status: runtime.status}, nil
	}

	if request.workDir != "" {
		runtime.workDir = request.workDir
		app.store.SetSessionWorkDir(route.sessionKey, request.workDir)
	}
	runtime.threadID = request.threadID
	defaults := app.defaultModelConfig(route.sessionKey)
	runtime.model = firstString(request.model, runtime.model, defaults.model)
	runtime.effort = firstString(request.effort, runtime.effort, defaults.effort)
	runtime.tier = firstString(request.tier, runtime.tier, defaults.tier)
	app.store.SetSessionThread(route.sessionKey, runtime.threadID)
	app.store.SetSessionModelConfig(route.sessionKey, runtime.model, runtime.effort, runtime.tier)

	err := app.store.Save(app.cfg.StatePath)
	return boundSession{
		workDir: runtime.workDir,
		model:   runtime.model,
		effort:  runtime.effort,
		tier:    runtime.tier,
		status:  runtime.status,
		bound:   true,
	}, err
}

func (app *daemon) handleHistory(ctx context.Context, messageID string, route messageRoute, arg string) error {
	limit, err := parseHistoryLimit(arg)
	if err != nil {
		return app.respond(ctx, messageID, route, err.Error())
	}

	sessions, err := codex.ScanHistory(limit)
	if err != nil {
		app.logError("history.scan.fail", "msg", messageID, "err", err)
		return app.respond(ctx, messageID, route, "扫描 Codex history 失败："+err.Error())
	}

	app.mu.Lock()
	app.history = sessions
	app.mu.Unlock()

	app.log("history.scan.ok", "msg", messageID, "count", len(sessions), "limit", limit)
	return app.respond(ctx, messageID, route, codex.FormatHistoryText(sessions))
}

func (app *daemon) handleImport(ctx context.Context, event lark.MessageEvent, route messageRoute, arg string) error {
	request, info, err := app.parseImportRequest(arg)
	if err != nil {
		app.log("session.import.invalid", "msg", event.MessageID, "err", err)
		return app.respond(ctx, event.MessageID, route, err.Error())
	}

	taskRoute := route
	sourceMessageID := event.MessageID
	if route.kind != kindTask {
		createdMessageID, err := app.lark.ReplyText(ctx, event.MessageID, "已创建导入话题，正在绑定 Codex session。", true)
		if err != nil {
			app.logError("session.import.reply.fail", "msg", event.MessageID, "thread", request.threadID, "err", err)
			return err
		}
		sourceMessageID = createdMessageID
		taskRoute = app.taskRouteForCreatedThread(ctx, event.MessageID, createdMessageID)
	}

	binding, err := app.bindExistingCodexSession(taskRoute, request)
	if !binding.bound {
		return app.respond(ctx, sourceMessageID, taskRoute, fmt.Sprintf("当前会话状态是 %s，不能 import。", binding.status))
	}
	if err != nil {
		app.logError("session.import.save.fail", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "err", err)
		return err
	}

	titleLine := ""
	if info.Title != "" {
		titleLine = "\ntitle: " + info.Title
	}
	app.log("session.import.ok", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "workdir", binding.workDir, "model", binding.model, "effort", binding.effort, "tier", binding.tier)
	return app.respond(ctx, sourceMessageID, taskRoute, fmt.Sprintf("已导入 Codex session。\nkey: %s\ncodex_thread: %s\nworkdir: %s%s\nmodel: %s\neffort: %s\ntier: %s", taskRoute.sessionKey, request.threadID, binding.workDir, titleLine, binding.model, binding.effort, binding.tier))
}

func (app *daemon) parseImportRequest(arg string) (attachRequest, codex.SessionInfo, error) {
	request, err := app.parseAttachRequest(arg)
	if err != nil {
		return attachRequest{}, codex.SessionInfo{}, fmt.Errorf("用法：/import ID_OR_INDEX [--cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast]")
	}

	info, ok := app.resolveHistorySelection(request.threadID)
	if !ok {
		if _, err := strconv.Atoi(request.threadID); err == nil {
			return attachRequest{}, codex.SessionInfo{}, fmt.Errorf("history 序号不存在。请先执行 /history 查看可导入会话")
		}
		return request, codex.SessionInfo{}, nil
	}

	request.threadID = info.ID
	if request.workDir == "" && info.CWD != "" {
		workDir, err := app.resolveWorkDir(info.CWD)
		if err != nil {
			return attachRequest{}, codex.SessionInfo{}, fmt.Errorf("history session cwd 不可用：%w。请用 --cwd 指定可访问目录", err)
		}
		request.workDir = workDir
	}
	return request, info, nil
}

func (app *daemon) resolveHistorySelection(selection string) (codex.SessionInfo, bool) {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return codex.SessionInfo{}, false
	}

	app.mu.Lock()
	defer app.mu.Unlock()

	if index, err := strconv.Atoi(selection); err == nil {
		if index >= 1 && index <= len(app.history) {
			return app.history[index-1], true
		}
		return codex.SessionInfo{}, false
	}

	for _, session := range app.history {
		if session.ID == selection {
			return session, true
		}
	}
	return codex.SessionInfo{}, false
}

func (app *daemon) attachRoute(route messageRoute) (messageRoute, bool) {
	if route.kind == kindTask {
		return route, true
	}
	if route.reason != "unknown_thread" {
		return messageRoute{}, false
	}
	sessionKey, reason := lark.SessionKeyReason(route.detail)
	if reason == "no_thread_or_root" || strings.TrimSpace(sessionKey) == "" {
		return messageRoute{}, false
	}
	return messageRoute{sessionKey: sessionKey, kind: kindTask, reason: "attach_" + reason, detail: route.detail}, true
}

func parseHistoryLimit(arg string) (int, error) {
	fields := strings.Fields(strings.TrimSpace(arg))
	limit := 10
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch {
		case field == "--limit":
			index++
			if index >= len(fields) {
				return 0, fmt.Errorf("--limit 需要一个数字")
			}
			value, err := strconv.Atoi(fields[index])
			if err != nil || value <= 0 {
				return 0, fmt.Errorf("--limit 必须是正整数")
			}
			limit = value
		default:
			return 0, fmt.Errorf("未知 /history 参数：%s", field)
		}
	}
	if limit > 50 {
		limit = 50
	}
	return limit, nil
}

func (app *daemon) taskRouteForCreatedThread(ctx context.Context, rootMessageID string, botMessageID string) messageRoute {
	start := time.Now()
	detail, err := app.lark.FetchMessageDetail(ctx, botMessageID)
	if err != nil {
		fallback := "thread:" + strings.TrimSpace(rootMessageID)
		app.logError("task.route.fail", "root", rootMessageID, "reply", botMessageID, "fallback", fallback, "dur", elapsed(start), "err", err)
		return messageRoute{sessionKey: fallback, kind: kindTask, reason: "create_detail_failed"}
	}

	sessionKey, reason := lark.SessionKeyReason(detail)
	if reason == "no_thread_or_root" {
		sessionKey = "thread:" + strings.TrimSpace(rootMessageID)
		reason = "create_fallback_root_message"
	}
	app.debug("task.route.ok", "root", rootMessageID, "reply", botMessageID, "thread", detail.ThreadID, "root_id", detail.RootID, "key", sessionKey, "reason", reason, "dur", elapsed(start))
	return messageRoute{sessionKey: sessionKey, kind: kindTask, reason: reason, detail: detail}
}
