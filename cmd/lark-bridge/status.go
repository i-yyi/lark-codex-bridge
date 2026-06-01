package main

import (
	"fmt"
	"sort"
	"strings"
)

func (app *daemon) projectNames() []string {
	names := make([]string, 0, len(app.cfg.WorkDirs))
	for name := range app.cfg.WorkDirs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (app *daemon) projectsText() string {
	names := app.projectNames()
	if len(names) == 0 {
		return "projects: none"
	}
	var builder strings.Builder
	builder.WriteString("projects:")
	for _, name := range names {
		fmt.Fprintf(&builder, "\n- %s: %s", name, app.cfg.WorkDirs[name])
	}
	return builder.String()
}

func (app *daemon) sessionsText(route messageRoute) string {
	app.mu.Lock()
	defer app.mu.Unlock()

	keys := make([]string, 0, len(app.store.Sessions))
	for key := range app.store.Sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		return "sessions: none"
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "sessions: %d\ncurrent: %s\n", len(keys), route.sessionKey)
	for _, key := range keys {
		snapshot := app.sessionStatusSnapshotLocked(key)
		current := ""
		if key == route.sessionKey {
			current = " current"
		}
		fmt.Fprintf(&builder, "- key=%s%s status=%s codex_thread=%s active_turn=%s workdir=%s model=%s effort=%s tier=%s pending=%s steer_backlog=%d\n", key, current, snapshot.status, snapshot.threadID, snapshot.activeTurnID, snapshot.workDir, snapshot.model, snapshot.effort, snapshot.tier, snapshot.pending, snapshot.steerBacklog)
	}
	return strings.TrimSpace(builder.String())
}

func (app *daemon) statusText(route messageRoute) string {
	app.mu.Lock()
	defer app.mu.Unlock()

	current := app.sessionStatusSnapshotLocked(route.sessionKey)
	chat := app.sessionStatusSnapshotLocked(chatSessionKey)

	running := 0
	pending := 0
	taskKeys := make([]string, 0)
	for key := range app.store.Sessions {
		runtime := app.sessions[key]
		status := statusIdle
		if runtime != nil {
			status = runtime.status
		}
		switch status {
		case statusRunning:
			running++
		case statusPending:
			pending++
		}
		if strings.HasPrefix(key, "thread:") {
			taskKeys = append(taskKeys, key)
		}
	}
	sort.Strings(taskKeys)

	var builder strings.Builder
	fmt.Fprintf(&builder, "online\n")
	fmt.Fprintf(&builder, "current_kind: %s\ncurrent_key: %s\ncurrent_status: %s\ncurrent_codex_thread: %s\ncurrent_active_turn: %s\ncurrent_workdir: %s\ncurrent_model: %s\ncurrent_effort: %s\ncurrent_tier: %s\ncurrent_pending: %s\ncurrent_steer_backlog: %d\n", route.kind, route.sessionKey, current.status, current.threadID, current.activeTurnID, current.workDir, current.model, current.effort, current.tier, current.pending, current.steerBacklog)
	fmt.Fprintf(&builder, "running: %d\npending: %d\nsessions: %d\ntasks: %d\nstate: %s\n", running, pending, len(app.store.Sessions), len(taskKeys), app.cfg.StatePath)
	fmt.Fprintf(&builder, "chat: status=%s codex_thread=%s active_turn=%s workdir=%s model=%s effort=%s tier=%s steer_backlog=%d\n", chat.status, chat.threadID, chat.activeTurnID, chat.workDir, chat.model, chat.effort, chat.tier, chat.steerBacklog)

	if len(taskKeys) == 0 {
		builder.WriteString("task_list: none")
		return builder.String()
	}
	builder.WriteString("task_list:")
	for _, key := range taskKeys {
		snapshot := app.sessionStatusSnapshotLocked(key)
		fmt.Fprintf(&builder, "\n- key=%s status=%s codex_thread=%s active_turn=%s workdir=%s model=%s effort=%s tier=%s pending=%s steer_backlog=%d", key, snapshot.status, snapshot.threadID, snapshot.activeTurnID, snapshot.workDir, snapshot.model, snapshot.effort, snapshot.tier, snapshot.pending, snapshot.steerBacklog)
	}
	return builder.String()
}

type sessionStatusSnapshot struct {
	status       sessionStatus
	threadID     string
	activeTurnID string
	workDir      string
	model        string
	effort       string
	tier         string
	pending      string
	steerBacklog int
}

func (app *daemon) sessionStatusSnapshotLocked(sessionKey string) sessionStatusSnapshot {
	session := app.store.EnsureSession(sessionKey, app.cfg.DefaultWorkDir)
	defaults := app.defaultModelConfig(sessionKey)
	snapshot := sessionStatusSnapshot{
		status:       statusIdle,
		threadID:     strings.TrimSpace(session.CodexThreadID),
		activeTurnID: "",
		workDir:      strings.TrimSpace(session.WorkDir),
		model:        firstString(session.Model, defaults.model),
		effort:       firstString(session.ReasoningEffort, defaults.effort),
		tier:         firstString(session.ServiceTier, defaults.tier),
		pending:      "none",
		steerBacklog: 0,
	}
	if snapshot.workDir == "" {
		snapshot.workDir = app.cfg.DefaultWorkDir
	}
	if runtime := app.sessions[sessionKey]; runtime != nil {
		snapshot.status = runtime.status
		if runtime.threadID != "" {
			snapshot.threadID = runtime.threadID
		}
		if runtime.activeTurnID != "" {
			snapshot.activeTurnID = runtime.activeTurnID
		}
		if runtime.workDir != "" {
			snapshot.workDir = runtime.workDir
		}
		if runtime.model != "" {
			snapshot.model = runtime.model
		}
		if runtime.effort != "" {
			snapshot.effort = runtime.effort
		}
		if runtime.tier != "" {
			snapshot.tier = runtime.tier
		}
		if runtime.pending != nil {
			snapshot.pending = runtime.pending.Method
		}
		snapshot.steerBacklog = len(runtime.steerBacklog)
	}
	return snapshot
}
