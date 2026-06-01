package main

import (
	"context"
	"time"
)

func (app *daemon) reactForRoute(ctx context.Context, route messageRoute, messageID string, emoji string) {
	if route.kind != kindTask {
		app.debug("lark.react.skip", "kind", route.kind, "key", route.sessionKey, "msg", messageID, "emoji", emoji)
		return
	}
	go app.react(ctx, messageID, emoji)
}

func (app *daemon) react(ctx context.Context, messageID string, emoji string) {
	start := time.Now()
	if err := app.lark.AddReaction(ctx, messageID, emoji); err != nil {
		app.logError("lark.react.fail", "msg", messageID, "emoji", emoji, "dur", elapsed(start), "err", err)
		return
	}
	app.debug("lark.react.ok", "msg", messageID, "emoji", emoji, "dur", elapsed(start))
}

func (app *daemon) respond(ctx context.Context, messageID string, route messageRoute, text string) error {
	if route.kind == kindChat {
		start := time.Now()
		responseID, err := app.lark.SendText(ctx, app.cfg.OwnerOpenID, text)
		if err != nil {
			app.logError("lark.reply.fail", "mode", "direct", "msg", messageID, "key", route.sessionKey, "dur", elapsed(start), "err", err)
			return err
		}
		app.debug("lark.reply.ok", "mode", "direct", "msg", messageID, "reply", responseID, "key", route.sessionKey, "text_len", len(text), "dur", elapsed(start))
		return nil
	}
	return app.replyThread(ctx, messageID, text)
}

func (app *daemon) replyThread(ctx context.Context, messageID string, text string) error {
	start := time.Now()
	if responseID, err := app.lark.ReplyText(ctx, messageID, text, true); err == nil {
		app.debug("lark.reply.ok", "mode", "thread", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(start))
		return nil
	} else {
		app.logError("lark.reply.fail", "mode", "thread", "msg", messageID, "dur", elapsed(start), "err", err)
	}
	fallbackStart := time.Now()
	responseID, err := app.lark.ReplyText(ctx, messageID, text, false)
	if err != nil {
		app.logError("lark.reply.fail", "mode", "fallback", "msg", messageID, "dur", elapsed(fallbackStart), "err", err)
		return err
	}
	app.debug("lark.reply.ok", "mode", "fallback", "msg", messageID, "reply", responseID, "text_len", len(text), "dur", elapsed(fallbackStart), "total", elapsed(start))
	return nil
}
