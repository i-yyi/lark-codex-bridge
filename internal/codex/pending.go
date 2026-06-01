package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	DecisionApprove = "approve"
	DecisionDeny    = "deny"
	DecisionCancel  = "cancel"
)

type PendingRequest struct {
	RequestID json.RawMessage
	Method    string
	ThreadID  string
	TurnID    string
	ItemID    string
	Summary   string
	Params    json.RawMessage
}

type PendingRequestError struct {
	Pending PendingRequest
}

func (err *PendingRequestError) Error() string {
	return "codex requested user interaction: " + err.Pending.Method
}

func (client *Client) RespondToPending(ctx context.Context, pending PendingRequest, decision string, onUpdate TurnUpdateFunc) (TurnResult, error) {
	result, err := pendingResponse(pending.Method, decision)
	if err != nil {
		return TurnResult{}, err
	}
	if err := client.write(map[string]any{
		"id":     pending.RequestID,
		"result": result,
	}); err != nil {
		return TurnResult{}, err
	}

	return client.drainTurn(ctx, pending.ThreadID, pending.TurnID, onUpdate)
}

func (client *Client) drainTurn(ctx context.Context, threadID string, turnID string, onUpdate TurnUpdateFunc) (TurnResult, error) {
	var deltas strings.Builder
	var finalText string
	for {
		message, err := client.readEvent(ctx)
		if err != nil {
			return TurnResult{}, err
		}
		if message.Error != nil {
			return TurnResult{}, message.Error
		}
		if message.isServerRequest() {
			pending, err := pendingFromMessage(message)
			if err != nil {
				return TurnResult{}, err
			}
			return TurnResult{}, &PendingRequestError{Pending: pending}
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
				if onUpdate != nil {
					onUpdate(strings.TrimSpace(deltas.String()))
				}
			}
		case "item/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return TurnResult{}, fmt.Errorf("decode completed item: %w", err)
			}
			if params.ThreadID == threadID && params.TurnID == turnID && params.Item.Type == "agentMessage" {
				finalText = params.Item.Text
				if onUpdate != nil && deltas.Len() == 0 {
					onUpdate(strings.TrimSpace(finalText))
				}
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

func pendingFromMessage(message wireMessage) (PendingRequest, error) {
	pending := PendingRequest{
		RequestID: append(json.RawMessage(nil), message.ID...),
		Method:    message.Method,
		Params:    append(json.RawMessage(nil), message.Params...),
	}

	var base struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
	}
	_ = json.Unmarshal(message.Params, &base)
	pending.ThreadID = base.ThreadID
	pending.TurnID = base.TurnID
	pending.ItemID = base.ItemID

	switch message.Method {
	case "item/commandExecution/requestApproval":
		pending.Summary = commandApprovalSummary(message.Params)
	case "item/fileChange/requestApproval":
		pending.Summary = fileChangeApprovalSummary(message.Params)
	default:
		pending.Summary = "Codex 请求了暂不支持的交互：" + message.Method
	}
	if pending.ThreadID == "" || pending.TurnID == "" {
		return PendingRequest{}, fmt.Errorf("codex request %s missing thread or turn id", message.Method)
	}
	return pending, nil
}

func commandApprovalSummary(params json.RawMessage) string {
	var request struct {
		Command            string   `json:"command"`
		CWD                string   `json:"cwd"`
		Reason             string   `json:"reason"`
		AvailableDecisions []string `json:"availableDecisions"`
	}
	_ = json.Unmarshal(params, &request)

	lines := []string{"Codex 需要执行命令。"}
	if request.Command != "" {
		lines = append(lines, "command: "+request.Command)
	}
	if request.CWD != "" {
		lines = append(lines, "cwd: "+request.CWD)
	}
	if request.Reason != "" {
		lines = append(lines, "reason: "+request.Reason)
	}
	return strings.Join(lines, "\n")
}

func fileChangeApprovalSummary(params json.RawMessage) string {
	var request struct {
		Reason    string `json:"reason"`
		GrantRoot string `json:"grantRoot"`
	}
	_ = json.Unmarshal(params, &request)

	lines := []string{"Codex 需要修改文件。"}
	if request.GrantRoot != "" {
		lines = append(lines, "root: "+request.GrantRoot)
	}
	if request.Reason != "" {
		lines = append(lines, "reason: "+request.Reason)
	}
	return strings.Join(lines, "\n")
}

func pendingResponse(method string, decision string) (map[string]any, error) {
	decision = strings.TrimSpace(strings.ToLower(decision))
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"decision": approvalDecision(decision)}, nil
	default:
		return nil, fmt.Errorf("unsupported codex interaction: %s", method)
	}
}

func approvalDecision(decision string) string {
	switch decision {
	case DecisionApprove:
		return "accept"
	case DecisionCancel:
		return "cancel"
	default:
		return "decline"
	}
}
