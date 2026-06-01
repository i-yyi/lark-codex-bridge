package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SessionInfo struct {
	ID      string
	CWD     string
	Title   string
	Updated time.Time
}

func ScanHistory(limit int) ([]SessionInfo, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}

	titles := loadSessionTitles(filepath.Join(home, "session_index.jsonl"))
	sessions := make([]SessionInfo, 0, limit)
	for _, dir := range []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")} {
		if err := scanSessionDir(dir, titles, &sessions); err != nil {
			return nil, err
		}
	}

	sort.Slice(sessions, func(left, right int) bool {
		return sessions[left].Updated.After(sessions[right].Updated)
	})
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

func FormatHistoryText(sessions []SessionInfo) string {
	if len(sessions) == 0 {
		return "没有扫描到 Codex sessions。"
	}
	var builder strings.Builder
	builder.WriteString("recent codex sessions:")
	for index, session := range sessions {
		title := session.Title
		if title == "" {
			title = "untitled"
		}
		cwd := session.CWD
		if cwd == "" {
			cwd = "-"
		}
		updated := "-"
		if !session.Updated.IsZero() {
			updated = session.Updated.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&builder, "\n%d. %s\n   id: %s\n   updated: %s\n   cwd: %s\n   import: /import %d", index+1, title, session.ID, updated, cwd, index+1)
	}
	return builder.String()
}

func Home() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve codex home: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func scanSessionDir(root string, titles map[string]string, sessions *[]SessionInfo) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", root, err)
	}

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		session, ok := readSessionInfo(path, titles)
		if ok {
			*sessions = append(*sessions, session)
		}
		return nil
	})
}

func readSessionInfo(path string, titles map[string]string) (SessionInfo, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return SessionInfo{}, false
	}

	var record struct {
		Payload struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		return SessionInfo{}, false
	}
	id := strings.TrimSpace(record.Payload.ID)
	if id == "" {
		id = sessionIDFromPath(path)
	}
	if id == "" {
		return SessionInfo{}, false
	}

	updated := time.Time{}
	if info, err := os.Stat(path); err == nil {
		updated = info.ModTime()
	}
	title := strings.TrimSpace(titles[id])
	if title == "" {
		title = generatedSessionTitle(scanner)
	}
	return SessionInfo{
		ID:      id,
		CWD:     strings.TrimSpace(record.Payload.CWD),
		Title:   title,
		Updated: updated,
	}, true
}

func generatedSessionTitle(scanner *bufio.Scanner) string {
	const maxScanLines = 300
	for index := 0; index < maxScanLines && scanner.Scan(); index++ {
		text := sessionUserText(scanner.Bytes())
		if text == "" {
			continue
		}
		return summarizeSessionTitle(text)
	}
	return ""
}

func sessionUserText(raw []byte) string {
	var record struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return ""
	}

	switch record.Type {
	case "event_msg":
		var payload struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil || payload.Type != "user_message" {
			return ""
		}
		return cleanSessionTitleText(payload.Message)
	case "response_item":
		var payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil || payload.Type != "message" || payload.Role != "user" {
			return ""
		}
		for _, part := range payload.Content {
			if part.Type != "input_text" {
				continue
			}
			if text := cleanSessionTitleText(part.Text); text != "" {
				return text
			}
		}
	}
	return ""
}

func cleanSessionTitleText(text string) string {
	text = strings.TrimSpace(text)
	if marker := "## My request for Codex:"; strings.Contains(text, marker) {
		_, after, _ := strings.Cut(text, marker)
		text = strings.TrimSpace(after)
	}
	text = strings.TrimSpace(text)
	if text == "" || isSessionBoilerplate(text) {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

func isSessionBoilerplate(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(lower, "<environment_context>") ||
		strings.HasPrefix(lower, "<permissions instructions>") ||
		strings.HasPrefix(lower, "<apps_instructions>") ||
		strings.HasPrefix(lower, "<skills_instructions>") ||
		strings.HasPrefix(lower, "<plugins_instructions>") ||
		strings.HasPrefix(lower, "<collaboration_mode>") ||
		strings.HasPrefix(lower, "<app-context>") {
		return true
	}
	if strings.HasPrefix(text, "The following is the Codex agent history") ||
		strings.HasPrefix(text, "Reviewed Codex session id:") ||
		strings.Contains(text, ">>> TRANSCRIPT DELTA START") ||
		strings.Contains(text, ">>> APPROVAL REQUEST START") {
		return true
	}
	return false
}

func summarizeSessionTitle(text string) string {
	const limit = 64
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "..."
}

func sessionIDFromPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	index := strings.LastIndex(name, "-019")
	if index < 0 {
		return ""
	}
	return strings.TrimPrefix(name[index:], "-")
}

func loadSessionTitles(path string) map[string]string {
	titles := make(map[string]string)
	file, err := os.Open(path)
	if err != nil {
		return titles
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		var record struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			continue
		}
		if id := strings.TrimSpace(record.ID); id != "" {
			titles[id] = strings.TrimSpace(record.ThreadName)
		}
	}
	return titles
}
