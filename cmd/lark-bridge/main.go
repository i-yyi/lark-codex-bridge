package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	configpkg "lark-bridge/internal/config"
	"lark-bridge/internal/lark"
	statepkg "lark-bridge/internal/state"
)

const (
	reactionCommand    = "OK"
	reactionProcessing = "OnIt"
	reactionDone       = "DONE"
	reactionError      = "ERROR"
)

type sessionStatus string

const (
	statusIdle    sessionStatus = "idle"
	statusRunning sessionStatus = "running"
	statusPending sessionStatus = "pending"
)

type sessionKind string

const (
	kindChat sessionKind = "chat"
	kindTask sessionKind = "task"
)

const chatSessionKey = "chat:default"

type messageRoute struct {
	sessionKey string
	kind       sessionKind
	reason     string
	detail     lark.MessageDetail
}

type sessionRuntime struct {
	key      string
	status   sessionStatus
	threadID string
	workDir  string
	client   *codex.Client
	cancel   context.CancelFunc
	pending  *codex.PendingRequest
}

type daemon struct {
	cfg    configpkg.Config
	store  statepkg.State
	lark   lark.Client
	logger *log.Logger
	seen   map[string]struct{}

	mu       sync.Mutex
	sessions map[string]*sessionRuntime
}

func (app *daemon) log(event string, fields ...any) {
	app.logger.Printf("%-20s %s", event, formatLogFields(fields...))
}

func formatLogFields(fields ...any) string {
	parts := make([]string, 0, (len(fields)+1)/2)
	for index := 0; index < len(fields); index += 2 {
		key := fmt.Sprint(fields[index])
		value := ""
		if index+1 < len(fields) {
			value = formatLogValue(fields[index+1])
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, " ")
}

func formatLogValue(value any) string {
	if value == nil {
		return "-"
	}
	switch typed := value.(type) {
	case time.Duration:
		return typed.Round(time.Millisecond).String()
	case error:
		return quoteLogString(typed.Error())
	case fmt.Stringer:
		return quoteLogString(typed.String())
	case string:
		return quoteLogString(typed)
	default:
		return quoteLogString(fmt.Sprint(value))
	}
}

func quoteLogString(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if value == "" {
		return "-"
	}
	if strings.ContainsAny(value, " \t\"") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

func elapsed(start time.Time) time.Duration {
	return time.Since(start)
}

func parseEventMillis(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(millis), true
}

func eventLag(from string, to string) any {
	fromTime, ok := parseEventMillis(from)
	if !ok {
		return nil
	}
	toTime, ok := parseEventMillis(to)
	if !ok {
		return nil
	}
	return toTime.Sub(fromTime)
}

func recvLag(eventTime string, receivedAt time.Time) any {
	createdAt, ok := parseEventMillis(eventTime)
	if !ok {
		return nil
	}
	return receivedAt.Sub(createdAt)
}

func main() {
	configPath := flag.String("config", "config.json", "path to JSON config")
	flag.Parse()

	logger := log.New(os.Stdout, "lark-bridge ", log.LstdFlags|log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal(err)
	}
}

func run(ctx context.Context, configPath string, logger *log.Logger) error {
	cfg, err := configpkg.LoadConfig(configPath)
	if err != nil {
		return err
	}

	store, err := statepkg.Load(cfg.StatePath, cfg.DefaultWorkDir)
	if err != nil {
		return err
	}

	consumer, err := lark.StartMessageConsumer(ctx, cfg.LarkCLI, cfg.LarkCLINoProxy)
	if err != nil {
		return err
	}
	defer consumer.Close()

	app := &daemon{
		cfg:      cfg,
		store:    store,
		lark:     lark.NewClient(cfg.LarkCLI, cfg.LarkCLINoProxy),
		logger:   logger,
		seen:     make(map[string]struct{}),
		sessions: make(map[string]*sessionRuntime),
	}
	defer app.shutdown()

	app.logStateSummary()
	app.log("daemon.online", "owner", cfg.OwnerOpenID, "sessions", len(store.Sessions), "state", cfg.StatePath, "workdir", cfg.DefaultWorkDir, "lark_cli", cfg.LarkCLI, "lark_no_proxy", cfg.LarkCLINoProxy, "codex_cli", cfg.CodexCLI)
	onlineStart := time.Now()
	if _, err := app.lark.SendText(ctx, cfg.OwnerOpenID, "lark-bridge online"); err != nil {
		app.log("lark.send.failed", "target", "owner", "dur", elapsed(onlineStart), "err", err)
	} else {
		app.log("lark.send.ok", "target", "owner", "dur", elapsed(onlineStart), "text_len", len("lark-bridge online"))
	}

	for {
		event, err := consumer.Receive(ctx)
		if err != nil {
			return err
		}
		if err := app.handleEvent(ctx, event); err != nil {
			app.log("event.failed", "msg", event.MessageID, "err", err)
		}
	}
}

func (app *daemon) logStateSummary() {
	app.mu.Lock()
	defer app.mu.Unlock()

	keys := make([]string, 0, len(app.store.Sessions))
	for key := range app.store.Sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	app.log("state.summary", "sessions", len(keys))
	for _, key := range keys {
		session := app.store.Sessions[key]
		hasThread := strings.TrimSpace(session.CodexThreadID) != ""
		app.log("state.session", "key", key, "has_thread", hasThread, "workdir", session.WorkDir)
	}
}

func (app *daemon) shutdown() {
	var clients []*codex.Client
	var cancels []context.CancelFunc

	app.mu.Lock()
	for _, runtime := range app.sessions {
		if runtime.cancel != nil {
			cancels = append(cancels, runtime.cancel)
		}
		if runtime.client != nil {
			clients = append(clients, runtime.client)
		}
		runtime.status = statusIdle
		runtime.client = nil
		runtime.cancel = nil
		runtime.pending = nil
	}
	app.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	for _, client := range clients {
		_ = client.Close()
	}
}

func (app *daemon) handleEvent(ctx context.Context, event lark.MessageEvent) error {
	start := time.Now()
	if event.SenderID != app.cfg.OwnerOpenID {
		return nil
	}
	if event.MessageType != "text" {
		app.log("event.ignore", "reason", "non_text", "event", event.EventID, "msg", event.MessageID, "type", event.MessageType, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if event.MessageID == "" {
		app.log("event.ignore", "reason", "missing_msg", "event", event.EventID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if _, ok := app.seen[event.MessageID]; ok {
		app.log("event.ignore", "reason", "duplicate", "event", event.EventID, "msg", event.MessageID, "total", elapsed(start))
		return nil
	}
	app.seen[event.MessageID] = struct{}{}

	text := strings.TrimSpace(event.Content)
	if text == "" {
		app.log("event.ignore", "reason", "empty_text", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}

	app.log("event.accept", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "chat_type", event.ChatType, "text_len", len(text), "create_time", event.CreateTime, "event_ts", event.Timestamp, "bus_lag", eventLag(event.CreateTime, event.Timestamp), "recv_lag", recvLag(event.CreateTime, start))
	routeStart := time.Now()
	route := app.eventRoute(ctx, event)
	app.log("event.route", "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey, "reason", route.reason, "dur", elapsed(routeStart))

	command := bridge.ParseCommand(text)
	dispatchStart := time.Now()
	var err error
	if command.IsCommand {
		err = app.handleCommand(ctx, event, route, command)
	} else {
		err = app.handlePrompt(ctx, event, route, text)
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	app.log("event.done", "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey, "command", command.Name, "status", status, "dispatch", elapsed(dispatchStart), "total", elapsed(start), "err", err)
	return err
}

func (app *daemon) eventRoute(ctx context.Context, event lark.MessageEvent) messageRoute {
	detailStart := time.Now()
	detail, err := app.lark.FetchMessageDetail(ctx, event.MessageID)
	if err != nil {
		app.log("lark.detail.fail", "msg", event.MessageID, "dur", elapsed(detailStart), "err", err, "fallback", chatSessionKey)
		return messageRoute{sessionKey: chatSessionKey, kind: kindChat, reason: "detail_failed"}
	}
	app.log("lark.detail.ok", "msg", event.MessageID, "detail_msg", detail.MessageID, "thread", detail.ThreadID, "root", detail.RootID, "dur", elapsed(detailStart))

	sessionKey, reason := app.taskSessionKey(detail)
	kind := kindTask
	if reason == "no_thread_or_root" {
		sessionKey = chatSessionKey
		kind = kindChat
	}
	if kind == kindTask && !app.knownSession(sessionKey) {
		app.log("route.unknown", "msg", event.MessageID, "detail_msg", detail.MessageID, "thread", detail.ThreadID, "root", detail.RootID, "candidate", sessionKey, "reason", reason, "fallback", chatSessionKey)
		return messageRoute{sessionKey: chatSessionKey, kind: kindChat, reason: "unknown_thread", detail: detail}
	}

	route := messageRoute{sessionKey: sessionKey, kind: kind, reason: reason, detail: detail}
	app.log("route.selected", "msg", event.MessageID, "detail_msg", detail.MessageID, "thread", detail.ThreadID, "root", detail.RootID, "kind", route.kind, "key", route.sessionKey, "reason", route.reason)
	return route
}

func (app *daemon) handleCommand(ctx context.Context, event lark.MessageEvent, route messageRoute, command bridge.Command) error {
	app.log("command.recv", "name", command.Name, "arg_len", len(command.Arg), "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey)
	app.reactForRoute(ctx, route, event.MessageID, reactionCommand)
	switch command.Name {
	case bridge.CommandHelp:
		return app.respond(ctx, event.MessageID, route, "支持：/create [--cwd DIR] [任务], /status, /reset, /approve, /deny, /cancel")
	case bridge.CommandCreate:
		return app.handleCreateTask(ctx, event, route, command.Arg)
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
		return app.respond(ctx, event.MessageID, route, "未知命令。当前支持：/create [--cwd DIR] [任务], /status, /reset, /approve, /deny, /cancel")
	default:
		return app.respond(ctx, event.MessageID, route, fmt.Sprintf("/%s 还没实现", command.Name))
	}
}

type createRequest struct {
	workDir string
	prompt  string
}

func (app *daemon) parseCreateRequest(arg string) (createRequest, error) {
	fields := strings.Fields(strings.TrimSpace(arg))
	request := createRequest{}
	promptFields := make([]string, 0, len(fields))

	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch {
		case field == "--cwd" || field == "--workdir":
			index++
			if index >= len(fields) {
				return createRequest{}, fmt.Errorf("%s 需要一个目录参数", field)
			}
			workDir, err := app.resolveWorkDir(fields[index])
			if err != nil {
				return createRequest{}, err
			}
			request.workDir = workDir
		case strings.HasPrefix(field, "--cwd="):
			workDir, err := app.resolveWorkDir(strings.TrimPrefix(field, "--cwd="))
			if err != nil {
				return createRequest{}, err
			}
			request.workDir = workDir
		case strings.HasPrefix(field, "--workdir="):
			workDir, err := app.resolveWorkDir(strings.TrimPrefix(field, "--workdir="))
			if err != nil {
				return createRequest{}, err
			}
			request.workDir = workDir
		case strings.HasPrefix(field, "--"):
			return createRequest{}, fmt.Errorf("未知 /create 参数：%s", field)
		default:
			promptFields = append(promptFields, field)
		}
	}

	request.prompt = strings.TrimSpace(strings.Join(promptFields, " "))
	return request, nil
}

func (app *daemon) resolveWorkDir(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("workdir 不能为空")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 home 目录失败：%w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("解析 workdir %q 失败：%w", raw, err)
		}
		path = absolute
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("workdir 不可访问：%s", path)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir 不是目录：%s", path)
	}
	return path, nil
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
	app.log("task.create.parsed", "msg", event.MessageID, "workdir", request.workDir, "prompt_len", len(request.prompt), "dur", elapsed(parseStart))

	message := "Task 已创建。请在这个话题里继续发送任务。"
	if request.prompt != "" {
		message = "Task 已创建，开始处理。"
	}
	if request.workDir != "" {
		message += "\nworkdir: " + request.workDir
	}

	replyStart := time.Now()
	createdMessageID, err := app.lark.ReplyText(ctx, event.MessageID, message, true)
	if err != nil {
		app.log("task.create.reply.fail", "msg", event.MessageID, "dur", elapsed(replyStart), "err", err)
		return err
	}
	app.log("task.create.reply.ok", "msg", event.MessageID, "reply", createdMessageID, "dur", elapsed(replyStart))

	taskRoute := app.taskRouteForCreatedThread(ctx, event.MessageID, createdMessageID)
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(taskRoute.sessionKey)
	if request.workDir != "" {
		runtime.workDir = request.workDir
		app.store.SetSessionWorkDir(taskRoute.sessionKey, request.workDir)
	}
	status := runtime.status
	workDir := runtime.workDir
	saveStart := time.Now()
	saveErr := app.store.Save(app.cfg.StatePath)
	app.mu.Unlock()
	if saveErr != nil {
		app.log("task.create.save.fail", "msg", event.MessageID, "key", taskRoute.sessionKey, "dur", elapsed(saveStart), "err", saveErr)
		return saveErr
	}
	app.log("task.create.ok", "msg", event.MessageID, "reply", createdMessageID, "key", taskRoute.sessionKey, "reason", taskRoute.reason, "status", status, "workdir", workDir, "prompt_len", len(request.prompt), "save", elapsed(saveStart), "total", elapsed(start))
	if request.prompt == "" {
		return nil
	}
	return app.startPromptTask(ctx, taskRoute, createdMessageID, request.prompt)
}

func (app *daemon) taskRouteForCreatedThread(ctx context.Context, rootMessageID string, botMessageID string) messageRoute {
	start := time.Now()
	detail, err := app.lark.FetchMessageDetail(ctx, botMessageID)
	if err != nil {
		fallback := "thread:" + strings.TrimSpace(rootMessageID)
		app.log("task.route.fail", "root", rootMessageID, "reply", botMessageID, "fallback", fallback, "dur", elapsed(start), "err", err)
		return messageRoute{sessionKey: fallback, kind: kindTask, reason: "create_detail_failed"}
	}

	sessionKey, reason := lark.SessionKeyReason(detail)
	if reason == "no_thread_or_root" {
		sessionKey = "thread:" + strings.TrimSpace(rootMessageID)
		reason = "create_fallback_root_message"
	}
	app.log("task.route.ok", "root", rootMessageID, "reply", botMessageID, "thread", detail.ThreadID, "root_id", detail.RootID, "key", sessionKey, "reason", reason, "dur", elapsed(start))
	return messageRoute{sessionKey: sessionKey, kind: kindTask, reason: reason, detail: detail}
}

func (app *daemon) handlePrompt(ctx context.Context, event lark.MessageEvent, route messageRoute, text string) error {
	return app.startPromptTask(ctx, route, event.MessageID, text)
}

func (app *daemon) startPromptTask(ctx context.Context, route messageRoute, messageID string, text string) error {
	start := time.Now()
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	app.log("prompt.dispatch", "msg", messageID, "kind", route.kind, "key", route.sessionKey, "status", runtime.status, "thread", runtime.threadID, "workdir", runtime.workDir, "text_len", len(text))
	switch runtime.status {
	case statusRunning:
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "这个会话已有任务在运行。请稍后，或回复 /cancel 取消。")
	case statusPending:
		summary := ""
		if runtime.pending != nil {
			summary = runtime.pending.Summary
		}
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "Codex 正在等待确认。请先回复 /approve、/deny 或 /cancel。\n\n"+summary)
	}

	taskCtx, cancel := context.WithCancel(ctx)
	threadID := runtime.threadID
	workDir := runtime.workDir
	runtime.status = statusRunning
	runtime.cancel = cancel
	runtime.pending = nil
	runtime.client = nil
	app.log("runtime.status", "key", route.sessionKey, "from", statusIdle, "to", statusRunning)
	app.mu.Unlock()

	reactStart := time.Now()
	app.reactForRoute(ctx, route, messageID, reactionProcessing)
	app.log("prompt.scheduled", "msg", messageID, "key", route.sessionKey, "react", elapsed(reactStart), "total", elapsed(start))
	go app.runPromptTask(taskCtx, route, messageID, threadID, workDir, text)
	return nil
}

func (app *daemon) runPromptTask(ctx context.Context, route messageRoute, messageID string, threadID string, workDir string, text string) {
	taskStart := time.Now()
	app.log("codex.task.start", "msg", messageID, "kind", route.kind, "key", route.sessionKey, "saved_thread", threadID, "workdir", workDir, "text_len", len(text))
	client := codex.NewClient(app.cfg.CodexCLI)
	clientStart := time.Now()
	if err := client.Start(ctx); err != nil {
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 启动失败", err)
		return
	}
	app.log("codex.client.ok", "msg", messageID, "key", route.sessionKey, "dur", elapsed(clientStart))
	app.attachClient(route.sessionKey, client)

	threadStart := time.Now()
	activeThreadID, err := app.ensureCodexThread(ctx, route.sessionKey, client, threadID, workDir)
	if err != nil {
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 创建 session 失败", err)
		return
	}

	app.log("codex.thread.ready", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "dur", elapsed(threadStart))
	turnStart := time.Now()
	app.log("codex.turn.start", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "text_len", len(text))
	result, err := client.RunTurn(ctx, activeThreadID, workDir, text)
	if err != nil {
		var pendingErr *codex.PendingRequestError
		if errors.As(err, &pendingErr) {
			app.log("codex.turn.pending", "msg", messageID, "key", route.sessionKey, "method", pendingErr.Pending.Method, "turn", pendingErr.Pending.TurnID, "dur", elapsed(turnStart), "total", elapsed(taskStart))
			app.pauseForPending(context.Background(), route, messageID, client, pendingErr.Pending)
			return
		}
		app.log("codex.turn.fail", "msg", messageID, "key", route.sessionKey, "dur", elapsed(turnStart), "total", elapsed(taskStart), "err", err)
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 运行失败", err)
		return
	}

	app.log("codex.turn.ok", "msg", messageID, "key", route.sessionKey, "thread", result.ThreadID, "turn", result.TurnID, "text_len", len(result.Text), "dur", elapsed(turnStart), "total", elapsed(taskStart))
	app.finishTaskSuccess(context.Background(), route, messageID, client, result)
}

func (app *daemon) ensureCodexThread(ctx context.Context, sessionKey string, client *codex.Client, threadID string, workDir string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID != "" {
		resumeStart := time.Now()
		app.log("codex.resume.start", "key", sessionKey, "thread", threadID, "workdir", workDir)
		resumedThreadID, err := client.ResumeThread(ctx, threadID, workDir)
		if err != nil {
			app.log("codex.resume.fail", "key", sessionKey, "thread", threadID, "dur", elapsed(resumeStart), "err", err)
			threadID = ""
		} else {
			app.setRuntimeThread(sessionKey, resumedThreadID)
			app.log("codex.resume.ok", "key", sessionKey, "thread", resumedThreadID, "dur", elapsed(resumeStart))
			return resumedThreadID, nil
		}
	}

	createStart := time.Now()
	app.log("codex.thread.create", "key", sessionKey, "workdir", workDir)
	newThreadID, err := client.StartThread(ctx, workDir, false)
	if err != nil {
		app.log("codex.thread.fail", "key", sessionKey, "workdir", workDir, "dur", elapsed(createStart), "err", err)
		return "", err
	}
	if err := app.saveSessionThread(sessionKey, newThreadID); err != nil {
		return "", err
	}
	app.log("codex.thread.ok", "key", sessionKey, "thread", newThreadID, "dur", elapsed(createStart))
	return newThreadID, nil
}

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
	taskCtx, cancel := context.WithCancel(ctx)
	runtime.status = statusRunning
	runtime.pending = nil
	runtime.cancel = cancel
	app.log("pending.resolve", "msg", messageID, "key", route.sessionKey, "decision", decision, "method", pending.Method, "turn", pending.TurnID)
	app.mu.Unlock()

	app.reactForRoute(ctx, route, messageID, reactionProcessing)
	go app.respondPendingTask(taskCtx, route, messageID, client, pending, decision)
	return nil
}

func (app *daemon) respondPendingTask(ctx context.Context, route messageRoute, messageID string, client *codex.Client, pending codex.PendingRequest, decision string) {
	start := time.Now()
	result, err := client.RespondToPending(ctx, pending, decision)
	if err != nil {
		var pendingErr *codex.PendingRequestError
		if errors.As(err, &pendingErr) {
			app.log("pending.next", "msg", messageID, "key", route.sessionKey, "method", pendingErr.Pending.Method, "dur", elapsed(start))
			app.pauseForPending(context.Background(), route, messageID, client, pendingErr.Pending)
			return
		}
		app.log("pending.respond.fail", "msg", messageID, "key", route.sessionKey, "decision", decision, "dur", elapsed(start), "err", err)
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 继续运行失败", err)
		return
	}
	app.log("pending.respond.ok", "msg", messageID, "key", route.sessionKey, "decision", decision, "thread", result.ThreadID, "turn", result.TurnID, "dur", elapsed(start))
	app.finishTaskSuccess(context.Background(), route, messageID, client, result)
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
		app.log("session.reset.fail", "msg", messageID, "key", route.sessionKey, "old_thread", oldThreadID, "err", err)
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
		return app.respond(ctx, messageID, route, "已请求取消当前 Codex 任务。")
	default:
		app.mu.Unlock()
		return app.respond(ctx, messageID, route, "当前会话没有运行中的 Codex 任务。")
	}
}

func (app *daemon) pauseForPending(ctx context.Context, route messageRoute, messageID string, client *codex.Client, pending codex.PendingRequest) {
	pendingCopy := pending
	var cancel context.CancelFunc
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	cancel = runtime.cancel
	runtime.status = statusPending
	runtime.client = client
	runtime.cancel = nil
	runtime.pending = &pendingCopy
	app.log("runtime.status", "key", route.sessionKey, "from", statusRunning, "to", statusPending, "pending", pending.Method, "turn", pending.TurnID, "item", pending.ItemID)
	app.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	_ = app.respond(ctx, messageID, route, "Codex 需要你确认：\n\n"+pending.Summary+"\n\n回复 /approve 允许，/deny 拒绝，或 /cancel 取消。")
}

func (app *daemon) finishTaskSuccess(ctx context.Context, route messageRoute, messageID string, client *codex.Client, result codex.TurnResult) {
	app.log("codex.task.ok", "msg", messageID, "key", route.sessionKey, "thread", result.ThreadID, "turn", result.TurnID, "text_len", len(result.Text))
	app.clearRuntimeClient(route.sessionKey, client)
	_ = client.Close()
	if err := app.replyTurnResult(ctx, messageID, route, result); err != nil {
		app.log("result.reply.fail", "msg", messageID, "key", route.sessionKey, "err", err)
	}
}

func (app *daemon) finishTaskError(ctx context.Context, route messageRoute, messageID string, client *codex.Client, prefix string, err error) {
	app.log("codex.task.fail", "msg", messageID, "key", route.sessionKey, "prefix", prefix, "err", err)
	app.clearRuntimeClient(route.sessionKey, client)
	if client != nil {
		_ = client.Close()
	}

	app.reactForRoute(ctx, route, messageID, reactionError)
	message := prefix + "：" + err.Error()
	if errors.Is(err, context.Canceled) {
		message = "Codex 任务已取消。"
	}
	if replyErr := app.respond(ctx, messageID, route, message); replyErr != nil {
		app.log("error.reply.fail", "msg", messageID, "key", route.sessionKey, "err", replyErr)
	}
}

func (app *daemon) attachClient(sessionKey string, client *codex.Client) {
	app.mu.Lock()
	defer app.mu.Unlock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	if runtime.status == statusRunning {
		runtime.client = client
	}
}

func (app *daemon) clearRuntimeClient(sessionKey string, client *codex.Client) {
	var cancel context.CancelFunc
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(sessionKey)
	if runtime.client != nil && runtime.client != client {
		app.log("runtime.clear.skip", "key", sessionKey, "reason", "client_changed")
		app.mu.Unlock()
		return
	}
	previous := runtime.status
	cancel = runtime.cancel
	runtime.status = statusIdle
	runtime.client = nil
	runtime.cancel = nil
	runtime.pending = nil
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

func (app *daemon) ensureRuntimeLocked(sessionKey string) *sessionRuntime {
	session := app.store.EnsureSession(sessionKey, app.cfg.DefaultWorkDir)
	workDir := strings.TrimSpace(session.WorkDir)
	if workDir == "" {
		workDir = app.cfg.DefaultWorkDir
	}

	runtime := app.sessions[sessionKey]
	if runtime == nil {
		runtime = &sessionRuntime{
			key:      sessionKey,
			status:   statusIdle,
			threadID: strings.TrimSpace(session.CodexThreadID),
			workDir:  workDir,
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
	return runtime
}

func (app *daemon) replyTurnResult(ctx context.Context, messageID string, route messageRoute, result codex.TurnResult) error {
	answer := strings.TrimSpace(result.Text)
	if answer == "" {
		answer = "Codex 没有返回文本。"
	}
	if err := app.respond(ctx, messageID, route, answer); err != nil {
		app.reactForRoute(ctx, route, messageID, reactionError)
		return err
	}
	app.reactForRoute(ctx, route, messageID, reactionDone)
	return nil
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
	fmt.Fprintf(&builder, "current_kind: %s\ncurrent_key: %s\ncurrent_status: %s\ncurrent_codex_thread: %s\ncurrent_workdir: %s\ncurrent_pending: %s\n", route.kind, route.sessionKey, current.status, current.threadID, current.workDir, current.pending)
	fmt.Fprintf(&builder, "running: %d\npending: %d\nsessions: %d\ntasks: %d\nstate: %s\n", running, pending, len(app.store.Sessions), len(taskKeys), app.cfg.StatePath)
	fmt.Fprintf(&builder, "chat: status=%s codex_thread=%s workdir=%s\n", chat.status, chat.threadID, chat.workDir)

	if len(taskKeys) == 0 {
		builder.WriteString("task_list: none")
		return builder.String()
	}
	builder.WriteString("task_list:")
	for _, key := range taskKeys {
		snapshot := app.sessionStatusSnapshotLocked(key)
		fmt.Fprintf(&builder, "\n- key=%s status=%s codex_thread=%s workdir=%s pending=%s", key, snapshot.status, snapshot.threadID, snapshot.workDir, snapshot.pending)
	}
	return builder.String()
}

type sessionStatusSnapshot struct {
	status   sessionStatus
	threadID string
	workDir  string
	pending  string
}

func (app *daemon) sessionStatusSnapshotLocked(sessionKey string) sessionStatusSnapshot {
	session := app.store.EnsureSession(sessionKey, app.cfg.DefaultWorkDir)
	snapshot := sessionStatusSnapshot{
		status:   statusIdle,
		threadID: strings.TrimSpace(session.CodexThreadID),
		workDir:  strings.TrimSpace(session.WorkDir),
		pending:  "none",
	}
	if snapshot.workDir == "" {
		snapshot.workDir = app.cfg.DefaultWorkDir
	}
	if runtime := app.sessions[sessionKey]; runtime != nil {
		snapshot.status = runtime.status
		if runtime.threadID != "" {
			snapshot.threadID = runtime.threadID
		}
		if runtime.workDir != "" {
			snapshot.workDir = runtime.workDir
		}
		if runtime.pending != nil {
			snapshot.pending = runtime.pending.Method
		}
	}
	return snapshot
}

func (app *daemon) reactForRoute(ctx context.Context, route messageRoute, messageID string, emoji string) {
	if route.kind != kindTask {
		app.log("lark.react.skip", "kind", route.kind, "key", route.sessionKey, "msg", messageID, "emoji", emoji)
		return
	}
	go app.react(ctx, messageID, emoji)
}

func (app *daemon) react(ctx context.Context, messageID string, emoji string) {
	start := time.Now()
	if err := app.lark.AddReaction(ctx, messageID, emoji); err != nil {
		app.log("lark.react.fail", "msg", messageID, "emoji", emoji, "dur", elapsed(start), "err", err)
		return
	}
	app.log("lark.react.ok", "msg", messageID, "emoji", emoji, "dur", elapsed(start))
}

func (app *daemon) respond(ctx context.Context, messageID string, route messageRoute, text string) error {
	if route.kind == kindChat {
		start := time.Now()
		responseID, err := app.lark.SendText(ctx, app.cfg.OwnerOpenID, text)
		if err != nil {
			app.log("lark.reply.fail", "mode", "direct", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return err
		}
		app.log("lark.reply.ok", "mode", "direct", "msg", messageID, "reply", responseID, "key", route.sessionKey, "text_len", len(text), "dur", elapsed(start))
		return nil
	}
	return app.replyThread(ctx, messageID, text)
}

func (app *daemon) replyThread(ctx context.Context, messageID string, text string) error {
	start := time.Now()
	if responseID, err := app.lark.ReplyText(ctx, messageID, text, true); err == nil {
		app.log("lark.reply.ok", "mode", "thread", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(start))
		return nil
	} else {
		app.log("lark.reply.fail", "mode", "thread", "msg", messageID, "dur", elapsed(start), "err", err)
	}
	fallbackStart := time.Now()
	responseID, err := app.lark.ReplyText(ctx, messageID, text, false)
	if err != nil {
		app.log("lark.reply.fail", "mode", "fallback", "msg", messageID, "dur", elapsed(fallbackStart), "err", err)
		return err
	}
	app.log("lark.reply.ok", "mode", "fallback", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(fallbackStart), "total", elapsed(start))
	return nil
}
