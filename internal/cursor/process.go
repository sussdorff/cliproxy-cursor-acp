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
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// CommandFactory starts only the official `agent acp` stdio protocol. It does
// not use a shell, private Cursor endpoints, or Cursor credential files.
type CommandFactory struct {
	// Executable pins the official CLI path and wins when it is set. Resolve is
	// used when Executable is empty, because a managed install can appear after
	// the plugin is configured.
	Executable string
	Resolve    func() (string, error)
	BaseEnv    []string
	// Arguments is test-only when non-empty. Production uses exactly "acp".
	Arguments        []string
	TestEnvironment  []string
	ProbeArguments   []string
	ProbeEnvironment []string
	MaxOutputBytes   int
	StartupTimeout   time.Duration
}

// ProfileProber supplies evidence that an official CLI profile is usable. It
// never reads credential files; a successful `agent models` invocation is the
// only evidence used for the pre-provisioned account flow.
type ProfileProber interface {
	Probe(context.Context, Account) (bool, error)
}

func (f CommandFactory) executable() (string, error) {
	if f.Executable != "" {
		return f.Executable, nil
	}
	if f.Resolve == nil {
		return "", ValidationFailure(CodeSetupRequired, "the official Cursor Agent CLI is not installed")
	}
	return f.Resolve()
}

func (f CommandFactory) Probe(ctx context.Context, account Account) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	arguments := f.ProbeArguments
	if len(arguments) == 0 {
		arguments = []string{"models"}
	}
	executable, err := f.executable()
	if err != nil {
		return false, err
	}
	command := exec.Command(executable, arguments...)
	command.Env = append(isolatedEnv(f.BaseEnv, account.ProfileDir), f.ProbeEnvironment...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return false, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return false, err
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	// Cursor can leave a detached worker in the child's group. Reap it on every
	// exit path, not only on timeout, so probes cannot accumulate orphans.
	pgid := command.Process.Pid
	defer terminateRemainingGroup(pgid)
	var out, diagnostics boundedBuffer
	var read sync.WaitGroup
	read.Add(2)
	go func() { defer read.Done(); _, _ = io.Copy(&out, stdout) }()
	go func() { defer read.Done(); _, _ = io.Copy(&diagnostics, stderr) }()
	readDone := make(chan struct{})
	go func() { read.Wait(); close(readDone) }()
	select {
	case <-readDone:
	case <-probeCtx.Done():
		terminateProcessGroup(command)
		select {
		case <-readDone:
		case <-time.After(time.Second):
			if command.Process != nil {
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			}
			<-readDone
		}
	}
	err = command.Wait()
	if err != nil || out.overflow || diagnostics.overflow || len(strings.TrimSpace(out.String())) == 0 {
		return false, fmt.Errorf("official Cursor CLI profile probe failed")
	}
	return true, nil
}

type boundedBuffer struct {
	strings.Builder
	overflow bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	const capBytes = 64 << 10
	remaining := capBytes - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = b.Builder.Write(value[:remaining])
		b.overflow = true
		return len(value), nil
	}
	return b.Builder.Write(value)
}

func terminateProcessGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
}

// acpArguments builds the ACP argv. The model is passed as a single
// "--model=<value>" token so a stored model can never be read as a separate
// flag, on top of the validation Account.validate already applies.
func acpArguments(configured []string, model string) []string {
	if len(configured) > 0 {
		return configured
	}
	arguments := []string{"acp"}
	if model != "" {
		arguments = append(arguments, "--model="+model)
	}
	return arguments
}

// terminateRemainingGroup kills whatever is left of a child's process group
// after the group leader was already reaped, which is where Cursor leaves its
// detached worker. Signalling an empty group is unsafe because the kernel may
// have recycled the id, so the group's existence is probed with signal 0 first
// and the group is left alone when it is already gone.
func terminateRemainingGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	if err := syscall.Kill(-pgid, syscall.Signal(0)); err != nil {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func (f CommandFactory) Start(ctx context.Context, account Account) (ACPClient, error) {
	arguments := acpArguments(f.Arguments, account.Model)
	executable, err := f.executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, arguments...)
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
	maxOutput := f.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	client := &acpProcess{command: command, stdin: stdin, maxOutputBytes: maxOutput}
	client.connection = acp.NewClientSideConnection(client, stdin, stdout)
	startupTimeout := f.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = 30 * time.Second
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if _, err := client.connection.Initialize(startupCtx, acp.InitializeRequest{
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
	command        *exec.Cmd
	stdin          io.WriteCloser
	connection     *acp.ClientSideConnection
	promptMu       sync.Mutex
	mu             sync.Mutex
	collector      *collector
	maxOutputBytes int
	closeOnce      sync.Once
}

type collector struct {
	sessionID string
	text      strings.Builder
	overflow  bool
	active    bool
}

func (p *acpProcess) NewSession(ctx context.Context, cwd string) (string, error) {
	response, err := p.connection.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		return "", err
	}
	return string(response.SessionId), nil
}

func (p *acpProcess) CloseSession(ctx context.Context, sessionID string) error {
	_, err := p.connection.CloseSession(ctx, acp.CloseSessionRequest{SessionId: acp.SessionId(sessionID)})
	return err
}

func (p *acpProcess) Prompt(ctx context.Context, sessionID, prompt string) (Result, error) {
	p.promptMu.Lock()
	defer p.promptMu.Unlock()
	p.mu.Lock()
	p.collector = &collector{sessionID: sessionID, active: true}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.collector != nil {
			p.collector.active = false
			p.collector = nil
		}
		p.mu.Unlock()
	}()
	response, err := p.connection.Prompt(ctx, acp.PromptRequest{SessionId: acp.SessionId(sessionID), Prompt: []acp.ContentBlock{acp.TextBlock(prompt)}})
	if err != nil {
		return Result{}, err
	}
	p.mu.Lock()
	text := p.collector.text.String()
	overflow := p.collector.overflow
	p.mu.Unlock()
	if overflow {
		return Result{}, fmt.Errorf("ACP output exceeded configured limit")
	}
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
		terminateProcessGroup(p.command)
		wait := make(chan error, 1)
		go func() { wait <- p.command.Wait() }()
		select {
		case closeErr = <-wait:
		case <-time.After(time.Second):
			if p.command.Process != nil {
				_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
			}
			closeErr = <-wait
		}
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
		if p.collector != nil && p.collector.active && p.collector.sessionID == string(notification.SessionId) {
			if p.collector.text.Len()+len(text.Content.Text.Text) > p.maxOutputBytes {
				p.collector.overflow = true
			} else {
				p.collector.text.WriteString(text.Content.Text.Text)
			}
		}
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
