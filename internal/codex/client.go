package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultBin = "codex"

type Client struct {
	Bin string

	mu       sync.Mutex
	nextID   atomic.Int64
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	messages chan wireMessage
	done     chan error
}

type TurnResult struct {
	ThreadID string
	TurnID   string
	Text     string
}

type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (err *RPCError) Error() string {
	if err == nil {
		return ""
	}
	if len(err.Data) == 0 {
		return fmt.Sprintf("codex rpc error %d: %s", err.Code, err.Message)
	}
	return fmt.Sprintf("codex rpc error %d: %s: %s", err.Code, err.Message, string(err.Data))
}

func NewClient(bin string) *Client {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = defaultBin
	}
	return &Client{Bin: bin}
}

func (client *Client) Start(ctx context.Context) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.cmd != nil {
		return nil
	}

	cmd := exec.Command(client.Bin, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}

	client.cmd = cmd
	client.stdin = stdin
	client.messages = make(chan wireMessage, 64)
	client.done = make(chan error, 1)
	client.nextID.Store(0)

	go scanJSONLines(stdout, client.messages)
	go scanStderr(stderr)
	go waitForExit(cmd, client.messages, client.done)

	if _, err := client.requestLocked(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "lark-bridge",
			"version": "0.0.0",
		},
		"capabilities": nil,
	}); err != nil {
		_ = client.closeLocked()
		return err
	}
	if err := client.writeLocked(map[string]any{"method": "initialized"}); err != nil {
		_ = client.closeLocked()
		return err
	}

	return nil
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.closeLocked()
}

func (client *Client) StartThread(ctx context.Context, cwd string, ephemeral bool) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	params := map[string]any{}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}
	if ephemeral {
		params["ephemeral"] = true
	}

	result, err := client.requestLocked(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}

	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("decode thread/start response: %w", err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return "", fmt.Errorf("thread/start response missing thread id")
	}
	return response.Thread.ID, nil
}

func (client *Client) ResumeThread(ctx context.Context, threadID string, cwd string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("thread id is required")
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	params := map[string]any{
		"threadId":     threadID,
		"excludeTurns": true,
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}

	result, err := client.requestLocked(ctx, "thread/resume", params)
	if err != nil {
		return "", err
	}

	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("decode thread/resume response: %w", err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return "", fmt.Errorf("thread/resume response missing thread id")
	}
	return response.Thread.ID, nil
}

func (client *Client) RunTurn(ctx context.Context, threadID string, cwd string, text string) (TurnResult, error) {
	threadID = strings.TrimSpace(threadID)
	text = strings.TrimSpace(text)
	if threadID == "" {
		return TurnResult{}, fmt.Errorf("thread id is required")
	}
	if text == "" {
		return TurnResult{}, fmt.Errorf("text is required")
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{
				"type":          "text",
				"text":          text,
				"text_elements": []any{},
			},
		},
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}

	result, err := client.requestLocked(ctx, "turn/start", params)
	if err != nil {
		return TurnResult{}, err
	}

	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return TurnResult{}, fmt.Errorf("decode turn/start response: %w", err)
	}
	turnID := strings.TrimSpace(started.Turn.ID)
	if turnID == "" {
		return TurnResult{}, fmt.Errorf("turn/start response missing turn id")
	}

	var deltas strings.Builder
	var finalText string
	for {
		message, err := client.readLocked(ctx)
		if err != nil {
			return TurnResult{}, err
		}
		if message.Error != nil {
			return TurnResult{}, message.Error
		}
		if message.isServerRequest() {
			return TurnResult{}, fmt.Errorf("codex requested user interaction: %s", message.Method)
		}

		switch message.Method {
		case "item/agentMessage/delta":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Delta    string `json:"delta"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return TurnResult{}, fmt.Errorf("decode agent delta: %w", err)
			}
			if params.ThreadID == threadID && params.TurnID == turnID {
				deltas.WriteString(params.Delta)
			}
		case "item/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					Type  string `json:"type"`
					Text  string `json:"text"`
					Phase string `json:"phase"`
				} `json:"item"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return TurnResult{}, fmt.Errorf("decode completed item: %w", err)
			}
			if params.ThreadID == threadID && params.TurnID == turnID && params.Item.Type == "agentMessage" {
				finalText = params.Item.Text
			}
		case "turn/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Error  any    `json:"error"`
				} `json:"turn"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return TurnResult{}, fmt.Errorf("decode turn/completed: %w", err)
			}
			if params.ThreadID != threadID || params.Turn.ID != turnID {
				continue
			}
			if params.Turn.Status != "completed" {
				return TurnResult{}, fmt.Errorf("turn %s ended with status %s: %v", turnID, params.Turn.Status, params.Turn.Error)
			}
			answer := deltas.String()
			if answer == "" {
				answer = finalText
			}
			return TurnResult{ThreadID: threadID, TurnID: turnID, Text: strings.TrimSpace(answer)}, nil
		case "error":
			var params struct {
				Error any `json:"error"`
			}
			_ = json.Unmarshal(message.Params, &params)
			return TurnResult{}, fmt.Errorf("codex error notification: %v", params.Error)
		}
	}
}

func (client *Client) requestLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if client.cmd == nil || client.stdin == nil {
		return nil, fmt.Errorf("codex client is not started")
	}

	id := client.nextID.Add(1)
	if err := client.writeLocked(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	}); err != nil {
		return nil, err
	}

	for {
		message, err := client.readLocked(ctx)
		if err != nil {
			return nil, err
		}
		if message.Error != nil {
			if message.matchesID(id) {
				return nil, message.Error
			}
			continue
		}
		if message.matchesID(id) {
			return message.Result, nil
		}
		if message.isServerRequest() {
			return nil, fmt.Errorf("codex requested user interaction before %s completed: %s", method, message.Method)
		}
	}
}

func (client *Client) readLocked(ctx context.Context) (wireMessage, error) {
	select {
	case <-ctx.Done():
		return wireMessage{}, ctx.Err()
	case message, ok := <-client.messages:
		if !ok {
			return wireMessage{}, fmt.Errorf("codex app-server message stream closed")
		}
		if message.err != nil {
			return wireMessage{}, message.err
		}
		return message, nil
	}
}

func (client *Client) writeLocked(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := client.stdin.Write(data); err != nil {
		return fmt.Errorf("write codex request: %w", err)
	}
	return nil
}

func (client *Client) closeLocked() error {
	if client.stdin != nil {
		_ = client.stdin.Close()
		client.stdin = nil
	}
	if client.cmd == nil {
		return nil
	}

	cmd := client.cmd
	client.cmd = nil
	done := client.done
	client.done = nil
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	err := <-done
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil
		}
	}
	return err
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`

	err error
}

func (message wireMessage) matchesID(id int64) bool {
	if len(message.ID) == 0 {
		return false
	}
	var number int64
	if err := json.Unmarshal(message.ID, &number); err == nil {
		return number == id
	}
	var text string
	if err := json.Unmarshal(message.ID, &text); err == nil {
		return text == strconv.FormatInt(id, 10)
	}
	return false
}

func (message wireMessage) isServerRequest() bool {
	return len(message.ID) > 0 && message.Method != "" && len(message.Result) == 0 && message.Error == nil
}

func scanJSONLines(reader io.Reader, messages chan<- wireMessage) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message wireMessage
		if err := json.Unmarshal(line, &message); err != nil {
			messages <- wireMessage{err: fmt.Errorf("decode codex message %q: %w", string(line), err)}
			continue
		}
		messages <- message
	}
	if err := scanner.Err(); err != nil {
		messages <- wireMessage{err: fmt.Errorf("read codex stdout: %w", err)}
	}
}

func scanStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
	}
}

func waitForExit(cmd *exec.Cmd, messages chan<- wireMessage, done chan<- error) {
	err := cmd.Wait()
	done <- err
	if err == nil {
		messages <- wireMessage{err: fmt.Errorf("codex app-server exited")}
		return
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		messages <- wireMessage{err: fmt.Errorf("codex app-server exited: %w", err)}
		return
	}
	messages <- wireMessage{err: fmt.Errorf("wait codex app-server: %w", err)}
}
