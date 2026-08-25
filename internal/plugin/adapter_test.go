package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

type testFactory struct{}

func (testFactory) Start(context.Context, cursor.Account) (cursor.ACPClient, error) {
	return testACP{}, nil
}

type testACP struct{}

func (testACP) NewSession(context.Context, string) (string, error) { return "session", nil }
func (testACP) Prompt(context.Context, string, string) (cursor.Result, error) {
	return cursor.Result{Text: "ok", InputTokens: 2, OutputTokens: 3}, nil
}
func (testACP) Close() error { return nil }

func TestRegistrationExposesCLIProxyAPICapabilities(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", Label: "Cursor A", ProfileDir: "/profiles/a", Model: "cursor/auto"}}
	service, err := cursor.NewService(cursor.Config{Executable: "agent", Accounts: accounts, MaxConcurrent: 1, MaxPromptBytes: 100}, testFactory{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(service, accounts)
	if err != nil {
		t.Fatal(err)
	}
	registration := adapter.Registration()
	if registration.Capabilities.AuthProvider == nil || registration.Capabilities.ModelProvider == nil || registration.Capabilities.Executor == nil || registration.Capabilities.UsagePlugin == nil {
		t.Fatalf("registration misses required capability: %#v", registration.Capabilities)
	}
	if registration.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("scope = %q", registration.Capabilities.ExecutorModelScope)
	}
	auth, err := adapter.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Auth.Metadata["subscription_quota_available"] != false || auth.Auth.Metadata["exact_subscription_quota"] != nil {
		t.Fatalf("quota metadata = %#v", auth.Auth.Metadata)
	}
}

func TestExecutorUsesHostSelectedAuthID(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", ProfileDir: "/profiles/a", Model: "cursor/auto"}}
	service, _ := cursor.NewService(cursor.Config{Executable: "agent", Accounts: accounts, MaxConcurrent: 1, MaxPromptBytes: 100}, testFactory{})
	adapter, _ := New(service, accounts)
	payload, _ := json.Marshal(map[string]string{"prompt": "hello", "conversation_id": "conversation", "working_directory": "/work"})
	_, err := adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "other-account", Payload: payload, Model: "cursor/auto"})
	if err == nil {
		t.Fatal("executor accepted a non-configured selected AuthID")
	}
}
