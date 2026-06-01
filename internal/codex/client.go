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
	Bin             string
	Model           string
	ReasoningEffort string
	ServiceTier     string

	mu       sync.Mutex
	nextID   atomic.Int64
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	incoming chan wireMessage
	events   chan wireMessage
	replies  map[string]chan wireMessage
	done     chan error
}

type TurnResult struct {
	ThreadID string
	TurnID   string
	Text     string
}

type TurnUpdateFunc func(text string)
type TurnStartedFunc func(turnID string)

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

func NewClient(bin string, model string, reasoningEffort string, serviceTier string) *Client {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = defaultBin
	}
	return &Client{
		Bin:             bin,
		Model:           strings.TrimSpace(model),
		ReasoningEffort: strings.ToLower(strings.TrimSpace(reasoningEffort)),
		ServiceTier:     strings.ToLower(strings.TrimSpace(serviceTier)),
	}
}

func (client *Client) Start(ctx context.Context) error {
	client.mu.Lock()

	if client.cmd != nil {
		client.mu.Unlock()
		return nil
	}

	args := []string{"app-server"}
	args = appendConfigOverride(args, "model", client.Model)
	args = appendConfigOverride(args, "model_reasoning_effort", client.ReasoningEffort)
	args = appendConfigOverride(args, "service_tier", client.ServiceTier)
	args = append(args, "--listen", "stdio://")
	cmd := exec.Command(client.Bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		client.mu.Unlock()
		return fmt.Errorf("open codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		client.mu.Unlock()
		return fmt.Errorf("open codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		client.mu.Unlock()
		return fmt.Errorf("open codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		client.mu.Unlock()
		return fmt.Errorf("start codex app-server: %w", err)
	}

	client.cmd = cmd
	client.stdin = stdin
	client.incoming = make(chan wireMessage, 64)
	client.events = make(chan wireMessage, 128)
	client.replies = make(map[string]chan wireMessage)
	client.done = make(chan error, 1)
	client.nextID.Store(0)
	incoming := client.incoming
	events := client.events
	client.mu.Unlock()

	go client.dispatchMessages(incoming, events)
	go scanJSONLines(stdout, incoming)
	go scanStderr(stderr)
	go waitForExit(cmd, client.done)

	if _, err := client.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "lark-bridge",
			"version": "0.0.0",
		},
		"capabilities": nil,
	}); err != nil {
		_ = client.Close()
		return err
	}
	if err := client.write(map[string]any{"method": "initialized"}); err != nil {
		_ = client.Close()
		return err
	}

	return nil
}

func appendConfigOverride(args []string, key string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return args
	}
	return append(args, "-c", fmt.Sprintf("%s=%q", key, value))
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.closeLocked()
}

func (client *Client) StartThread(ctx context.Context, cwd string, ephemeral bool) (string, error) {
	params := map[string]any{}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}
	if ephemeral {
		params["ephemeral"] = true
	}

	result, err := client.request(ctx, "thread/start", params)
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

	params := map[string]any{
		"threadId": threadID,
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}

	result, err := client.request(ctx, "thread/resume", params)
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

func (client *Client) RunTurn(ctx context.Context, threadID string, cwd string, text string, onUpdate TurnUpdateFunc, onStarted TurnStartedFunc) (TurnResult, error) {
	threadID = strings.TrimSpace(threadID)
	text = strings.TrimSpace(text)
	if threadID == "" {
		return TurnResult{}, fmt.Errorf("thread id is required")
	}
	if text == "" {
		return TurnResult{}, fmt.Errorf("text is required")
	}

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

	result, err := client.request(ctx, "turn/start", params)
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
	if onStarted != nil {
		onStarted(turnID)
	}

	return client.drainTurn(ctx, threadID, turnID, onUpdate)
}

func (client *Client) SteerTurn(ctx context.Context, threadID string, turnID string, text string) error {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	text = strings.TrimSpace(text)
	if threadID == "" {
		return fmt.Errorf("thread id is required")
	}
	if turnID == "" {
		return fmt.Errorf("turn id is required")
	}
	if text == "" {
		return fmt.Errorf("text is required")
	}

	_, err := client.request(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input": []map[string]any{
			{
				"type":          "text",
				"text":          text,
				"text_elements": []any{},
			},
		},
	})
	return err
}

func (client *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := client.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	reply := make(chan wireMessage, 1)

	client.mu.Lock()
	if client.cmd == nil || client.stdin == nil {
		client.mu.Unlock()
		return nil, fmt.Errorf("codex client is not started")
	}
	client.replies[key] = reply
	if err := client.writeLocked(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	}); err != nil {
		delete(client.replies, key)
		client.mu.Unlock()
		return nil, err
	}
	client.mu.Unlock()

	select {
	case <-ctx.Done():
		client.forgetReply(key)
		return nil, ctx.Err()
	case message := <-reply:
		if message.err != nil {
			return nil, message.err
		}
		if message.Error != nil {
			return nil, message.Error
		}
		return message.Result, nil
	}
}

func (client *Client) readEvent(ctx context.Context) (wireMessage, error) {
	select {
	case <-ctx.Done():
		return wireMessage{}, ctx.Err()
	case message, ok := <-client.events:
		if !ok {
			return wireMessage{}, fmt.Errorf("codex app-server message stream closed")
		}
		if message.err != nil {
			return wireMessage{}, message.err
		}
		return message, nil
	}
}

func (client *Client) write(value any) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.cmd == nil || client.stdin == nil {
		return fmt.Errorf("codex client is not started")
	}
	return client.writeLocked(value)
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
	for key, reply := range client.replies {
		delete(client.replies, key)
		reply <- wireMessage{err: fmt.Errorf("codex client closed")}
		close(reply)
	}
	client.incoming = nil
	client.events = nil
	client.replies = nil
	done := client.done
	client.done = nil
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	var err error
	if done != nil {
		err = <-done
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil
		}
	}
	return err
}

func (client *Client) dispatchMessages(incoming <-chan wireMessage, events chan<- wireMessage) {
	for message := range incoming {
		if message.err != nil {
			client.failReplies(message)
			events <- message
			continue
		}
		if len(message.ID) > 0 && message.Method == "" {
			key := requestIDKey(message.ID)
			client.deliverReply(key, message)
			continue
		}
		events <- message
	}
	client.failReplies(wireMessage{err: fmt.Errorf("codex app-server message stream closed")})
	close(events)
}

func (client *Client) deliverReply(key string, message wireMessage) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.replies == nil {
		return false
	}
	reply := client.replies[key]
	if reply == nil {
		return false
	}
	delete(client.replies, key)
	reply <- message
	close(reply)
	return true
}

func (client *Client) forgetReply(key string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.replies != nil {
		delete(client.replies, key)
	}
}

func (client *Client) failReplies(message wireMessage) {
	client.mu.Lock()
	defer client.mu.Unlock()
	for key, reply := range client.replies {
		delete(client.replies, key)
		reply <- message
		close(reply)
	}
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`

	err error
}

func requestIDKey(raw json.RawMessage) string {
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.FormatInt(number, 10)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}

func (message wireMessage) isServerRequest() bool {
	return len(message.ID) > 0 && message.Method != "" && len(message.Result) == 0 && message.Error == nil
}

func scanJSONLines(reader io.Reader, messages chan<- wireMessage) {
	defer close(messages)
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
		return
	}
	messages <- wireMessage{err: fmt.Errorf("codex app-server stdout closed")}
}

func scanStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
	}
}

func waitForExit(cmd *exec.Cmd, done chan<- error) {
	done <- cmd.Wait()
}
