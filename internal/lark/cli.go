package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type CLI struct {
	Bin string
}

func NewCLI(bin string) CLI {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = "lark-cli"
	}
	return CLI{Bin: bin}
}

func (cli CLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cli.Bin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", cli.Bin, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (cli CLI) RunJSON(ctx context.Context, target any, args ...string) error {
	output, err := cli.Run(ctx, args...)
	if err != nil {
		return err
	}
	if err := parseJSONOutput(output, target); err != nil {
		return fmt.Errorf("parse %s %s: %w", cli.Bin, strings.Join(args, " "), err)
	}
	return nil
}

func parseJSONOutput(output []byte, target any) error {
	trimmed := strings.TrimSpace(string(output))
	for index, char := range trimmed {
		if char != '{' && char != '[' {
			continue
		}
		if err := json.Unmarshal([]byte(trimmed[index:]), target); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no JSON payload found in output: %s", trimmed)
}
