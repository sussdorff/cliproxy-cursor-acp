package cursor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// ToolKind identifies the narrow caller-owned operations supported by the ACP
// bridge. The plugin forwards these operations and never executes them.
type ToolKind string

const (
	ToolRead  ToolKind = "read"
	ToolWrite ToolKind = "write"
	ToolShell ToolKind = "bash"
)

// ToolRequest is the protocol-neutral form of one official ACP callback.
type ToolRequest struct {
	Kind       ToolKind
	Path       string
	Content    string
	Line       int
	Limit      int
	Command    string
	Args       []string
	WorkingDir string
}

// ToolResult is supplied by the outer caller after its own permission and
// execution policy has completed.
type ToolResult struct {
	Output   string
	IsError  bool
	ExitCode *int
	Signal   *string
}

type ToolHandler interface {
	Call(context.Context, ToolRequest) (ToolResult, error)
}

type ToolHandlerFunc func(context.Context, ToolRequest) (ToolResult, error)

func (f ToolHandlerFunc) Call(ctx context.Context, request ToolRequest) (ToolResult, error) {
	return f(ctx, request)
}

type ToolNames map[ToolKind]string

type ToolTurnRequest struct {
	Request Request
	Tools   ToolNames
}

type ToolCall struct {
	ID      string
	Name    string
	Request ToolRequest
}

type ToolTurnEvent struct {
	ConversationID string
	ToolCall       *ToolCall
	Result         *Result
}

type ToolResultSubmission struct {
	AuthID         string
	ConversationID string
	CallID         string
	Result         ToolResult
}

type pendingToolTurn struct {
	authID         string
	conversationID string
	generation     uint64
	processGen     uint64
	sessionID      string
	tools          ToolNames
	ctx            context.Context
	cancel         context.CancelFunc
	events         chan toolTurnOutcome
	admissionOnce  sync.Once
}

func (t *pendingToolTurn) releaseAdmission(sem chan struct{}) {
	t.admissionOnce.Do(func() { <-sem })
}

type pendingToolCall struct {
	turn       *pendingToolTurn
	turnGen    uint64
	processGen uint64
	sessionID  string
	result     chan ToolResult
}

type toolTurnOutcome struct {
	event ToolTurnEvent
	err   error
	final bool
}

func opaqueToolCallID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create tool call identity: %w", err)
	}
	return "call_" + hex.EncodeToString(value), nil
}
