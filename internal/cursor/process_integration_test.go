package cursor

import (
	"context"
	"errors"
	"os"
	"sync"
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

func TestProbeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_FAKE_PROBE") != "1" {
		return
	}
	if os.Getenv("FAKE_PROBE_MODE") == "large" {
		_, _ = os.Stdout.Write(make([]byte, (64<<10)+1))
		os.Exit(0)
	}
	if os.Getenv("FAKE_PROBE_MODE") == "sleep" {
		time.Sleep(12 * time.Second)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("model\n")
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

func TestCommandFactoryBridgesReadCallbackWithoutReadingWorkspace(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	handler := ToolHandlerFunc(func(_ context.Context, call ToolRequest) (ToolResult, error) {
		if call.Kind != ToolRead || call.Path != "/workspace/main.go" || call.Line != 3 || call.Limit != 7 {
			t.Fatalf("tool call = %#v", call)
		}
		return ToolResult{Output: "caller-owned contents"}, nil
	})
	result, err := client.PromptWithTools(context.Background(), session, "read-tool", handler)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "caller-owned contents" {
		t.Fatalf("ACP text = %q", result.Text)
	}
}

func TestCommandFactoryBridgesWritePermissionAndTerminalCallbacks(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []ToolKind
	handler := ToolHandlerFunc(func(_ context.Context, call ToolRequest) (ToolResult, error) {
		kinds = append(kinds, call.Kind)
		switch call.Kind {
		case ToolWrite:
			if call.Path != "/workspace/out.txt" || call.Content != "new contents" {
				t.Fatalf("write call = %#v", call)
			}
			return ToolResult{Output: "written"}, nil
		case ToolShell:
			if call.Command != "printf" || len(call.Args) != 2 || call.WorkingDir != "/workspace" {
				t.Fatalf("shell call = %#v", call)
			}
			exitCode := 0
			return ToolResult{Output: "terminal output", ExitCode: &exitCode}, nil
		default:
			return ToolResult{}, errors.New("unexpected tool")
		}
	})
	result, err := client.PromptWithTools(context.Background(), session, "tool-sequence", handler)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "terminal output" || len(kinds) != 2 || kinds[0] != ToolWrite || kinds[1] != ToolShell {
		t.Fatalf("result = %#v, kinds = %#v", result, kinds)
	}
}

func TestCommandFactoryRefusesPermissionWithoutSupportedCallerMapping(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	client, err := factory.Start(context.Background(), Account{AuthID: "cursor-a", ProfileDir: "/private/cursor-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.PromptWithTools(context.Background(), session, "unsupported-permission", ToolHandlerFunc(func(context.Context, ToolRequest) (ToolResult, error) {
		return ToolResult{}, errors.New("handler must not be called")
	}))
	if err != nil || result.Text != "permission-cancelled" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestServiceResumesRealStdioACPTurnAfterCallerResult(t *testing.T) {
	root := t.TempDir()
	profiles := root + "/profiles"
	workspace := root + "/workspace"
	profile := profiles + "/cursor-a"
	for _, path := range []string{profile, workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	factory := CommandFactory{Executable: os.Args[0], Arguments: []string{"-test.run=TestACPHelperProcess", "--"}, BaseEnv: os.Environ(), TestEnvironment: []string{"GO_WANT_FAKE_ACP=1"}}
	service, err := NewService(Config{Executable: os.Args[0], MaxConcurrent: 1, MaxPromptBytes: 1024, MaxOutputBytes: 1024, Timeout: time.Second}, NewPaths(root, workspace), factory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-a", ProfileDir: profile, Model: "auto"}); err != nil {
		t.Fatal(err)
	}
	event, err := service.StartTurn(context.Background(), ToolTurnRequest{Request: Request{AuthID: "cursor-a", ConversationID: "stdio-conversation", Prompt: "read-tool"}, Tools: ToolNames{ToolRead: "read"}})
	if err != nil || event.ToolCall == nil {
		t.Fatalf("pause = %#v, %v", event, err)
	}
	final, err := service.ResumeTurn(context.Background(), ToolResultSubmission{AuthID: "cursor-a", ConversationID: "stdio-conversation", CallID: event.ToolCall.ID, Result: ToolResult{Output: "stdio caller contents"}})
	if err != nil || final.Result == nil || final.Result.Text != "stdio caller contents" {
		t.Fatalf("final = %#v, %v", final, err)
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

func TestProbeSupervisionBoundsOutputAndTimeout(t *testing.T) {
	account := Account{ProfileDir: "/private/cursor-a"}
	for _, mode := range []string{"large", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			factory := CommandFactory{Executable: os.Args[0], ProbeArguments: []string{"-test.run=TestProbeHelperProcess", "--"}, ProbeEnvironment: []string{"GO_WANT_FAKE_PROBE=1", "FAKE_PROBE_MODE=" + mode}}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			available, err := factory.Probe(ctx, account)
			if err == nil || available {
				t.Fatalf("probe %s = %v, %v", mode, available, err)
			}
		})
	}
}

func TestProbeSupervisionAcceptsFastOutput(t *testing.T) {
	factory := CommandFactory{Executable: os.Args[0], ProbeArguments: []string{"-test.run=TestProbeHelperProcess", "--"}, ProbeEnvironment: []string{"GO_WANT_FAKE_PROBE=1"}}
	available, err := factory.Probe(context.Background(), Account{ProfileDir: "/private/cursor-a"})
	if err != nil || !available {
		t.Fatalf("fast probe = %v, %v", available, err)
	}
}

type testACPAgent struct {
	connection   *acp.AgentSideConnection
	mu           sync.Mutex
	capabilities acp.ClientCapabilities
	cwds         map[string]string
}

func (a *testACPAgent) Initialize(_ context.Context, request acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.mu.Lock()
	a.capabilities = request.ClientCapabilities
	a.cwds = make(map[string]string)
	a.mu.Unlock()
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
func (a *testACPAgent) NewSession(_ context.Context, request acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	a.cwds["fake-session"] = request.Cwd
	a.mu.Unlock()
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
	if request.Prompt[0].Text.Text == "read-tool" {
		a.mu.Lock()
		capable := a.capabilities.Fs.ReadTextFile
		cwd := a.cwds[string(request.SessionId)]
		a.mu.Unlock()
		if !capable {
			return acp.PromptResponse{}, errors.New("client did not advertise fs/read_text_file")
		}
		line, limit := 3, 7
		result, err := a.connection.ReadTextFile(ctx, acp.ReadTextFileRequest{SessionId: request.SessionId, Path: cwd + "/main.go", Line: &line, Limit: &limit})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if err := a.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: request.SessionId, Update: acp.UpdateAgentMessageText(result.Content)}); err != nil {
			return acp.PromptResponse{}, err
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if request.Prompt[0].Text.Text == "tool-sequence" {
		a.mu.Lock()
		capable := a.capabilities.Fs.WriteTextFile && a.capabilities.Terminal
		cwd := a.cwds[string(request.SessionId)]
		a.mu.Unlock()
		if !capable {
			return acp.PromptResponse{}, errors.New("client did not advertise write and terminal callbacks")
		}
		kind := acp.ToolKindEdit
		permission, err := a.connection.RequestPermission(ctx, acp.RequestPermissionRequest{SessionId: request.SessionId, ToolCall: acp.ToolCallUpdate{ToolCallId: "write-call", Kind: &kind}, Options: []acp.PermissionOption{{OptionId: "allow-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce}, {OptionId: "reject-once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}})
		if err != nil || permission.Outcome.Cancelled == nil {
			return acp.PromptResponse{}, errors.New("ACP permission was not kept outside the plugin")
		}
		if _, err := a.connection.WriteTextFile(ctx, acp.WriteTextFileRequest{SessionId: request.SessionId, Path: cwd + "/out.txt", Content: "new contents"}); err != nil {
			return acp.PromptResponse{}, err
		}
		terminal, err := a.connection.CreateTerminal(ctx, acp.CreateTerminalRequest{SessionId: request.SessionId, Command: "printf", Args: []string{"%s", "terminal output"}, Cwd: &cwd})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		output, err := a.connection.TerminalOutput(ctx, acp.TerminalOutputRequest{SessionId: request.SessionId, TerminalId: terminal.TerminalId})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if _, err := a.connection.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{SessionId: request.SessionId, TerminalId: terminal.TerminalId}); err != nil {
			return acp.PromptResponse{}, err
		}
		if _, err := a.connection.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: request.SessionId, TerminalId: terminal.TerminalId}); err != nil {
			return acp.PromptResponse{}, err
		}
		if err := a.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: request.SessionId, Update: acp.UpdateAgentMessageText(output.Output)}); err != nil {
			return acp.PromptResponse{}, err
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if request.Prompt[0].Text.Text == "unsupported-permission" {
		kind := acp.ToolKindDelete
		permission, err := a.connection.RequestPermission(ctx, acp.RequestPermissionRequest{SessionId: request.SessionId, ToolCall: acp.ToolCallUpdate{ToolCallId: "delete-call", Kind: &kind}, Options: []acp.PermissionOption{{OptionId: "allow-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce}}})
		if err != nil || permission.Outcome.Cancelled == nil {
			return acp.PromptResponse{}, errors.New("unsupported permission was not cancelled")
		}
		if err := a.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: request.SessionId, Update: acp.UpdateAgentMessageText("permission-cancelled")}); err != nil {
			return acp.PromptResponse{}, err
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	profile := os.Getenv("CURSOR_CONFIG_DIR")
	if err := a.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: request.SessionId, Update: acp.UpdateAgentMessageText(profile + ":" + request.Prompt[0].Text.Text)}); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn, Usage: &acp.Usage{InputTokens: 7, OutputTokens: 11, TotalTokens: 18}}, nil
}
