package main

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	configpkg "lark-bridge/internal/config"
	"lark-bridge/internal/lark"
	statepkg "lark-bridge/internal/state"
)

func run(ctx context.Context, configPath string, logger *log.Logger) error {
	cfg, err := configpkg.LoadConfig(configPath)
	if err != nil {
		return err
	}

	store, err := statepkg.Load(cfg.StatePath)
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
	app.log("daemon.online", "owner", cfg.OwnerOpenID, "sessions", len(store.Sessions), "projects", len(cfg.WorkDirs), "state", cfg.StatePath, "workdir", cfg.DefaultWorkDir, "task_model", cfg.DefaultTaskModel, "task_effort", cfg.DefaultTaskEffort, "task_tier", cfg.DefaultTaskServiceTier, "chat_model", cfg.DefaultChatModel, "chat_effort", cfg.DefaultChatEffort, "chat_tier", cfg.DefaultChatServiceTier, "chat_initial_prompt_len", len(cfg.ChatInitialPrompt), "lark_app", cfg.LarkAppID, "codex_cli", cfg.CodexCLI, "log_level", cfg.LogLevel)
	botName := "lark-bridge"
	if name, err := app.lark.BotName(ctx); err != nil {
		app.logError("lark.bot_name.failed", "err", err)
	} else {
		botName = name
	}
	onlineText := bridge.PickPhrasef(time.Now().Format("2006-01-02")+":"+botName, bridge.OnlineGreetings, botName)
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
