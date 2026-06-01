package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	OwnerOpenID            string            `json:"owner_open_id"`
	DefaultWorkDir         string            `json:"default_work_dir"`
	StatePath              string            `json:"state_path"`
	LarkAppID              string            `json:"lark_app_id"`
	LarkAppSecret          string            `json:"lark_app_secret"`
	CodexCLI               string            `json:"codex_cli"`
	WorkDirs               map[string]string `json:"work_dirs"`
	DefaultTaskModel       string            `json:"default_task_model"`
	DefaultTaskEffort      string            `json:"default_task_reasoning_effort"`
	DefaultTaskServiceTier string            `json:"default_task_service_tier"`
	DefaultChatModel       string            `json:"default_chat_model"`
	DefaultChatEffort      string            `json:"default_chat_reasoning_effort"`
	DefaultChatServiceTier string            `json:"default_chat_service_tier"`
	ChatInitialPrompt      string            `json:"chat_initial_prompt"`
	LogLevel               string            `json:"log_level"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	cfg.OwnerOpenID = strings.TrimSpace(cfg.OwnerOpenID)
	cfg.DefaultWorkDir = strings.TrimSpace(cfg.DefaultWorkDir)
	cfg.StatePath = strings.TrimSpace(cfg.StatePath)
	cfg.LarkAppID = firstString(cfg.LarkAppID, os.Getenv("LARK_APP_ID"))
	cfg.LarkAppSecret = firstString(cfg.LarkAppSecret, os.Getenv("LARK_APP_SECRET"))
	cfg.CodexCLI = strings.TrimSpace(cfg.CodexCLI)
	cfg.DefaultTaskModel = firstString(cfg.DefaultTaskModel, os.Getenv("LARK_BRIDGE_DEFAULT_TASK_MODEL"))
	cfg.DefaultTaskEffort = strings.ToLower(firstString(cfg.DefaultTaskEffort, os.Getenv("LARK_BRIDGE_DEFAULT_TASK_REASONING_EFFORT")))
	cfg.DefaultTaskServiceTier = strings.ToLower(firstString(cfg.DefaultTaskServiceTier, os.Getenv("LARK_BRIDGE_DEFAULT_TASK_SERVICE_TIER")))
	cfg.DefaultChatModel = firstString(cfg.DefaultChatModel, os.Getenv("LARK_BRIDGE_DEFAULT_CHAT_MODEL"))
	cfg.DefaultChatEffort = strings.ToLower(firstString(cfg.DefaultChatEffort, os.Getenv("LARK_BRIDGE_DEFAULT_CHAT_REASONING_EFFORT")))
	cfg.DefaultChatServiceTier = strings.ToLower(firstString(cfg.DefaultChatServiceTier, os.Getenv("LARK_BRIDGE_DEFAULT_CHAT_SERVICE_TIER")))
	cfg.ChatInitialPrompt = strings.TrimSpace(firstString(cfg.ChatInitialPrompt, os.Getenv("LARK_BRIDGE_CHAT_INITIAL_PROMPT")))
	cfg.LogLevel = strings.ToLower(firstString(cfg.LogLevel, os.Getenv("LARK_BRIDGE_LOG_LEVEL")))
	workDirs, err := cleanWorkDirs(cfg.WorkDirs)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkDirs = workDirs

	if cfg.OwnerOpenID == "" {
		return Config{}, fmt.Errorf("owner_open_id is required")
	}
	if cfg.DefaultWorkDir == "" {
		cfg.DefaultWorkDir = "."
	}
	if cfg.StatePath == "" {
		cfg.StatePath = "state.json"
	}
	if cfg.LarkAppID == "" {
		return Config{}, fmt.Errorf("lark_app_id is required")
	}
	if cfg.LarkAppSecret == "" {
		return Config{}, fmt.Errorf("lark_app_secret is required")
	}
	if cfg.CodexCLI == "" {
		cfg.CodexCLI = "codex"
	}
	if cfg.DefaultTaskModel == "" {
		cfg.DefaultTaskModel = "gpt-5.5"
	}
	if cfg.DefaultTaskEffort == "" {
		cfg.DefaultTaskEffort = "xhigh"
	}
	if cfg.DefaultTaskServiceTier == "" {
		cfg.DefaultTaskServiceTier = "fast"
	}
	if cfg.DefaultChatModel == "" {
		cfg.DefaultChatModel = "gpt-5.5"
	}
	if cfg.DefaultChatEffort == "" {
		cfg.DefaultChatEffort = "medium"
	}
	if cfg.DefaultChatServiceTier == "" {
		cfg.DefaultChatServiceTier = "fast"
	}
	if !validEffort(cfg.DefaultTaskEffort) {
		return Config{}, fmt.Errorf("default_task_reasoning_effort must be one of low, medium, high, xhigh")
	}
	if !validEffort(cfg.DefaultChatEffort) {
		return Config{}, fmt.Errorf("default_chat_reasoning_effort must be one of low, medium, high, xhigh")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	return cfg, nil
}

func validEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func cleanWorkDirs(workDirs map[string]string) (map[string]string, error) {
	if len(workDirs) == 0 {
		return nil, nil
	}
	cleaned := make(map[string]string, len(workDirs))
	for name, path := range workDirs {
		cleanName := strings.TrimSpace(name)
		cleanPath := strings.TrimSpace(path)
		if cleanName == "" {
			return nil, fmt.Errorf("work_dirs contains empty project name")
		}
		if cleanPath == "" {
			return nil, fmt.Errorf("work_dirs.%s is empty", cleanName)
		}
		cleaned[cleanName] = cleanPath
	}
	return cleaned, nil
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
