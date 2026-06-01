package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type sessionOptions struct {
	workDir string
	project string
	model   string
	effort  string
	tier    string
}

type createRequest struct {
	sessionOptions
	prompt string
}

type attachRequest struct {
	sessionOptions
	threadID string
}

func (app *daemon) parseCreateRequest(arg string) (createRequest, error) {
	fields := strings.Fields(strings.TrimSpace(arg))
	request := createRequest{}
	promptFields := make([]string, 0, len(fields))

	for index := 0; index < len(fields); index++ {
		handled, err := app.parseSessionOption(fields, &index, "create", &request.sessionOptions)
		if err != nil {
			return createRequest{}, err
		}
		if !handled {
			promptFields = append(promptFields, fields[index])
		}
	}

	request.prompt = strings.TrimSpace(strings.Join(promptFields, " "))
	return request, nil
}

func (app *daemon) parseAttachRequest(arg string) (attachRequest, error) {
	fields := strings.Fields(strings.TrimSpace(arg))
	request := attachRequest{}

	for index := 0; index < len(fields); index++ {
		handled, err := app.parseSessionOption(fields, &index, "attach", &request.sessionOptions)
		if err != nil {
			return attachRequest{}, err
		}
		if !handled {
			if request.threadID != "" {
				return attachRequest{}, fmt.Errorf("/attach 只接受一个 Codex thread id")
			}
			request.threadID = strings.TrimSpace(fields[index])
		}
	}

	if request.threadID == "" {
		return attachRequest{}, fmt.Errorf("用法：/attach CODEX_THREAD_ID [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast]")
	}
	return request, nil
}

func (app *daemon) parseSessionOption(fields []string, index *int, command string, options *sessionOptions) (bool, error) {
	field := fields[*index]
	name, value, ok, err := sessionOptionValue(fields, index, field)
	if err != nil {
		return true, err
	}
	if !ok {
		if strings.HasPrefix(field, "--") {
			return true, fmt.Errorf("未知 /%s 参数：%s", command, field)
		}
		return false, nil
	}

	switch name {
	case "cwd":
		workDir, err := app.resolveWorkDir(value)
		if err != nil {
			return true, err
		}
		options.workDir = workDir
	case "project":
		if err := app.applyProjectOption(options, value); err != nil {
			return true, err
		}
	case "model":
		model, err := normalizeModel(value)
		if err != nil {
			return true, err
		}
		options.model = model
	case "effort":
		effort, err := normalizeEffort(value)
		if err != nil {
			return true, err
		}
		options.effort = effort
	case "tier":
		tier, err := normalizeServiceTier(value)
		if err != nil {
			return true, err
		}
		options.tier = tier
	default:
		return true, fmt.Errorf("未知 /%s 参数：%s", command, field)
	}
	return true, nil
}

func sessionOptionValue(fields []string, index *int, field string) (string, string, bool, error) {
	switch field {
	case "--cwd", "--project", "--model", "--effort", "--tier":
		value, err := nextOptionValue(fields, index, field, strings.TrimPrefix(field, "--"))
		return strings.TrimPrefix(field, "--"), value, true, err
	default:
		return "", "", false, nil
	}
}

func nextOptionValue(fields []string, index *int, field string, name string) (string, error) {
	*index = *index + 1
	if *index >= len(fields) {
		return "", fmt.Errorf("%s 需要一个 %s 参数", field, name)
	}
	return fields[*index], nil
}

func (app *daemon) applyProjectOption(options *sessionOptions, raw string) error {
	project := strings.TrimSpace(raw)
	workDir, err := app.resolveProjectWorkDir(project)
	if err != nil {
		return err
	}
	options.project = project
	options.workDir = workDir
	return nil
}

func normalizeModel(raw string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", fmt.Errorf("model 不能为空")
	}
	return model, nil
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
