package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type State struct {
	Sessions map[string]Session `json:"sessions"`
}

type Session struct {
	CodexThreadID   string `json:"codex_thread_id"`
	WorkDir         string `json:"work_dir"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{Sessions: make(map[string]Session)}, nil
		}
		return State{}, fmt.Errorf("read state %q: %w", path, err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse state %q: %w", path, err)
	}

	if state.Sessions == nil {
		state.Sessions = make(map[string]Session)
	}
	delete(state.Sessions, "default")

	return state, nil
}

func (state *State) EnsureSession(key string, defaultWorkDir string) Session {
	key = cleanSessionKey(key)
	if state.Sessions == nil {
		state.Sessions = make(map[string]Session)
	}

	session, ok := state.Sessions[key]
	if !ok {
		session = Session{WorkDir: cleanDefaultWorkDir(defaultWorkDir)}
		state.Sessions[key] = session
		return session
	}
	if strings.TrimSpace(session.WorkDir) == "" {
		session.WorkDir = cleanDefaultWorkDir(defaultWorkDir)
		state.Sessions[key] = session
	}

	return session
}

func (state *State) SetSessionThread(key string, threadID string) {
	key = cleanSessionKey(key)
	session := state.EnsureSession(key, "")
	session.CodexThreadID = strings.TrimSpace(threadID)
	state.Sessions[key] = session
}

func (state *State) SetSessionWorkDir(key string, workDir string) {
	key = cleanSessionKey(key)
	session := state.EnsureSession(key, "")
	session.WorkDir = cleanDefaultWorkDir(workDir)
	state.Sessions[key] = session
}

func (state *State) SetSessionModelConfig(key string, model string, effort string, serviceTier string) {
	key = cleanSessionKey(key)
	session := state.EnsureSession(key, "")
	session.Model = strings.TrimSpace(model)
	session.ReasoningEffort = strings.ToLower(strings.TrimSpace(effort))
	session.ServiceTier = strings.ToLower(strings.TrimSpace(serviceTier))
	state.Sessions[key] = session
}

func (state State) Save(path string) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create state dir %q: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace state %q: %w", path, err)
	}

	return nil
}

func cleanDefaultWorkDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "."
	}
	return path
}

func cleanSessionKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "default"
	}
	return key
}
