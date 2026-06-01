package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	configpkg "lark-bridge/internal/config"
	"lark-bridge/internal/lark"
)

func runLarkProbe(ctx context.Context, configPath string, logger *log.Logger) error {
	cfg, err := configpkg.LoadConfig(configPath)
	if err != nil {
		return err
	}
	client := lark.NewClient(cfg.LarkAppID, cfg.LarkAppSecret)

	botName, err := client.BotName(ctx)
	if err != nil {
		return err
	}
	logger.Printf("%-20s %s", "probe.bot.ok", formatLogFields("name", botName))

	textID, err := client.SendText(ctx, cfg.OwnerOpenID, "lark-bridge probe: text send ok")
	if err != nil {
		return err
	}
	logger.Printf("%-20s %s", "probe.text.ok", formatLogFields("msg", textID))

	if err := client.AddReaction(ctx, textID, reactionDone); err != nil {
		return err
	}
	logger.Printf("%-20s %s", "probe.react.ok", formatLogFields("msg", textID, "emoji", reactionDone))

	cardID, err := client.SendCard(ctx, cfg.OwnerOpenID, lark.BuildStatusCard(lark.StatusCard{
		Title:  "lark-bridge probe",
		Status: "running",
		Body:   "card send ok\n\n下一步测试 card patch。",
		Footer: "permission probe",
	}))
	if err != nil {
		return err
	}
	logger.Printf("%-20s %s", "probe.card.ok", formatLogFields("msg", cardID))

	if err := client.PatchCard(ctx, cardID, lark.BuildStatusCard(lark.StatusCard{
		Title:  "lark-bridge probe",
		Status: "success",
		Body:   "card patch ok\n\n基础飞书发送、表情、卡片更新权限正常。",
		Footer: "permission probe",
	})); err != nil {
		return err
	}
	logger.Printf("%-20s %s", "probe.patch.ok", formatLogFields("msg", cardID))
	return nil
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
	logger.Printf("%-20s %s", "config.ok", formatLogFields("path", configPath, "workdirs", len(cfg.WorkDirs), "codex_cli", cfg.CodexCLI, "task_model", cfg.DefaultTaskModel, "chat_model", cfg.DefaultChatModel, "chat_initial_prompt_len", len(cfg.ChatInitialPrompt)))
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

	text := bridge.PickPhrasef("health:"+botName, bridge.HealthGreetings, botName)
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
	prompt := bridge.PickPhrase(cfg.DefaultWorkDir, bridge.HealthCheckPrompts)
	result, err := client.RunTurn(ctx, threadID, cfg.DefaultWorkDir, prompt, nil, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Text), nil
}
