package main

import (
	"context"
	"strings"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	"lark-bridge/internal/lark"
)

func (app *daemon) replyTurnResult(ctx context.Context, messageID string, route messageRoute, result codex.TurnResult, statusMessageID string) error {
	answer := strings.TrimSpace(result.Text)
	if answer == "" {
		answer = bridge.PickPhrase(messageID+":empty", bridge.EmptyResult)
	}
	if route.kind == kindTask {
		card := app.taskCard("Codex 完成", "success", answer, route, "", nil)
		if err := app.replyOrPatchCard(ctx, messageID, statusMessageID, route, card); err != nil {
			app.reactForRoute(ctx, route, messageID, reactionError)
			return err
		}
		app.reactForRoute(ctx, route, messageID, reactionDone)
		return nil
	}
	if err := app.respond(ctx, messageID, route, answer); err != nil {
		app.reactForRoute(ctx, route, messageID, reactionError)
		return err
	}
	app.reactForRoute(ctx, route, messageID, reactionDone)
	return nil
}

func (app *daemon) taskCard(title string, status string, body string, route messageRoute, workDir string, actions []lark.CardAction) string {
	footerParts := []string{"session: " + route.sessionKey}
	if strings.TrimSpace(workDir) != "" {
		footerParts = append(footerParts, "workdir: "+workDir)
	}
	return lark.BuildStatusCard(lark.StatusCard{
		Title:   title,
		Status:  status,
		Body:    body,
		Footer:  strings.Join(footerParts, "\n"),
		Actions: actions,
	})
}

func (app *daemon) pendingActions(route messageRoute, pending codex.PendingRequest) []lark.CardAction {
	return []lark.CardAction{
		{Action: codex.DecisionApprove, Label: "Approve", Style: "primary", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
		{Action: codex.DecisionDeny, Label: "Deny", Style: "danger", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
		{Action: codex.DecisionCancel, Label: "Cancel", Style: "default", SessionKey: route.sessionKey, TurnID: pending.TurnID, ItemID: pending.ItemID},
	}
}

func (app *daemon) runningActions(route messageRoute) []lark.CardAction {
	return []lark.CardAction{
		{Action: codex.DecisionCancel, Label: "Cancel", Style: "danger", SessionKey: route.sessionKey},
	}
}

func (app *daemon) liveCardUpdater(ctx context.Context, route messageRoute, messageID string, statusMessageID string, workDir string) (codex.TurnUpdateFunc, func()) {
	if route.kind != kindTask || strings.TrimSpace(statusMessageID) == "" {
		return nil, func() {}
	}
	updates := make(chan string, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})

	go app.liveCardLoop(ctx, route, messageID, statusMessageID, workDir, updates, done, stopped)

	onUpdate := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		select {
		case updates <- text:
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- text:
			default:
			}
		}
	}

	stop := func() {
		close(done)
		<-stopped
	}
	return onUpdate, stop
}

func (app *daemon) liveCardLoop(ctx context.Context, route messageRoute, messageID string, statusMessageID string, workDir string, updates <-chan string, done <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(liveCardUpdateInterval)
	defer ticker.Stop()

	latest := ""
	dirty := false
	for {
		select {
		case text := <-updates:
			latest = text
			dirty = true
		case <-ticker.C:
			if !dirty || strings.TrimSpace(latest) == "" {
				continue
			}
			body := liveCardBody(latest)
			card := app.taskCard("Codex 正在运行", "running", body, route, workDir, app.runningActions(route))
			patchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := app.replyOrPatchCard(patchCtx, messageID, statusMessageID, route, card)
			cancel()
			if err != nil {
				app.logError("live.card.fail", "msg", messageID, "key", route.sessionKey, "err", err)
			}
			dirty = false
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func liveCardBody(text string) string {
	return bridge.PickPhrase(text, bridge.LiveCardIntro) + "\n\n" + strings.TrimSpace(text) + "\n\n⏳"
}

func (app *daemon) replyOrPatchCard(ctx context.Context, sourceMessageID string, statusMessageID string, route messageRoute, card string) error {
	if statusMessageID != "" {
		start := time.Now()
		if err := app.lark.PatchCard(ctx, statusMessageID, card); err != nil {
			app.logError("lark.card.patch.fail", "msg", statusMessageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return err
		}
		app.debug("lark.card.patch.ok", "msg", statusMessageID, "key", route.sessionKey, "dur", elapsed(start))
		return nil
	}
	_, err := app.replyCard(ctx, sourceMessageID, route, card)
	return err
}

func (app *daemon) replyCard(ctx context.Context, messageID string, route messageRoute, card string) (string, error) {
	start := time.Now()
	if route.kind == kindChat {
		responseID, err := app.lark.SendCard(ctx, app.cfg.OwnerOpenID, card)
		if err != nil {
			app.logError("lark.card.fail", "mode", "direct", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return "", err
		}
		app.debug("lark.card.ok", "mode", "direct", "msg", messageID, "reply", responseID, "key", route.sessionKey, "dur", elapsed(start))
		return responseID, nil
	}
	responseID, err := app.lark.ReplyCard(ctx, messageID, card, true)
	if err != nil {
		app.logError("lark.card.fail", "mode", "thread", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
		return "", err
	}
	app.debug("lark.card.ok", "mode", "thread", "msg", messageID, "reply", responseID, "key", route.sessionKey, "dur", elapsed(start))
	return responseID, nil
}
