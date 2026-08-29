package cursor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeFactory struct {
	mu      sync.Mutex
	clients map[string]*fakeClient
}

func (f *fakeFactory) Start(_ context.Context, account Account) (ACPClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clients == nil {
		f.clients = map[string]*fakeClient{}
	}
	client := &fakeClient{authID: account.AuthID, profile: account.ProfileDir, sessions: map[string]bool{}}
	f.clients[account.AuthID] = client
	return client, nil
}

type fakeClient struct {
	mu                sync.Mutex
	authID, profile   string
	sessions          map[string]bool
	closed            bool
	fail              bool
	closedSessions    int
	closeSessionBlock <-chan struct{}
}

func (c *fakeClient) NewSession(_ context.Context, cwd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.fail {
		return "", errors.New("process exited")
	}
	id := fmt.Sprintf("%s:%s:%d", c.authID, cwd, len(c.sessions)+1)
	c.sessions[id] = true
	return id, nil
}
func (c *fakeClient) Prompt(ctx context.Context, sessionID, prompt string) (Result, error) {
	c.mu.Lock()
	valid := c.sessions[sessionID] && !c.closed && !c.fail
	c.mu.Unlock()
	if !valid {
		return Result{}, errors.New("invalid session")
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}
	return Result{Text: c.authID + ":" + prompt, InputTokens: 3, OutputTokens: 5}, nil
}
func (c *fakeClient) PromptWithTools(ctx context.Context, sessionID, prompt string, _ ToolHandler) (Result, error) {
	return c.Prompt(ctx, sessionID, prompt)
}
func (c *fakeClient) Close() error { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *fakeClient) CloseSession(ctx context.Context, _ string) error {
	c.mu.Lock()
	block := c.closeSessionBlock
	c.closedSessions++
	c.mu.Unlock()
	if block == nil {
		return nil
	}
	select {
	case <-block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func testService(t *testing.T) (*Service, *fakeFactory) {
	t.Helper()
	root := t.TempDir()
	profiles := root + "/profiles"
	workspace := root + "/workspace"
	for _, path := range []string{profiles + "/a", profiles + "/b", workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	factory := &fakeFactory{}
	paths := NewPaths(root, workspace)
	service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: 2, MaxPromptBytes: 1024, MaxOutputBytes: 1024, Timeout: time.Second}, paths, factory)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []Account{
		{AuthID: "cursor-a", Label: "A", ProfileDir: profiles + "/a", Model: "auto"},
		{AuthID: "cursor-b", Label: "B", ProfileDir: profiles + "/b", Model: "auto"},
	} {
		if _, errRegister := service.RegisterAccount(account); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return service, factory
}

func TestServiceRequiresSelectedAuthID(t *testing.T) {
	service, _ := testService(t)
	_, err := service.Execute(context.Background(), Request{ConversationID: "conversation", Prompt: "hello", WorkingDir: "/work"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureFatal || failure.Code != "missing_auth" {
		t.Fatalf("error = %#v", err)
	}
}

func TestServiceKeepsAccountsAndSessionsIsolatedUnderConcurrency(t *testing.T) {
	service, factory := testService(t)
	requests := []Request{
		{AuthID: "cursor-a", ConversationID: "conversation-a", Prompt: "one"},
		{AuthID: "cursor-b", ConversationID: "conversation-b", Prompt: "two"},
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(requests))
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Execute(context.Background(), request)
			if err == nil && result.Text[:8] != request.AuthID {
				err = fmt.Errorf("result %q crossed account boundary", result.Text)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for authID, client := range factory.clients {
		if len(client.sessions) != 1 {
			t.Fatalf("%s sessions = %d, want 1", authID, len(client.sessions))
		}
		if client.profile == "/profiles/a" && authID != "cursor-a" {
			t.Fatalf("profile crossed AuthID boundary")
		}
	}

	result, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "conversation-a", Prompt: "again"})
	if err != nil || result.Text != "cursor-a:again" {
		t.Fatalf("affinity result = %#v, %v", result, err)
	}
	if got := len(factory.clients["cursor-a"].sessions); got != 1 {
		t.Fatalf("affinity created %d sessions, want 1", got)
	}
}

func TestServiceRefusesToMigrateConversationToAnotherAccount(t *testing.T) {
	service, _ := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "conversation", Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-b", ConversationID: "conversation", Prompt: "two"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureFatal || failure.Code != "conversation_account_mismatch" {
		t.Fatalf("migration error = %#v", err)
	}
}

type bridgeClient struct {
	*fakeClient
	requests  chan ToolRequest
	workspace string
}

func (c *bridgeClient) PromptWithTools(ctx context.Context, sessionID, prompt string, handler ToolHandler) (Result, error) {
	if prompt == "private-path" {
		_, err := handler.Call(ctx, ToolRequest{Kind: ToolRead, Path: c.profile + "/.cursor/auth.json"})
		return Result{}, err
	}
	call := ToolRequest{Kind: ToolRead, Path: c.workspace + "/main.go", Line: 2, Limit: 5}
	c.requests <- call
	result, err := handler.Call(ctx, call)
	if err != nil {
		return Result{}, err
	}
	if prompt == "two-tools" {
		writeResult, err := handler.Call(ctx, ToolRequest{Kind: ToolWrite, Path: c.workspace + "/out.go", Content: result.Output})
		if err != nil {
			return Result{}, err
		}
		return Result{Text: "wrote:" + writeResult.Output}, nil
	}
	return Result{Text: "read:" + result.Output, InputTokens: 4, OutputTokens: 6}, nil
}

func TestServiceRefusesPrivateProfilePathBeforeCallerDelivery(t *testing.T) {
	service := testBridgeService(t)
	_, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "private-path", Prompt: "private-path"}, Tools: ToolNames{ToolRead: "read"}})
	if err == nil || !strings.Contains(err.Error(), "outside the caller workspace") {
		t.Fatalf("private callback error = %#v", err)
	}
	service.mu.Lock()
	pending := len(service.pendingCalls)
	service.mu.Unlock()
	if pending != 0 {
		t.Fatalf("private callback reached pending caller delivery: %d", pending)
	}
}

type bridgeFactory struct {
	mu        sync.Mutex
	clients   map[string]*bridgeClient
	workspace string
}

func (f *bridgeFactory) Start(_ context.Context, account Account) (ACPClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clients == nil {
		f.clients = make(map[string]*bridgeClient)
	}
	client := &bridgeClient{fakeClient: &fakeClient{authID: account.AuthID, profile: account.ProfileDir, sessions: make(map[string]bool)}, requests: make(chan ToolRequest, 1), workspace: f.workspace}
	f.clients[account.AuthID] = client
	return client, nil
}

func testBridgeService(t *testing.T) *Service {
	return testBridgeServiceWithMaxConcurrent(t, 2)
}

func testBridgeServiceWithMaxConcurrent(t *testing.T, maxConcurrent int) *Service {
	t.Helper()
	root := t.TempDir()
	profiles := root + "/profiles"
	workspace := root + "/workspace"
	for _, path := range []string{profiles + "/a", profiles + "/b", workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := NewPaths(root, workspace)
	canonicalWorkspace, err := paths.Workspace()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: maxConcurrent, MaxPromptBytes: 1024, MaxOutputBytes: 1024, Timeout: time.Second}, paths, &bridgeFactory{workspace: canonicalWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []Account{{AuthID: "cursor-a", ProfileDir: profiles + "/a", Model: "auto"}, {AuthID: "cursor-b", ProfileDir: profiles + "/b", Model: "auto"}} {
		if _, err := service.RegisterAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func TestStartTurnAdmitsCapacityBeforeAllocatingPendingState(t *testing.T) {
	service := testBridgeServiceWithMaxConcurrent(t, 1)
	first, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "admitted", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil || first.ToolCall == nil {
		t.Fatalf("first turn = %#v, %v", first, err)
	}

	type startResult struct {
		event ToolTurnEvent
		err   error
	}
	started := make(chan struct{})
	secondDone := make(chan startResult, 1)
	go func() {
		close(started)
		event, startErr := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-b", ConversationID: "waiting-admission", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
		secondDone <- startResult{event: event, err: startErr}
	}()
	<-started
	select {
	case result := <-secondDone:
		t.Fatalf("excess turn completed before capacity release: %#v, %v", result.event, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	service.mu.Lock()
	pendingBeforeRelease := len(service.pendingTurns)
	secondClientStarted := service.accounts["cursor-b"].client != nil
	service.mu.Unlock()

	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "admitted", CallID: first.ToolCall.ID, Result: ToolResult{Output: "first"}}); err != nil {
		t.Fatal(err)
	}
	second := <-secondDone
	if second.err != nil || second.event.ToolCall == nil {
		t.Fatalf("second turn after release = %#v, %v", second.event, second.err)
	}
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-b", ConversationID: "waiting-admission", CallID: second.event.ToolCall.ID, Result: ToolResult{Output: "second"}}); err != nil {
		t.Fatal(err)
	}
	if pendingBeforeRelease != 1 || secondClientStarted {
		t.Fatalf("excess admission allocated state: pending=%d second_client_started=%t", pendingBeforeRelease, secondClientStarted)
	}
}

func TestStaleCurrentTurnCancelsAndReleasesAdmission(t *testing.T) {
	service := testBridgeServiceWithMaxConcurrent(t, 1)
	stale, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "stale-release", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil || stale.ToolCall == nil {
		t.Fatalf("stale turn = %#v, %v", stale, err)
	}
	service.mu.Lock()
	client := service.accounts["cursor-a"].client
	service.mu.Unlock()
	service.invalidate("cursor-a", client)
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "stale-release", CallID: stale.ToolCall.ID}); FailureCode(err) != "stale_tool_call" {
		t.Fatalf("stale result error = %#v", err)
	}
	eventually(t, 250*time.Millisecond, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.pendingTurns["stale-release"] == nil && len(service.sem) == 0
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	next, err := service.StartTurn(ctx, ToolTurnRequest{Request: Request{AuthID: "cursor-b", ConversationID: "after-stale", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil || next.ToolCall == nil {
		t.Fatalf("turn after stale release = %#v, %v", next, err)
	}
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-b", ConversationID: "after-stale", CallID: next.ToolCall.ID, Result: ToolResult{Output: "next"}}); err != nil {
		t.Fatal(err)
	}
}

func TestServicePausesAndResumesExactPendingToolCall(t *testing.T) {
	service := testBridgeService(t)
	event, err := service.StartTurn(context.Background(), ToolTurnRequest{
		Request: Request{AuthID: "cursor-a", ConversationID: "conversation-a", Prompt: "inspect"},
		Tools:   ToolNames{ToolRead: "read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := service.Workspace()
	if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name != "read" || event.ToolCall.Request.Path != workspace+"/main.go" {
		t.Fatalf("pause event = %#v", event)
	}
	callID := event.ToolCall.ID
	event, err = service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "conversation-a", CallID: callID, Result: ToolResult{Output: "contents"}})
	if err != nil {
		t.Fatal(err)
	}
	if event.Result == nil || event.Result.Text != "read:contents" {
		t.Fatalf("final event = %#v", event)
	}
	_, err = service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "conversation-a", CallID: callID, Result: ToolResult{Output: "duplicate"}})
	if FailureCode(err) != "unknown_tool_call" {
		t.Fatalf("duplicate result error = %#v", err)
	}
}

func TestServicePreservesOneTurnAcrossMultipleToolPauses(t *testing.T) {
	service := testBridgeService(t)
	first, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "multi", Prompt: "two-tools"}, Tools: ToolNames{ToolRead: "read", ToolWrite: "write"}})
	if err != nil || first.ToolCall == nil || first.ToolCall.Name != "read" {
		t.Fatalf("first pause = %#v, %v", first, err)
	}
	second, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "multi", CallID: first.ToolCall.ID, Result: ToolResult{Output: "contents"}})
	if err != nil || second.ToolCall == nil || second.ToolCall.Name != "write" || second.ToolCall.Request.Content != "contents" {
		t.Fatalf("second pause = %#v, %v", second, err)
	}
	final, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "multi", CallID: second.ToolCall.ID, Result: ToolResult{Output: "done"}})
	if err != nil || final.Result == nil || final.Result.Text != "wrote:done" {
		t.Fatalf("final = %#v, %v", final, err)
	}
}

func TestServiceRejectsWrongToolResultBindingWithoutConsumingCall(t *testing.T) {
	service := testBridgeService(t)
	event, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "conversation-a", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, submission := range []ToolResultSubmission{
		{AuthID: "cursor-b", ConversationID: "conversation-a", CallID: event.ToolCall.ID},
		{AuthID: "cursor-a", ConversationID: "conversation-b", CallID: event.ToolCall.ID},
	} {
		if _, err := service.ResumeTurn(context.Background(), submission); FailureCode(err) != "tool_result_mismatch" {
			t.Fatalf("mismatched result error = %#v", err)
		}
	}
	final, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "conversation-a", CallID: event.ToolCall.ID, Result: ToolResult{Output: "safe"}})
	if err != nil || final.Result == nil || final.Result.Text != "read:safe" {
		t.Fatalf("valid result after mismatch = %#v, %v", final, err)
	}
}

func TestServiceKeepsTwoAccountPendingTurnsIsolated(t *testing.T) {
	service := testBridgeService(t)
	type started struct {
		auth  string
		conv  string
		event ToolTurnEvent
		err   error
	}
	startedTurns := make(chan started, 2)
	for _, binding := range []struct{ auth, conversation string }{{"cursor-a", "conversation-a"}, {"cursor-b", "conversation-b"}} {
		binding := binding
		go func() {
			event, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: binding.auth, ConversationID: binding.conversation, Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
			startedTurns <- started{auth: binding.auth, conv: binding.conversation, event: event, err: err}
		}()
	}
	pauses := make([]started, 0, 2)
	for range 2 {
		pause := <-startedTurns
		if pause.err != nil || pause.event.ToolCall == nil {
			t.Fatalf("start = %#v", pause)
		}
		pauses = append(pauses, pause)
	}
	for _, pause := range pauses {
		final, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: pause.auth, ConversationID: pause.conv, CallID: pause.event.ToolCall.ID, Result: ToolResult{Output: pause.auth}})
		if err != nil || final.Result == nil || final.Result.Text != "read:"+pause.auth {
			t.Fatalf("resume %s = %#v, %v", pause.auth, final, err)
		}
	}
}

func TestServiceRejectsStaleAndFailedToolResultsAndCleansState(t *testing.T) {
	service := testBridgeService(t)
	event, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "stale", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	client := service.accounts["cursor-a"].client
	service.mu.Unlock()
	service.invalidate("cursor-a", client)
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "stale", CallID: event.ToolCall.ID}); FailureCode(err) != "stale_tool_call" {
		t.Fatalf("stale result error = %#v", err)
	}

	event, err = service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-b", ConversationID: "failed", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-b", ConversationID: "failed", CallID: event.ToolCall.ID, Result: ToolResult{Output: `{"error":"denied"}`, IsError: true}}); err == nil {
		t.Fatal("failed caller operation resumed ACP successfully")
	}
	eventually(t, time.Second, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.pendingTurns["failed"] == nil && len(service.pendingCalls) == 0
	})
}

func TestServiceBoundsCallerToolResultBeforeACPDelivery(t *testing.T) {
	service := testBridgeService(t)
	service.maxToolResultBytes = 4
	event, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "oversized", Prompt: "inspect"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "oversized", CallID: event.ToolCall.ID, Result: ToolResult{Output: "12345"}}); FailureCode(err) != "tool_result_too_large" {
		t.Fatalf("oversized result error = %#v", err)
	}
}

type waitingFactory struct{}

func (waitingFactory) Start(_ context.Context, account Account) (ACPClient, error) {
	return &waitingClient{fakeClient: &fakeClient{authID: account.AuthID, sessions: make(map[string]bool)}}, nil
}

type waitingClient struct{ *fakeClient }

func (c *waitingClient) PromptWithTools(ctx context.Context, _, _ string, _ ToolHandler) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}

type failingToolFactory struct{}

func (failingToolFactory) Start(_ context.Context, account Account) (ACPClient, error) {
	return &failingToolClient{fakeClient: &fakeClient{authID: account.AuthID, sessions: make(map[string]bool)}}, nil
}

type failingToolClient struct{ *fakeClient }

func (c *failingToolClient) PromptWithTools(context.Context, string, string, ToolHandler) (Result, error) {
	return Result{}, errors.New("child exited")
}

func TestPendingTurnChildFailureUnblocksAndCleansState(t *testing.T) {
	root := t.TempDir()
	profile := root + "/profiles/a"
	workspace := root + "/workspace"
	for _, path := range []string{profile, workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: 1, MaxPromptBytes: 1024, MaxOutputBytes: 1024, Timeout: time.Second}, NewPaths(root, workspace), failingToolFactory{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-a", ProfileDir: profile, Model: "auto"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "child-failure", Prompt: "fail"}, Tools: ToolNames{ToolRead: "read"}}); FailureCode(err) != "agent_process_failed" {
		t.Fatalf("child failure = %#v", err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.pendingTurns) != 0 || len(service.pendingCalls) != 0 || len(service.sem) != 0 || service.accounts["cursor-a"].client != nil {
		t.Fatalf("state survived child failure")
	}
}

func TestPendingTurnCancellationTimeoutAndShutdownCleanState(t *testing.T) {
	for _, mode := range []string{"caller-cancel", "timeout", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			profiles := root + "/profiles"
			workspace := root + "/workspace"
			for _, path := range []string{profiles + "/a", workspace} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: 1, MaxPromptBytes: 1024, MaxOutputBytes: 1024, Timeout: 80 * time.Millisecond}, NewPaths(root, workspace), waitingFactory{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.RegisterAccount(Account{AuthID: "cursor-a", ProfileDir: profiles + "/a", Model: "auto"}); err != nil {
				t.Fatal(err)
			}
			if mode == "shutdown" || mode == "caller-cancel" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, startErr := service.StartTurn(ctx, ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: mode, Prompt: "wait"}, Tools: ToolNames{ToolRead: "read"}})
					done <- startErr
				}()
				eventually(t, time.Second, func() bool {
					service.mu.Lock()
					defer service.mu.Unlock()
					return service.pendingTurns[mode] != nil
				})
				if mode == "shutdown" {
					if err := service.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				if err := <-done; err == nil {
					t.Fatalf("%s did not unblock pending turn", mode)
				}
			} else {
				if _, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: mode, Prompt: "wait"}, Tools: ToolNames{ToolRead: "read"}}); err == nil {
					t.Fatalf("%s did not stop pending turn", mode)
				}
			}
			service.mu.Lock()
			turns, calls, permits := len(service.pendingTurns), len(service.pendingCalls), len(service.sem)
			service.mu.Unlock()
			if turns != 0 || calls != 0 || permits != 0 {
				t.Fatalf("pending state after %s: turns=%d calls=%d permits=%d", mode, turns, calls, permits)
			}
			_ = service.Close()
		})
	}
}

func TestCloseWaitsForTurnCleanupAndPermitRelease(t *testing.T) {
	service := testBridgeServiceWithMaxConcurrent(t, 1)
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &pendingToolTurn{authID: "cursor-a", conversationID: "shutdown-barrier", ctx: turnCtx, cancel: cancel, done: make(chan struct{})}
	service.sem <- struct{}{}
	service.mu.Lock()
	service.pendingTurns[turn.conversationID] = turn
	service.mu.Unlock()

	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	<-turnCtx.Done()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before turn cleanup: %v", err)
	default:
	}

	service.completeToolTurn(turn)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	turns, calls, permits := len(service.pendingTurns), len(service.pendingCalls), len(service.sem)
	service.mu.Unlock()
	if turns != 0 || calls != 0 || permits != 0 {
		t.Fatalf("state after synchronized Close: turns=%d calls=%d permits=%d", turns, calls, permits)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestServiceClassifiesCancellationAndProcessFailureForFailover(t *testing.T) {
	service, factory := testService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.Execute(ctx, Request{AuthID: "cursor-a", ConversationID: "cancelled", Prompt: "hello"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureRetryable {
		t.Fatalf("cancel error = %#v", err)
	}

	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "failed", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	factory.clients["cursor-a"].fail = true
	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "failed", Prompt: "again"})
	if !errors.As(err, &failure) || failure.Kind != FailureRetryable || failure.Code != "agent_process_failed" {
		t.Fatalf("process error = %#v", err)
	}
}

type staticQuotaProvider struct{ quota Quota }

func (p staticQuotaProvider) Fetch(context.Context, QuotaTarget) (Quota, error) { return p.quota, nil }

func TestMetadataReportsSubscriptionQuotaWhenTheProfileObservationSucceeds(t *testing.T) {
	service, _ := testService(t)
	service.quota = staticQuotaProvider{quota: Quota{
		Available: true, WindowStart: "2026-08-01T00:00:00Z", WindowEnd: "2026-09-01T00:00:00Z",
		MembershipType: "pro", LimitType: "monthly", Used: 125, Limit: 500, Remaining: 375,
	}}
	metadata, err := service.Metadata(context.Background(), "cursor-a")
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.SubscriptionQuotaAvailable || metadata.ExactSubscriptionQuota == nil || *metadata.ExactSubscriptionQuota != 375 {
		t.Fatalf("quota metadata = %#v", metadata)
	}
	if metadata.Quota.WindowStart == "" || metadata.Quota.Limit != 500 || metadata.Quota.Used != 125 {
		t.Fatalf("quota data = %#v", metadata.Quota)
	}
	if metadata.ObservedAt == "" {
		t.Fatal("a successful observation must record its observation time")
	}
}

func TestMetadataReportsObservedUseNotSubscriptionQuota(t *testing.T) {
	service, _ := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "usage", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Metadata(context.Background(), "cursor-a")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SubscriptionQuotaAvailable || metadata.ExactSubscriptionQuota != nil {
		t.Fatalf("quota must be explicitly unavailable: %#v", metadata)
	}
	if metadata.ObservedInputTokens != 3 || metadata.ObservedOutputTokens != 5 {
		t.Fatalf("observed usage = %#v", metadata)
	}
}

func TestSameAccountTurnQueueSurvivesConcurrentFailure(t *testing.T) {
	service, factory := testService(t)
	var group sync.WaitGroup
	errorsOut := make(chan error, 8)
	for index := 0; index < 8; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: fmt.Sprintf("stress-%d", index), Prompt: "hello"})
			errorsOut <- err
		}()
	}
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	factory.clients["cursor-a"].fail = true
	_, _ = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "failure", Prompt: "hello"})
}

func TestStatelessTurnClosesSessionAndDoesNotRetainAffinity(t *testing.T) {
	service, factory := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "stateless", Prompt: "hello", Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	client := factory.clients["cursor-a"]
	client.mu.Lock()
	closed := client.closedSessions
	client.mu.Unlock()
	if closed != 1 {
		t.Fatalf("closed sessions = %d", closed)
	}
	service.mu.Lock()
	_, hasSession := service.accounts["cursor-a"].sessions["stateless"]
	_, bound := service.conversationAuth["stateless"]
	service.mu.Unlock()
	if hasSession || bound {
		t.Fatal("stateless turn retained session affinity")
	}
}

func TestBoundConversationSurvivesLaterAccountFailure(t *testing.T) {
	service, factory := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "bound", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	factory.clients["cursor-a"].fail = true
	_, _ = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "bound", Prompt: "again"})
	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-b", ConversationID: "bound", Prompt: "retry"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "conversation_account_mismatch" {
		t.Fatalf("binding was rolled back: %#v", err)
	}
}

func TestStatelessCleanupTimeoutInvalidatesOnlyItsAccountClient(t *testing.T) {
	service, factory := testService(t)
	service.timeout = 40 * time.Millisecond
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "warm", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	first := factory.clients["cursor-a"]
	first.closeSessionBlock = make(chan struct{})
	started := time.Now()
	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "stateless-timeout", Prompt: "hello", Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 300*time.Millisecond {
		t.Fatal("stateless cleanup exceeded bound")
	}
	_, err = service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "next", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if factory.clients["cursor-a"] == first {
		t.Fatal("cleanup failure did not replace account client")
	}
}
