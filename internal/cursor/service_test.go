package cursor

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	mu              sync.Mutex
	authID, profile string
	sessions        map[string]bool
	closed          bool
	fail            bool
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
func (c *fakeClient) Close() error                               { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *fakeClient) CloseSession(context.Context, string) error { return nil }

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
	service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: 2, MaxPromptBytes: 1024, Accounts: []Account{
		{AuthID: "cursor-a", Label: "A", ProfileDir: profiles + "/a", Model: "auto"},
		{AuthID: "cursor-b", Label: "B", ProfileDir: profiles + "/b", Model: "auto"},
	}, MaxOutputBytes: 1024, WorkspaceRoot: workspace, Timeout: time.Second}, factory)
	if err != nil {
		t.Fatal(err)
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

func TestMetadataReportsObservedUseNotSubscriptionQuota(t *testing.T) {
	service, _ := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-a", ConversationID: "usage", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Metadata("cursor-a")
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
