package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func main() {
	appID := strings.TrimSpace(os.Getenv("LARK_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("LARK_APP_SECRET"))
	messageID := strings.TrimSpace(os.Getenv("LARK_MESSAGE_ID"))
	replyText := strings.TrimSpace(os.Getenv("LARK_REPLY_TEXT"))
	if appID == "" || appSecret == "" || messageID == "" {
		fmt.Fprintln(os.Stderr, "LARK_APP_ID, LARK_APP_SECRET, and LARK_MESSAGE_ID are required")
		os.Exit(2)
	}

	ctx := context.Background()
	client := lark.NewClient(appID, appSecret)
	for index := 1; index <= 5; index++ {
		start := time.Now()
		resp, err := client.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().
			MessageId(messageID).
			UserIdType("open_id").
			Build())
		duration := time.Since(start)
		if err != nil {
			fmt.Printf("sdk_detail_run=%d dur=%s err=%q\n", index, duration.Round(time.Millisecond), err.Error())
			continue
		}

		hasThread := false
		hasRoot := false
		if resp.Data != nil && len(resp.Data.Items) > 0 {
			item := resp.Data.Items[0]
			hasThread = item.ThreadId != nil && *item.ThreadId != ""
			hasRoot = item.RootId != nil && *item.RootId != ""
		}
		fmt.Printf("sdk_detail_run=%d dur=%s code=%d has_thread=%t has_root=%t\n", index, duration.Round(time.Millisecond), resp.Code, hasThread, hasRoot)
	}
	if replyText == "" {
		return
	}

	content, err := json.Marshal(map[string]string{"text": replyText})
	if err != nil {
		fmt.Printf("sdk_reply err=%q\n", err.Error())
		return
	}
	start := time.Now()
	resp, err := client.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			Content(string(content)).
			MsgType("text").
			ReplyInThread(true).
			Build()).
		Build())
	duration := time.Since(start)
	if err != nil {
		fmt.Printf("sdk_reply dur=%s err=%q\n", duration.Round(time.Millisecond), err.Error())
		return
	}
	replyID := ""
	if resp.Data != nil && resp.Data.MessageId != nil {
		replyID = *resp.Data.MessageId
	}
	fmt.Printf("sdk_reply dur=%s code=%d reply=%s\n", duration.Round(time.Millisecond), resp.Code, replyID)
}
