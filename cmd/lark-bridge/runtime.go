package main

import (
	"context"
	"strings"

	"lark-bridge/internal/codex"
	"lark-bridge/internal/lark"
)

func (app *daemon) attachClient(sessionKey string, client *codex.Client) {
	app.mu.Lock()
	defer app.mu.Unlock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	if runtime.status == statusRunning {
		runtime.client = client
	}
}

func (app *daemon) markActiveTurn(sessionKey string, client *codex.Client, threadID string, turnID string) []steerMessage {
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	if runtime.client != client || runtime.status != statusRunning {
		app.mu.Unlock()
		return nil
	}
	runtime.threadID = strings.TrimSpace(threadID)
	runtime.activeTurnID = strings.TrimSpace(turnID)
	backlog := append([]steerMessage(nil), runtime.steerBacklog...)
	runtime.steerBacklog = nil
	app.mu.Unlock()

	app.debug("codex.turn.active", "key", sessionKey, "thread", threadID, "turn", turnID, "backlog", len(backlog))
	return backlog
}

func (app *daemon) clearRuntimeClient(sessionKey string, client *codex.Client) {
	var cancel context.CancelFunc
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	if runtime.client != nil && runtime.client != client {
		app.logError("runtime.clear.skip", "key", sessionKey, "reason", "client_changed")
		app.mu.Unlock()
		return
	}
	previous := runtime.status
	cancel = runtime.cancel
	runtime.status = statusIdle
	runtime.client = nil
	runtime.cancel = nil
	runtime.pending = nil
	runtime.activeTurnID = ""
	runtime.steerBacklog = nil
	app.log("runtime.status", "key", sessionKey, "from", previous, "to", statusIdle)
	app.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (app *daemon) setRuntimeThread(sessionKey string, threadID string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	runtime.threadID = strings.TrimSpace(threadID)
}

func (app *daemon) saveSessionThread(sessionKey string, threadID string) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	runtime.threadID = strings.TrimSpace(threadID)
	app.store.SetSessionThread(sessionKey, threadID)
	return app.store.Save(app.cfg.StatePath)
}

func (app *daemon) taskSessionKey(detail lark.MessageDetail) (string, string) {
	threadKey := "thread:" + strings.TrimSpace(detail.ThreadID)
	rootKey := "thread:" + strings.TrimSpace(detail.RootID)

	if threadKey != "thread:" && app.knownSession(threadKey) {
		return threadKey, "thread_id"
	}
	if rootKey != "thread:" && app.knownSession(rootKey) {
		return rootKey, "root_id"
	}
	return lark.SessionKeyReason(detail)
}

func (app *daemon) knownSession(sessionKey string) bool {
	app.mu.Lock()
	defer app.mu.Unlock()
	_, ok := app.store.Sessions[strings.TrimSpace(sessionKey)]
	return ok
}

func (app *daemon) defaultModelConfig(sessionKey string) codexModelConfig {
	if strings.TrimSpace(sessionKey) == chatSessionKey {
		return codexModelConfig{
			model:  app.cfg.DefaultChatModel,
			effort: app.cfg.DefaultChatEffort,
			tier:   app.cfg.DefaultChatServiceTier,
		}
	}
	return codexModelConfig{
		model:  app.cfg.DefaultTaskModel,
		effort: app.cfg.DefaultTaskEffort,
		tier:   app.cfg.DefaultTaskServiceTier,
	}
}

func (app *daemon) ensureRuntimeLocked(sessionKey string) *sessionRuntime {
	session := app.store.EnsureSession(sessionKey, app.cfg.DefaultWorkDir)
	workDir := strings.TrimSpace(session.WorkDir)
	if workDir == "" {
		workDir = app.cfg.DefaultWorkDir
	}
	defaults := app.defaultModelConfig(sessionKey)
	model := firstString(session.Model, defaults.model)
	effort := firstString(session.ReasoningEffort, defaults.effort)
	tier := firstString(session.ServiceTier, defaults.tier)

	runtime := app.sessions[sessionKey]
	if runtime == nil {
		runtime = &sessionRuntime{
			key:      sessionKey,
			status:   statusIdle,
			threadID: strings.TrimSpace(session.CodexThreadID),
			workDir:  workDir,
			model:    model,
			effort:   effort,
			tier:     tier,
		}
		app.sessions[sessionKey] = runtime
		return runtime
	}
	if runtime.workDir == "" {
		runtime.workDir = workDir
	}
	if runtime.threadID == "" {
		runtime.threadID = strings.TrimSpace(session.CodexThreadID)
	}
	if runtime.model == "" {
		runtime.model = model
	}
	if runtime.effort == "" {
		runtime.effort = effort
	}
	if runtime.tier == "" {
		runtime.tier = tier
	}
	return runtime
}
