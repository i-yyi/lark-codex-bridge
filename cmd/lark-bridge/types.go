package main

import (
	"context"
	"log"
	"sync"
	"time"

	"lark-bridge/internal/bridge"
	"lark-bridge/internal/codex"
	configpkg "lark-bridge/internal/config"
	"lark-bridge/internal/lark"
	statepkg "lark-bridge/internal/state"
)

const (
	reactionCommand    = "OK"
	reactionProcessing = "OnIt"
	reactionDone       = "DONE"
	reactionError      = "ERROR"
)

const (
	liveCardUpdateInterval = time.Second
	maxSeenMessages        = 2048
)

type sessionStatus string

const (
	statusIdle    sessionStatus = "idle"
	statusRunning sessionStatus = "running"
	statusPending sessionStatus = "pending"
)

type sessionKind string

const (
	kindChat sessionKind = "chat"
	kindTask sessionKind = "task"
)

const chatSessionKey = "chat:default"

type messageRoute struct {
	sessionKey string
	kind       sessionKind
	reason     string
	detail     lark.MessageDetail
}

type inboundKind string

const (
	inboundPrompt  inboundKind = "prompt"
	inboundCommand inboundKind = "command"
)

type inboundMessage struct {
	kind    inboundKind
	event   lark.MessageEvent
	text    string
	route   messageRoute
	command bridge.Command
}

type sessionRuntime struct {
	key          string
	status       sessionStatus
	threadID     string
	workDir      string
	model        string
	effort       string
	tier         string
	client       *codex.Client
	cancel       context.CancelFunc
	pending      *codex.PendingRequest
	activeTurnID string
	steerBacklog []steerMessage
}

type steerMessage struct {
	messageID string
	text      string
}

type codexModelConfig struct {
	model  string
	effort string
	tier   string
}

type daemon struct {
	cfg       configpkg.Config
	store     statepkg.State
	lark      lark.Client
	logger    *log.Logger
	level     logLevel
	seen      map[string]struct{}
	seenOrder []string

	mu       sync.Mutex
	sessions map[string]*sessionRuntime
	history  []codex.SessionInfo
}
