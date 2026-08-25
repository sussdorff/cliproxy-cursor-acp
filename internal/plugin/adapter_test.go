package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
func (testACP) Close() error                               { return nil }
func (testACP) CloseSession(context.Context, string) error { return nil }

type availableProbe struct{}

func (availableProbe) Probe(context.Context, cursor.Account) (bool, error) { return true, nil }

func testConfig(t *testing.T, accounts []cursor.Account) cursor.Config {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range accounts {
		accounts[index].ProfileDir = filepath.Join(root, accounts[index].AuthID)
		if err := os.MkdirAll(accounts[index].ProfileDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config, err := cursor.NormalizeConfig(cursor.Config{Executable: os.Args[0], Accounts: accounts, MaxConcurrent: 1, MaxPromptBytes: 100, MaxOutputBytes: 100, WorkspaceRoot: workspace, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRegistrationExposesCLIProxyAPICapabilities(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", Label: "Cursor A", ProfileDir: "/profiles/a", Model: "auto"}}
	config := testConfig(t, accounts)
	accounts = config.Accounts
	service, err := cursor.NewService(config, testFactory{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(service, accounts, config.WorkspaceRoot, availableProbe{})
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
	if auth.Auth.Disabled || auth.Auth.Metadata["status"] != "available" {
		t.Fatalf("provisioned account status = %#v", auth.Auth)
	}
}

func TestExecutorUsesHostSelectedAuthID(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", Model: "auto"}}
	config := testConfig(t, accounts)
	accounts = config.Accounts
	service, _ := cursor.NewService(config, testFactory{})
	adapter, _ := New(service, accounts, config.WorkspaceRoot, nil)
	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	_, err := adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "other-account", Payload: payload, Model: "cursor/auto"})
	if err == nil {
		t.Fatal("executor accepted a non-configured selected AuthID")
	}
}

func TestExecutorAcceptsCanonicalOpenAIRequestWithoutPluginFields(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", Model: "auto"}}
	config := testConfig(t, accounts)
	accounts = config.Accounts
	service, err := cursor.NewService(config, testFactory{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := New(service, accounts, config.WorkspaceRoot, availableProbe{})
	payload, _ := json.Marshal(map[string]any{"model": "cursor/auto", "messages": []map[string]string{{"role": "user", "content": "hello"}}})
	response, err := adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "cursor-a", Model: "cursor/auto", Format: "openai", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Payload), "\"content\":\"ok\"") {
		t.Fatalf("response = %s", response.Payload)
	}
}

func TestMetadataFreeRequestsHaveDistinctStatelessSessions(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	_, first, _, err := decodeRequest(pluginapi.ExecutorRequest{AuthID: "cursor-a", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	_, second, _, err := decodeRequest(pluginapi.ExecutorRequest{AuthID: "cursor-a", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("metadata-free requests reused session identity %q", first)
	}
}

func TestPreprovisionedLoginPollReturnsAllAvailableAccounts(t *testing.T) {
	accounts := []cursor.Account{{AuthID: "cursor-a", Model: "auto"}, {AuthID: "cursor-b", Model: "auto"}}
	config := testConfig(t, accounts)
	accounts = config.Accounts
	service, err := cursor.NewService(config, testFactory{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := New(service, accounts, config.WorkspaceRoot, availableProbe{})
	start, err := adapter.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := adapter.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess || len(poll.Auths) != 2 {
		t.Fatalf("host login flow = %#v", poll)
	}
}
