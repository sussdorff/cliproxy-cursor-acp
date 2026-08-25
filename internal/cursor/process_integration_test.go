package cursor

import (
	"context"
	"os"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestACPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_FAKE_ACP") != "1" {
		return
	}
	agent := &testACPAgent{}
	connection := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.connection = connection
	<-connection.Done()
	os.Exit(0)
}

func TestCommandFactoryUsesStdioACPAndPrivateProfile(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: append(os.Environ(), "CURSOR_API_KEY=must-not-reach-child"), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Prompt(context.Background(), session, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "/private/cursor-a:hello" {
		t.Fatalf("ACP text = %q", result.Text)
	}
	if result.InputTokens != 7 || result.OutputTokens != 11 {
		t.Fatalf("ACP usage = %#v", result)
	}
}

func TestCommandFactoryCancelsAndCleansUpChild(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Prompt(ctx, session, "wait")
	if err == nil {
		t.Fatal("cancelled prompt succeeded")
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child cleanup timed out")
	}
}

func TestCommandFactoryBoundsACPOutput(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}, MaxOutputBytes: 3}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prompt(context.Background(), session, "hello"); err == nil {
		t.Fatal("oversized ACP output succeeded")
	}
}

func TestProbeBufferRejectsLargeOutput(t *testing.T) {
	buffer := &boundedBuffer{}
	_, _ = buffer.Write(make([]byte, (64<<10)+1))
	if !buffer.overflow {
		t.Fatal("large probe output was not bounded")
	}
}

type testACPAgent struct{ connection *acp.AgentSideConnection }

func (a *testACPAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber, AgentCapabilities: acp.AgentCapabilities{}}, nil
}
func (a *testACPAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *testACPAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *testACPAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *testACPAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *testACPAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *testACPAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: "fake-session"}, nil
}
func (a *testACPAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *testACPAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *testACPAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *testACPAgent) Prompt(ctx context.Context, request acp.PromptRequest) (acp.PromptResponse, error) {
	if len(request.Prompt) != 1 || request.Prompt[0].Text == nil {
		return acp.PromptResponse{}, nil
	}
	if request.Prompt[0].Text.Text == "wait" {
		<-ctx.Done()
		return acp.PromptResponse{}, ctx.Err()
	}
	profile := os.Getenv("CURSOR_CONFIG_DIR")
	if err := a.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: request.SessionId, Update: acp.UpdateAgentMessageText(profile + ":" + request.Prompt[0].Text.Text)}); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn, Usage: &acp.Usage{InputTokens: 7, OutputTokens: 11, TotalTokens: 18}}, nil
}
