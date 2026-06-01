package lark

import (
	"encoding/json"
	"fmt"
	"strings"
)

type CardAction struct {
	Action     string
	Label      string
	Style      string
	SessionKey string
	TurnID     string
	ItemID     string
}

type StatusCard struct {
	Title   string
	Status  string
	Body    string
	Footer  string
	Actions []CardAction
}

func BuildStatusCard(card StatusCard) string {
	template := statusTemplate(card.Status)
	elements := []any{
		map[string]any{
			"tag":     "markdown",
			"content": truncateCardText(card.Body, 12000),
		},
	}
	if strings.TrimSpace(card.Footer) != "" {
		elements = append(elements,
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag":     "markdown",
				"content": truncateCardText(card.Footer, 900),
			},
		)
	}
	if len(card.Actions) > 0 {
		elements = append(elements, actionColumns(card.Actions))
	}

	payload := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi":     true,
			"wide_screen_mode": true,
		},
		"header": map[string]any{
			"template": template,
			"title": map[string]any{
				"tag":     "plain_text",
				"content": card.Title,
			},
		},
		"body": map[string]any{
			"direction": "vertical",
			"elements":  elements,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"card render failed"}]}}`
	}
	return string(data)
}

func actionColumns(actions []CardAction) map[string]any {
	columns := make([]any, 0, len(actions))
	for _, action := range actions {
		columns = append(columns, map[string]any{
			"tag":    "column",
			"width":  "weighted",
			"weight": 1,
			"elements": []any{
				map[string]any{
					"tag": "button",
					"text": map[string]any{
						"tag":     "plain_text",
						"content": action.Label,
					},
					"type": action.Style,
					"behaviors": []any{
						map[string]any{
							"type": "callback",
							"value": map[string]any{
								"action":      action.Action,
								"session_key": action.SessionKey,
								"turn_id":     action.TurnID,
								"item_id":     action.ItemID,
							},
						},
					},
				},
			},
		})
	}
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"columns":   columns,
	}
}

func statusTemplate(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "done", "ok":
		return "green"
	case "error", "failed":
		return "red"
	case "pending", "warning":
		return "orange"
	case "running", "processing":
		return "blue"
	default:
		return "blue"
	}
}

func truncateCardText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len([]rune(text)) <= limit {
		return text
	}
	runes := []rune(text)
	return fmt.Sprintf("%s\n\n...已截断 %d 字符", string(runes[:limit]), len(runes)-limit)
}
