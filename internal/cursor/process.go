package cursor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	acp "github.com/coder/acp-go-sdk"
)

// CommandFactory starts only the official `agent acp` stdio protocol. It does
// not use a shell, private Cursor endpoints, or Cursor credential files.
type CommandFactory struct {
	Executable string
	BaseEnv    []string
	// Arguments is test-only when non-empty. Production uses exactly "acp".
	Arguments       []string
	TestEnvironment []string
}

func (f CommandFactory) Start(ctx context.Context, account Account) (ACPClient, error) {
	arguments := f.Arguments
	if len(arguments) == 0 {
		arguments = []string{"acp"}
	}
	command := exec.CommandContext(ctx, f.Executable, arguments...)
	command.Env = append(isolatedEnv(f.BaseEnv, account.ProfileDir), f.TestEnvironment...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Cursor ACP stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Cursor ACP stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Cursor ACP: %w", err)
	}
	client := &acpProcess{command: command, stdin: stdin}
	client.connection = acp.NewClientSideConnection(client, stdin, stdout)
	if _, err := client.connection.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{},
		ClientInfo:         &acp.Implementation{Name: "cliproxy-cursor-acp", Version: "development"},
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize Cursor ACP: %w", err)
	}
	return client, nil
}

func isolatedEnv(base []string, profileDir string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TERM": true, "NO_COLOR": true, "LANG": true, "LC_ALL": true}
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] && name != "CURSOR_API_KEY" && name != "CURSOR_CONFIG_DIR" {
			env = append(env, entry)
		}
	}
	if len(base) == 0 {
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if allowed[name] && name != "CURSOR_API_KEY" && name != "CURSOR_CONFIG_DIR" {
				env = append(env, entry)
			}
		}
	}
	return append(env, "CURSOR_CONFIG_DIR="+profileDir)
}

type acpProcess struct {
	command    *exec.Cmd
	stdin      io.WriteCloser
	connection *acp.ClientSideConnection
	updates    strings.Builder
	mu         sync.Mutex
	closeOnce  sync.Once
}

func (p *acpProcess) NewSession(ctx context.Context, cwd string) (string, error) {
	response, err := p.connection.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		return "", err
	}
	return string(response.SessionId), nil
}

func (p *acpProcess) Prompt(ctx context.Context, sessionID, prompt string) (Result, error) {
	p.mu.Lock()
	p.updates.Reset()
	p.mu.Unlock()
	response, err := p.connection.Prompt(ctx, acp.PromptRequest{SessionId: acp.SessionId(sessionID), Prompt: []acp.ContentBlock{acp.TextBlock(prompt)}})
	if err != nil {
		return Result{}, err
	}
	p.mu.Lock()
	text := p.updates.String()
	p.mu.Unlock()
	result := Result{Text: text}
	if response.Usage != nil {
		result.InputTokens = int64(response.Usage.InputTokens)
		result.OutputTokens = int64(response.Usage.OutputTokens)
	}
	return result, nil
}

func (p *acpProcess) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		if p.command.Process != nil {
			_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
		}
		closeErr = p.command.Wait()
	})
	return closeErr
}

func (p *acpProcess) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, fmt.Errorf("filesystem access disabled")
}
func (p *acpProcess) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, fmt.Errorf("filesystem access disabled")
}
func (p *acpProcess) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}
func (p *acpProcess) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	if text := notification.Update.AgentMessageChunk; text != nil && text.Content.Text != nil {
		p.mu.Lock()
		p.updates.WriteString(text.Content.Text.Text)
		p.mu.Unlock()
	}
	return nil
}
func (p *acpProcess) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("terminal access disabled")
}
func (p *acpProcess) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, fmt.Errorf("terminal access disabled")
}
func (p *acpProcess) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, fmt.Errorf("terminal access disabled")
}
func (p *acpProcess) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, fmt.Errorf("terminal access disabled")
}
func (p *acpProcess) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, fmt.Errorf("terminal access disabled")
}
