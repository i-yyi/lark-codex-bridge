package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	OwnerOpenID    string `json:"owner_open_id"`
	DefaultWorkDir string `json:"default_work_dir"`
	StatePath      string `json:"state_path"`
	LarkCLI        string `json:"lark_cli"`
	LarkCLINoProxy bool   `json:"lark_cli_no_proxy"`
	CodexCLI       string `json:"codex_cli"`
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
	cfg.LarkCLI = strings.TrimSpace(cfg.LarkCLI)
	cfg.CodexCLI = strings.TrimSpace(cfg.CodexCLI)

	if cfg.OwnerOpenID == "" {
		return Config{}, fmt.Errorf("owner_open_id is required")
	}
	if cfg.DefaultWorkDir == "" {
		cfg.DefaultWorkDir = "."
	}
	if cfg.StatePath == "" {
		cfg.StatePath = "state.json"
	}
	if cfg.LarkCLI == "" {
		cfg.LarkCLI = "lark-cli"
	}
	if cfg.CodexCLI == "" {
		cfg.CodexCLI = "codex"
	}

	return cfg, nil
}
