package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
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

type logLevel int

const (
	levelDebug logLevel = iota
	levelInfo
	levelError
)

func parseLogLevel(value string) logLevel {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return levelDebug
	case "error":
		return levelError
	default:
		return levelInfo
	}
}

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
	model    string
	effort   string
	tier     string
	client   *codex.Client
	cancel   context.CancelFunc
	pending  *codex.PendingRequest
}

type codexModelConfig struct {
	model  string
	effort string
	tier   string
}

type daemon struct {
	cfg    configpkg.Config
	store  statepkg.State
	lark   lark.Client
	logger *log.Logger
	level  logLevel
	seen   map[string]struct{}

	mu       sync.Mutex
	sessions map[string]*sessionRuntime
}

func (app *daemon) log(event string, fields ...any) {
	app.logAt(levelInfo, event, fields...)
}

func (app *daemon) debug(event string, fields ...any) {
	app.logAt(levelDebug, event, fields...)
}

func (app *daemon) logError(event string, fields ...any) {
	app.logAt(levelError, event, fields...)
}

func (app *daemon) logAt(level logLevel, event string, fields ...any) {
	if level < app.level {
		return
	}
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

func firstString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
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
	checkConfig := flag.Bool("check-config", false, "validate config and exit")
	healthOnce := flag.Bool("health-once", false, "send one health check message and exit")
	flag.Parse()

	logger := log.New(os.Stdout, "lark-bridge ", log.LstdFlags|log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *checkConfig {
		if err := runConfigCheck(*configPath, logger); err != nil {
			logger.Fatal(err)
		}
		return
	}

	if *healthOnce {
		if err := runHealthOnce(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Fatal(err)
		}
		return
	}

	if err := run(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal(err)
	}
}

func runConfigCheck(configPath string, logger *log.Logger) error {
	cfg, err := configpkg.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if err := checkDir("default_work_dir", cfg.DefaultWorkDir); err != nil {
		return err
	}
	for name, path := range cfg.WorkDirs {
		if err := checkDir("work_dirs."+name, path); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath(cfg.CodexCLI); err != nil {
		return fmt.Errorf("codex_cli %q not found in PATH", cfg.CodexCLI)
	}
	logger.Printf("%-20s %s", "config.ok", formatLogFields("path", configPath, "workdirs", len(cfg.WorkDirs), "codex_cli", cfg.CodexCLI, "task_model", cfg.DefaultTaskModel, "chat_model", cfg.DefaultChatModel))
	return nil
}

func checkDir(name string, raw string) error {
	path := strings.TrimSpace(raw)
	if path == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve %s home: %w", name, err)
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
			return fmt.Errorf("resolve %s %q: %w", name, raw, err)
		}
		path = absolute
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s is not accessible: %s", name, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %s", name, path)
	}
	return nil
}

func runHealthOnce(ctx context.Context, configPath string, logger *log.Logger) error {
	cfg, err := configpkg.LoadConfig(configPath)
	if err != nil {
		return err
	}

	larkClient := lark.NewClient(cfg.LarkAppID, cfg.LarkAppSecret)
	botName := "lark-bridge"
	if name, err := larkClient.BotName(ctx); err == nil {
		botName = name
	} else {
		logger.Printf("%-20s %s", "health.bot_name.fail", formatLogFields("err", err))
	}

	healthCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	answer, codexErr := codexHealthCheck(healthCtx, cfg)

	text := fmt.Sprintf("早安，%s在线。", botName)
	if answer != "" {
		text += "\n" + answer
	}
	if codexErr != nil {
		text += "\nCodex healthcheck failed: " + codexErr.Error()
	}

	if _, err := larkClient.SendText(ctx, cfg.OwnerOpenID, text); err != nil {
		return err
	}
	if codexErr != nil {
		return codexErr
	}
	return nil
}

func codexHealthCheck(ctx context.Context, cfg configpkg.Config) (string, error) {
	client := codex.NewClient(cfg.CodexCLI, cfg.DefaultChatModel, cfg.DefaultChatEffort, cfg.DefaultChatServiceTier)
	if err := client.Start(ctx); err != nil {
		return "", err
	}
	defer client.Close()

	threadID, err := client.StartThread(ctx, cfg.DefaultWorkDir, true)
	if err != nil {
		return "", err
	}
	result, err := client.RunTurn(ctx, threadID, cfg.DefaultWorkDir, "请用一句不超过20个字的中文确认 lark-bridge 健康检查正常。", nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Text), nil
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

	consumer, err := lark.StartEventConsumer(ctx, cfg.LarkAppID, cfg.LarkAppSecret)
	if err != nil {
		return err
	}
	defer consumer.Close()

	app := &daemon{
		cfg:      cfg,
		store:    store,
		lark:     lark.NewClient(cfg.LarkAppID, cfg.LarkAppSecret),
		logger:   logger,
		level:    parseLogLevel(cfg.LogLevel),
		seen:     make(map[string]struct{}),
		sessions: make(map[string]*sessionRuntime),
	}
	defer app.shutdown()

	app.logStateSummary()
	app.log("daemon.online", "owner", cfg.OwnerOpenID, "sessions", len(store.Sessions), "projects", len(cfg.WorkDirs), "state", cfg.StatePath, "workdir", cfg.DefaultWorkDir, "task_model", cfg.DefaultTaskModel, "task_effort", cfg.DefaultTaskEffort, "task_tier", cfg.DefaultTaskServiceTier, "chat_model", cfg.DefaultChatModel, "chat_effort", cfg.DefaultChatEffort, "chat_tier", cfg.DefaultChatServiceTier, "lark_app", cfg.LarkAppID, "codex_cli", cfg.CodexCLI, "log_level", cfg.LogLevel)
	botName := "lark-bridge"
	if name, err := app.lark.BotName(ctx); err != nil {
		app.logError("lark.bot_name.failed", "err", err)
	} else {
		botName = name
	}
	onlineText := fmt.Sprintf("早安，%s上线啦", botName)
	onlineStart := time.Now()
	if _, err := app.lark.SendText(ctx, cfg.OwnerOpenID, onlineText); err != nil {
		app.logError("lark.send.failed", "target", "owner", "dur", elapsed(onlineStart), "err", err)
	} else {
		app.debug("lark.send.ok", "target", "owner", "dur", elapsed(onlineStart), "text_len", len(onlineText))
	}

	for {
		event, err := consumer.Receive(ctx)
		if err != nil {
			return err
		}
		switch event.Kind {
		case lark.EventKindMessage:
			if err := app.handleEvent(ctx, event.Message); err != nil {
				app.logError("event.failed", "msg", event.Message.MessageID, "err", err)
			}
		case lark.EventKindCardAction:
			if err := app.handleCardAction(ctx, event.CardAction); err != nil {
				app.logError("card.failed", "msg", event.CardAction.MessageID, "action", event.CardAction.Action, "err", err)
			}
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
		defaults := app.defaultModelConfig(key)
		app.log("state.session", "key", key, "has_thread", hasThread, "workdir", session.WorkDir, "model", firstString(session.Model, defaults.model), "effort", firstString(session.ReasoningEffort, defaults.effort), "tier", firstString(session.ServiceTier, defaults.tier))
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
		app.debug("event.ignore", "reason", "non_text", "event", event.EventID, "msg", event.MessageID, "type", event.MessageType, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if event.MessageID == "" {
		app.debug("event.ignore", "reason", "missing_msg", "event", event.EventID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}
	if _, ok := app.seen[event.MessageID]; ok {
		app.debug("event.ignore", "reason", "duplicate", "event", event.EventID, "msg", event.MessageID, "total", elapsed(start))
		return nil
	}
	app.seen[event.MessageID] = struct{}{}

	text := strings.TrimSpace(event.Content)
	if text == "" {
		app.debug("event.ignore", "reason", "empty_text", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "total", elapsed(start))
		return nil
	}

	app.debug("event.accept", "event", event.EventID, "msg", event.MessageID, "chat", event.ChatID, "chat_type", event.ChatType, "text_len", len(text), "create_time", event.CreateTime, "event_ts", event.Timestamp, "bus_lag", eventLag(event.CreateTime, event.Timestamp), "recv_lag", recvLag(event.CreateTime, start))
	routeStart := time.Now()
	route := app.eventRoute(ctx, event)
	app.debug("event.route", "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey, "reason", route.reason, "dur", elapsed(routeStart))

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
	app.debug("event.done", "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey, "command", command.Name, "status", status, "dispatch", elapsed(dispatchStart), "total", elapsed(start), "err", err)
	return err
}

func (app *daemon) eventRoute(ctx context.Context, event lark.MessageEvent) messageRoute {
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

func (app *daemon) handleCommand(ctx context.Context, event lark.MessageEvent, route messageRoute, command bridge.Command) error {
	app.log("command.recv", "name", command.Name, "arg_len", len(command.Arg), "msg", event.MessageID, "kind", route.kind, "key", route.sessionKey)
	app.reactForRoute(ctx, route, event.MessageID, reactionCommand)
	switch command.Name {
	case bridge.CommandHelp:
		return app.respond(ctx, event.MessageID, route, "支持：/create [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast] [任务], /attach THREAD_ID [--project NAME | --cwd DIR] [--model MODEL] [--effort ...] [--tier fast], /projects, /sessions, /status, /reset, /approve, /deny, /cancel")
	case bridge.CommandCreate:
		return app.handleCreateTask(ctx, event, route, command.Arg)
	case bridge.CommandAttach:
		return app.handleAttach(ctx, event, route, command.Arg)
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
		return app.respond(ctx, event.MessageID, route, "未知命令。当前支持：/create, /attach, /projects, /sessions, /status, /reset, /approve, /deny, /cancel")
	default:
		return app.respond(ctx, event.MessageID, route, fmt.Sprintf("/%s 还没实现", command.Name))
	}
}

type createRequest struct {
	workDir string
	project string
	model   string
	effort  string
	tier    string
	prompt  string
}

type attachRequest struct {
	threadID string
	workDir  string
	project  string
	model    string
	effort   string
	tier     string
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
		case field == "--project":
			index++
			if index >= len(fields) {
				return createRequest{}, fmt.Errorf("%s 需要一个项目名", field)
			}
			workDir, err := app.resolveProjectWorkDir(fields[index])
			if err != nil {
				return createRequest{}, err
			}
			request.project = strings.TrimSpace(fields[index])
			request.workDir = workDir
		case strings.HasPrefix(field, "--project="):
			project := strings.TrimSpace(strings.TrimPrefix(field, "--project="))
			workDir, err := app.resolveProjectWorkDir(project)
			if err != nil {
				return createRequest{}, err
			}
			request.project = project
			request.workDir = workDir
		case field == "--model":
			index++
			if index >= len(fields) {
				return createRequest{}, fmt.Errorf("%s 需要一个模型参数", field)
			}
			request.model = strings.TrimSpace(fields[index])
			if request.model == "" {
				return createRequest{}, fmt.Errorf("model 不能为空")
			}
		case strings.HasPrefix(field, "--model="):
			request.model = strings.TrimSpace(strings.TrimPrefix(field, "--model="))
			if request.model == "" {
				return createRequest{}, fmt.Errorf("model 不能为空")
			}
		case field == "--effort" || field == "--reasoning-effort":
			index++
			if index >= len(fields) {
				return createRequest{}, fmt.Errorf("%s 需要一个 effort 参数", field)
			}
			effort, err := normalizeEffort(fields[index])
			if err != nil {
				return createRequest{}, err
			}
			request.effort = effort
		case strings.HasPrefix(field, "--effort="):
			effort, err := normalizeEffort(strings.TrimPrefix(field, "--effort="))
			if err != nil {
				return createRequest{}, err
			}
			request.effort = effort
		case strings.HasPrefix(field, "--reasoning-effort="):
			effort, err := normalizeEffort(strings.TrimPrefix(field, "--reasoning-effort="))
			if err != nil {
				return createRequest{}, err
			}
			request.effort = effort
		case field == "--tier" || field == "--service-tier":
			index++
			if index >= len(fields) {
				return createRequest{}, fmt.Errorf("%s 需要一个 tier 参数", field)
			}
			tier, err := normalizeServiceTier(fields[index])
			if err != nil {
				return createRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--tier="):
			tier, err := normalizeServiceTier(strings.TrimPrefix(field, "--tier="))
			if err != nil {
				return createRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--service-tier="):
			tier, err := normalizeServiceTier(strings.TrimPrefix(field, "--service-tier="))
			if err != nil {
				return createRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--"):
			return createRequest{}, fmt.Errorf("未知 /create 参数：%s", field)
		default:
			promptFields = append(promptFields, field)
		}
	}

	request.prompt = strings.TrimSpace(strings.Join(promptFields, " "))
	return request, nil
}

func (app *daemon) parseAttachRequest(arg string) (attachRequest, error) {
	fields := strings.Fields(strings.TrimSpace(arg))
	request := attachRequest{}

	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch {
		case field == "--cwd" || field == "--workdir":
			index++
			if index >= len(fields) {
				return attachRequest{}, fmt.Errorf("%s 需要一个目录参数", field)
			}
			workDir, err := app.resolveWorkDir(fields[index])
			if err != nil {
				return attachRequest{}, err
			}
			request.workDir = workDir
		case strings.HasPrefix(field, "--cwd="):
			workDir, err := app.resolveWorkDir(strings.TrimPrefix(field, "--cwd="))
			if err != nil {
				return attachRequest{}, err
			}
			request.workDir = workDir
		case strings.HasPrefix(field, "--workdir="):
			workDir, err := app.resolveWorkDir(strings.TrimPrefix(field, "--workdir="))
			if err != nil {
				return attachRequest{}, err
			}
			request.workDir = workDir
		case field == "--project":
			index++
			if index >= len(fields) {
				return attachRequest{}, fmt.Errorf("%s 需要一个项目名", field)
			}
			workDir, err := app.resolveProjectWorkDir(fields[index])
			if err != nil {
				return attachRequest{}, err
			}
			request.project = strings.TrimSpace(fields[index])
			request.workDir = workDir
		case strings.HasPrefix(field, "--project="):
			project := strings.TrimSpace(strings.TrimPrefix(field, "--project="))
			workDir, err := app.resolveProjectWorkDir(project)
			if err != nil {
				return attachRequest{}, err
			}
			request.project = project
			request.workDir = workDir
		case field == "--model":
			index++
			if index >= len(fields) {
				return attachRequest{}, fmt.Errorf("%s 需要一个模型参数", field)
			}
			request.model = strings.TrimSpace(fields[index])
			if request.model == "" {
				return attachRequest{}, fmt.Errorf("model 不能为空")
			}
		case strings.HasPrefix(field, "--model="):
			request.model = strings.TrimSpace(strings.TrimPrefix(field, "--model="))
			if request.model == "" {
				return attachRequest{}, fmt.Errorf("model 不能为空")
			}
		case field == "--effort" || field == "--reasoning-effort":
			index++
			if index >= len(fields) {
				return attachRequest{}, fmt.Errorf("%s 需要一个 effort 参数", field)
			}
			effort, err := normalizeEffort(fields[index])
			if err != nil {
				return attachRequest{}, err
			}
			request.effort = effort
		case strings.HasPrefix(field, "--effort="):
			effort, err := normalizeEffort(strings.TrimPrefix(field, "--effort="))
			if err != nil {
				return attachRequest{}, err
			}
			request.effort = effort
		case strings.HasPrefix(field, "--reasoning-effort="):
			effort, err := normalizeEffort(strings.TrimPrefix(field, "--reasoning-effort="))
			if err != nil {
				return attachRequest{}, err
			}
			request.effort = effort
		case field == "--tier" || field == "--service-tier":
			index++
			if index >= len(fields) {
				return attachRequest{}, fmt.Errorf("%s 需要一个 tier 参数", field)
			}
			tier, err := normalizeServiceTier(fields[index])
			if err != nil {
				return attachRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--tier="):
			tier, err := normalizeServiceTier(strings.TrimPrefix(field, "--tier="))
			if err != nil {
				return attachRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--service-tier="):
			tier, err := normalizeServiceTier(strings.TrimPrefix(field, "--service-tier="))
			if err != nil {
				return attachRequest{}, err
			}
			request.tier = tier
		case strings.HasPrefix(field, "--"):
			return attachRequest{}, fmt.Errorf("未知 /attach 参数：%s", field)
		default:
			if request.threadID != "" {
				return attachRequest{}, fmt.Errorf("/attach 只接受一个 Codex thread id")
			}
			request.threadID = strings.TrimSpace(field)
		}
	}

	if request.threadID == "" {
		return attachRequest{}, fmt.Errorf("用法：/attach CODEX_THREAD_ID [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast]")
	}
	return request, nil
}

func normalizeEffort(raw string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(raw))
	switch effort {
	case "low", "medium", "high", "xhigh":
		return effort, nil
	default:
		return "", fmt.Errorf("effort 必须是 low、medium、high 或 xhigh")
	}
}

func normalizeServiceTier(raw string) (string, error) {
	tier := strings.ToLower(strings.TrimSpace(raw))
	if tier == "" {
		return "", fmt.Errorf("tier 不能为空")
	}
	return tier, nil
}

func (app *daemon) resolveProjectWorkDir(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("project 不能为空")
	}
	path, ok := app.cfg.WorkDirs[name]
	if !ok {
		for candidate, candidatePath := range app.cfg.WorkDirs {
			if strings.EqualFold(candidate, name) {
				name = candidate
				path = candidatePath
				ok = true
				break
			}
		}
	}
	if !ok {
		projects := app.projectNames()
		if len(projects) == 0 {
			return "", fmt.Errorf("未配置 work_dirs，不能使用 --project")
		}
		return "", fmt.Errorf("未知 project：%s。可用：%s", name, strings.Join(projects, ", "))
	}
	workDir, err := app.resolveWorkDir(path)
	if err != nil {
		return "", fmt.Errorf("project %s 指向的 workdir 无效：%w", name, err)
	}
	return workDir, nil
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
	app.debug("task.create.parsed", "msg", event.MessageID, "project", request.project, "workdir", request.workDir, "model", request.model, "effort", request.effort, "tier", request.tier, "prompt_len", len(request.prompt), "dur", elapsed(parseStart))

	taskDefaults := app.defaultModelConfig("thread:default")
	message := "Task 已创建。请在这个话题里继续发送任务。"
	if request.prompt != "" {
		message = "Task 已创建，开始处理。"
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

	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(taskRoute.sessionKey)
	if runtime.status == statusRunning || runtime.status == statusPending {
		status := runtime.status
		app.mu.Unlock()
		return app.respond(ctx, event.MessageID, taskRoute, fmt.Sprintf("当前会话状态是 %s，不能 attach。", status))
	}

	if request.workDir != "" {
		runtime.workDir = request.workDir
		app.store.SetSessionWorkDir(taskRoute.sessionKey, request.workDir)
	}
	runtime.threadID = request.threadID
	defaults := app.defaultModelConfig(taskRoute.sessionKey)
	runtime.model = firstString(request.model, runtime.model, defaults.model)
	runtime.effort = firstString(request.effort, runtime.effort, defaults.effort)
	runtime.tier = firstString(request.tier, runtime.tier, defaults.tier)
	app.store.SetSessionThread(taskRoute.sessionKey, runtime.threadID)
	app.store.SetSessionModelConfig(taskRoute.sessionKey, runtime.model, runtime.effort, runtime.tier)
	workDir := runtime.workDir
	model := runtime.model
	effort := runtime.effort
	tier := runtime.tier
	saveErr := app.store.Save(app.cfg.StatePath)
	app.mu.Unlock()
	if saveErr != nil {
		app.logError("session.attach.save.fail", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "err", saveErr)
		return saveErr
	}

	app.log("session.attach.ok", "msg", event.MessageID, "key", taskRoute.sessionKey, "thread", request.threadID, "project", request.project, "workdir", workDir, "model", model, "effort", effort, "tier", tier)
	projectLine := ""
	if request.project != "" {
		projectLine = "\nproject: " + request.project
	}
	return app.respond(ctx, event.MessageID, taskRoute, fmt.Sprintf("已 attach 到 Codex session。\nkey: %s\ncodex_thread: %s\nworkdir: %s%s\nmodel: %s\neffort: %s\ntier: %s", taskRoute.sessionKey, request.threadID, workDir, projectLine, model, effort, tier))
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

func (app *daemon) handlePrompt(ctx context.Context, event lark.MessageEvent, route messageRoute, text string) error {
	return app.startPromptTask(ctx, route, event.MessageID, text)
}

func (app *daemon) startPromptTask(ctx context.Context, route messageRoute, messageID string, text string) error {
	start := time.Now()
	app.mu.Lock()
	runtime := app.ensureRuntimeLocked(route.sessionKey)
	app.debug("prompt.dispatch", "msg", messageID, "kind", route.kind, "key", route.sessionKey, "status", runtime.status, "thread", runtime.threadID, "workdir", runtime.workDir, "text_len", len(text))
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
	app.log("runtime.status", "key", route.sessionKey, "from", statusIdle, "to", statusRunning)
	app.mu.Unlock()

	statusMessageID := ""
	if route.kind == kindTask {
		card := app.taskCard("Codex 正在处理", "running", fmt.Sprintf("已收到任务，正在准备 Codex。\n\nmodel: %s\neffort: %s\ntier: %s", model, effort, tier), route, workDir, app.runningActions(route))
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
		card := app.taskCard("Codex 正在处理", "running", "Codex 已启动，正在准备 session。", route, workDir, app.runningActions(route))
		_ = app.replyOrPatchCard(context.Background(), messageID, statusMessageID, route, card)
	}
	app.attachClient(route.sessionKey, client)

	threadStart := time.Now()
	activeThreadID, err := app.ensureCodexThread(ctx, route.sessionKey, client, threadID, workDir)
	if err != nil {
		app.finishTaskError(context.Background(), route, messageID, client, "Codex 创建 session 失败", err, statusMessageID)
		return
	}

	app.debug("codex.thread.ready", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "dur", elapsed(threadStart))
	if statusMessageID != "" {
		card := app.taskCard("Codex 正在运行", "running", "Codex session 已就绪，正在运行任务。", route, workDir, app.runningActions(route))
		_ = app.replyOrPatchCard(context.Background(), messageID, statusMessageID, route, card)
	}
	turnStart := time.Now()
	app.debug("codex.turn.start", "msg", messageID, "key", route.sessionKey, "thread", activeThreadID, "text_len", len(text))
	onUpdate, stopUpdates := app.liveCardUpdater(ctx, route, messageID, statusMessageID, workDir)
	result, err := client.RunTurn(ctx, activeThreadID, workDir, text, onUpdate)
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

func (app *daemon) ensureCodexThread(ctx context.Context, sessionKey string, client *codex.Client, threadID string, workDir string) (string, error) {
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
			return resumedThreadID, nil
		}
	}

	createStart := time.Now()
	app.debug("codex.thread.create", "key", sessionKey, "workdir", workDir)
	newThreadID, err := client.StartThread(ctx, workDir, false)
	if err != nil {
		app.logError("codex.thread.fail", "key", sessionKey, "workdir", workDir, "dur", elapsed(createStart), "err", err)
		return "", err
	}
	if err := app.saveSessionThread(sessionKey, newThreadID); err != nil {
		return "", err
	}
	app.debug("codex.thread.ok", "key", sessionKey, "thread", newThreadID, "dur", elapsed(createStart))
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
	workDir := runtime.workDir
	taskCtx, cancel := context.WithCancel(ctx)
	runtime.status = statusRunning
	runtime.pending = nil
	runtime.cancel = cancel
	app.log("pending.resolve", "msg", messageID, "key", route.sessionKey, "decision", decision, "method", pending.Method, "turn", pending.TurnID)
	app.mu.Unlock()

	statusMessageID := cardStatusMessageID(route, messageID)
	if statusMessageID != "" {
		card := app.taskCard("Codex 继续运行", "running", "已收到确认，Codex 正在继续处理。", route, workDir, app.runningActions(route))
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
		workDir := runtime.workDir
		app.mu.Unlock()
		app.log("session.cancel", "state", "running", "msg", messageID, "key", route.sessionKey)
		if cancel != nil {
			cancel()
		}
		if route.reason == "card_action" {
			card := app.taskCard("Codex 正在取消", "running", "已请求取消当前 Codex 任务。", route, workDir, nil)
			if err := app.replyOrPatchCard(ctx, messageID, messageID, route, card); err != nil {
				app.logError("cancel.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
			}
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
	app.log("runtime.status", "key", route.sessionKey, "from", statusRunning, "to", statusPending, "pending", pending.Method, "turn", pending.TurnID, "item", pending.ItemID)
	app.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if route.kind == kindTask {
		body := "Codex 需要你确认：\n\n" + pending.Summary
		card := app.taskCard("Codex 等待确认", "pending", body, route, "", app.pendingActions(route, pending))
		if err := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); err != nil {
			app.logError("pending.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
		}
		return
	}
	_ = app.respond(ctx, messageID, route, "Codex 需要你确认：\n\n"+pending.Summary+"\n\n回复 /approve 允许，/deny 拒绝，或 /cancel 取消。")
}

func (app *daemon) finishTaskSuccess(ctx context.Context, route messageRoute, messageID string, client *codex.Client, result codex.TurnResult, statusMessageID ...string) {
	app.log("codex.task.ok", "msg", messageID, "key", route.sessionKey, "thread", result.ThreadID, "turn", result.TurnID, "text_len", len(result.Text))
	app.clearRuntimeClient(route.sessionKey, client)
	_ = client.Close()
	if err := app.replyTurnResult(ctx, messageID, route, result, firstString(statusMessageID...)); err != nil {
		app.logError("result.reply.fail", "msg", messageID, "key", route.sessionKey, "err", err)
	}
}

func (app *daemon) finishTaskError(ctx context.Context, route messageRoute, messageID string, client *codex.Client, prefix string, err error, statusMessageID ...string) {
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
		if replyErr := app.replyOrPatchCard(ctx, messageID, firstString(statusMessageID...), route, card); replyErr != nil {
			app.logError("error.card.fail", "msg", messageID, "key", route.sessionKey, "err", replyErr)
		}
		return
	}
	if replyErr := app.respond(ctx, messageID, route, message); replyErr != nil {
		app.logError("error.reply.fail", "msg", messageID, "key", route.sessionKey, "err", replyErr)
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

func (app *daemon) replyTurnResult(ctx context.Context, messageID string, route messageRoute, result codex.TurnResult, statusMessageID string) error {
	answer := strings.TrimSpace(result.Text)
	if answer == "" {
		answer = "Codex 没有返回文本。"
	}
	if route.kind == kindTask {
		card := app.taskCard("Codex 完成", "success", answer, route, "", nil)
		if err := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); err != nil {
			app.reactForRoute(ctx, route, messageID, reactionError)
			return err
		}
		app.reactForRoute(ctx, route, messageID, reactionDone)
		return nil
	}
	if err := app.respond(ctx, messageID, route, answer); err != nil {
		app.reactForRoute(ctx, route, messageID, reactionError)
		return err
	}
	app.reactForRoute(ctx, route, messageID, reactionDone)
	return nil
}

func (app *daemon) taskCard(title string, status string, body string, route messageRoute, workDir string, actions []lark.CardAction) string {
	footerParts := []string{"session: " + route.sessionKey}
	if strings.TrimSpace(workDir) != "" {
		footerParts = append(footerParts, "workdir: "+workDir)
	}
	return lark.BuildStatusCard(lark.StatusCard{
		Title:   title,
		Status:  status,
		Body:    body,
		Footer:  strings.Join(footerParts, "\n"),
		Actions: actions,
	})
}

func (app *daemon) pendingActions(route messageRoute, pending codex.PendingRequest) []lark.CardAction {
	return []lark.CardAction{
		{Action: codex.DecisionApprove, Label: "Approve", Style: "primary", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
		{Action: codex.DecisionDeny, Label: "Deny", Style: "danger", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
		{Action: codex.DecisionCancel, Label: "Cancel", Style: "default", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
	}
}

func (app *daemon) runningActions(route messageRoute) []lark.CardAction {
	return []lark.CardAction{
		{Action: codex.DecisionCancel, Label: "Cancel", Style: "danger", SessionKey: route.sessionKey},
	}
}

func (app *daemon) liveCardUpdater(ctx context.Context, route messageRoute, messageID string, statusMessageID string, workDir string) (codex.TurnUpdateFunc, func()) {
	if route.kind != kindTask || strings.TrimSpace(statusMessageID) == "" {
		return nil, func() {}
	}
	updates := make(chan string, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	var stopOnce sync.Once

	go app.liveCardLoop(ctx, route, messageID, statusMessageID, workDir, updates, done, stopped)

	onUpdate := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		select {
		case updates <- text:
			return
		default:
		}
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- text:
		default:
		}
	}
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				app.logError("live.card.stop.timeout", "msg", messageID, "key", route.sessionKey)
			}
		})
	}
	return onUpdate, stop
}

func (app *daemon) liveCardLoop(ctx context.Context, route messageRoute, messageID string, statusMessageID string, workDir string, updates <-chan string, done <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	latest := ""
	dirty := false
	for {
		select {
		case text := <-updates:
			latest = text
			dirty = true
		case <-ticker.C:
			if !dirty || strings.TrimSpace(latest) == "" {
				continue
			}
			body := "Codex 正在生成回答，下面是当前内容。\n\n" + latest
			card := app.taskCard("Codex 正在运行", "running", body, route, workDir, app.runningActions(route))
			patchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := app.replyOrPatchCard(patchCtx, messageID, statusMessageID, route, card)
			cancel()
			if err != nil {
				app.logError("live.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
			}
			dirty = false
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (app *daemon) replyOrPatchCard(ctx context.Context, sourceMessageID string, statusMessageID string, route messageRoute, card string) error {
	if statusMessageID != "" {
		start := time.Now()
		if err := app.lark.PatchCard(ctx, statusMessageID, card); err != nil {
			app.logError("lark.card.patch.fail", "msg", statusMessageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return err
		}
		app.debug("lark.card.patch.ok", "msg", statusMessageID, "key", route.sessionKey, "dur", elapsed(start))
		return nil
	}
	_, err := app.replyCard(ctx, sourceMessageID, route, card)
	return err
}

func (app *daemon) replyCard(ctx context.Context, messageID string, route messageRoute, card string) (string, error) {
	start := time.Now()
	if route.kind == kindChat {
		responseID, err := app.lark.SendCard(ctx, app.cfg.OwnerOpenID, card)
		if err != nil {
			app.logError("lark.card.fail", "mode", "direct", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return "", err
		}
		app.debug("lark.card.ok", "mode", "direct", "msg", messageID, "reply", responseID, "key", route.sessionKey, "dur", elapsed(start))
		return responseID, nil
	}
	responseID, err := app.lark.ReplyCard(ctx, messageID, card, true)
	if err != nil {
		app.logError("lark.card.fail", "mode", "thread", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
		return "", err
	}
	app.debug("lark.card.ok", "mode", "thread", "msg", messageID, "reply", responseID, "key", route.sessionKey, "dur", elapsed(start))
	return responseID, nil
}

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
		fmt.Fprintf(&builder, "- key=%s%s status=%s codex_thread=%s workdir=%s model=%s effort=%s tier=%s pending=%s\n", key, current, snapshot.status, snapshot.threadID, snapshot.workDir, snapshot.model, snapshot.effort, snapshot.tier, snapshot.pending)
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
	fmt.Fprintf(&builder, "current_kind: %s\ncurrent_key: %s\ncurrent_status: %s\ncurrent_codex_thread: %s\ncurrent_workdir: %s\ncurrent_model: %s\ncurrent_effort: %s\ncurrent_tier: %s\ncurrent_pending: %s\n", route.kind, route.sessionKey, current.status, current.threadID, current.workDir, current.model, current.effort, current.tier, current.pending)
	fmt.Fprintf(&builder, "running: %d\npending: %d\nsessions: %d\ntasks: %d\nstate: %s\n", running, pending, len(app.store.Sessions), len(taskKeys), app.cfg.StatePath)
	fmt.Fprintf(&builder, "chat: status=%s codex_thread=%s workdir=%s model=%s effort=%s tier=%s\n", chat.status, chat.threadID, chat.workDir, chat.model, chat.effort, chat.tier)

	if len(taskKeys) == 0 {
		builder.WriteString("task_list: none")
		return builder.String()
	}
	builder.WriteString("task_list:")
	for _, key := range taskKeys {
		snapshot := app.sessionStatusSnapshotLocked(key)
		fmt.Fprintf(&builder, "\n- key=%s status=%s codex_thread=%s workdir=%s model=%s effort=%s tier=%s pending=%s", key, snapshot.status, snapshot.threadID, snapshot.workDir, snapshot.model, snapshot.effort, snapshot.tier, snapshot.pending)
	}
	return builder.String()
}

type sessionStatusSnapshot struct {
	status   sessionStatus
	threadID string
	workDir  string
	model    string
	effort   string
	tier     string
	pending  string
}

func (app *daemon) sessionStatusSnapshotLocked(sessionKey string) sessionStatusSnapshot {
	session := app.store.EnsureSession(sessionKey, app.cfg.DefaultWorkDir)
	defaults := app.defaultModelConfig(sessionKey)
	snapshot := sessionStatusSnapshot{
		status:   statusIdle,
		threadID: strings.TrimSpace(session.CodexThreadID),
		workDir:  strings.TrimSpace(session.WorkDir),
		model:    firstString(session.Model, defaults.model),
		effort:   firstString(session.ReasoningEffort, defaults.effort),
		tier:     firstString(session.ServiceTier, defaults.tier),
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
	}
	return snapshot
}

func (app *daemon) reactForRoute(ctx context.Context, route messageRoute, messageID string, emoji string) {
	if route.kind != kindTask {
		app.debug("lark.react.skip", "kind", route.kind, "key", route.sessionKey, "msg", messageID, "emoji", emoji)
		return
	}
	go app.react(ctx, messageID, emoji)
}

func (app *daemon) react(ctx context.Context, messageID string, emoji string) {
	start := time.Now()
	if err := app.lark.AddReaction(ctx, messageID, emoji); err != nil {
		app.logError("lark.react.fail", "msg", messageID, "emoji", emoji, "dur", elapsed(start), "err", err)
		return
	}
	app.debug("lark.react.ok", "msg", messageID, "emoji", emoji, "dur", elapsed(start))
}

func (app *daemon) respond(ctx context.Context, messageID string, route messageRoute, text string) error {
	if route.kind == kindChat {
		start := time.Now()
		responseID, err := app.lark.SendText(ctx, app.cfg.OwnerOpenID, text)
		if err != nil {
			app.logError("lark.reply.fail", "mode", "direct", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return err
		}
		app.debug("lark.reply.ok", "mode", "direct", "msg", messageID, "reply", responseID, "key", route.sessionKey, "text_len", len(text), "dur", elapsed(start))
		return nil
	}
	return app.replyThread(ctx, messageID, text)
}

func (app *daemon) replyThread(ctx context.Context, messageID string, text string) error {
	start := time.Now()
	if responseID, err := app.lark.ReplyText(ctx, messageID, text, true); err == nil {
		app.debug("lark.reply.ok", "mode", "thread", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(start))
		return nil
	} else {
		app.logError("lark.reply.fail", "mode", "thread", "msg", messageID, "dur", elapsed(start), "err", err)
	}
	fallbackStart := time.Now()
	responseID, err := app.lark.ReplyText(ctx, messageID, text, false)
	if err != nil {
		app.logError("lark.reply.fail", "mode", "fallback", "msg", messageID, "dur", elapsed(fallbackStart), "err", err)
		return err
	}
	app.debug("lark.reply.ok", "mode", "fallback", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(fallbackStart), "total", elapsed(start))
	return nil
}
