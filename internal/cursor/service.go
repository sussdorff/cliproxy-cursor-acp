package cursor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrUnknownAuth = errors.New("selected Cursor AuthID is not configured")

// Request is the narrow seam between CLIProxyAPI's selected AuthID and ACP.
// ConversationID is a host-owned stable client-conversation key.
type Request struct {
	AuthID         string
	ConversationID string
	Prompt         string
	WorkingDir     string
	Stateless      bool
}

// Result reports ACP output plus optional observed token figures. These values
// are not subscription quota and must never be rendered as a paid balance.
type Result struct {
	Text         string
	InputTokens  int64
	OutputTokens int64
}

type accountRuntime struct {
	account      Account
	client       ACPClient
	sessions     map[string]string
	inputTokens  int64
	outputTokens int64
	turn         chan struct{}
}

// Service keeps every ACP process and ACP session inside the selected AuthID.
// It intentionally has no account-selection algorithm: CLIProxyAPI owns that.
type Service struct {
	mu               sync.Mutex
	accounts         map[string]*accountRuntime
	conversationAuth map[string]string
	factory          Factory
	sem              chan struct{}
	maxPromptBytes   int
	workspaceRoot    string
	timeout          time.Duration
}

// Factory starts a new ACP peer using the selected account only.
type Factory interface {
	Start(context.Context, Account) (ACPClient, error)
}

// ACPClient is the protocol-level session boundary used by the account pool.
type ACPClient interface {
	NewSession(context.Context, string) (string, error)
	Prompt(context.Context, string, string) (Result, error)
	CloseSession(context.Context, string) error
	Close() error
}

func NewService(config Config, factory Factory) (*Service, error) {
	var err error
	config, err = NormalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return nil, fmt.Errorf("ACP factory is required")
	}
	accounts := make(map[string]*accountRuntime, len(config.Accounts))
	for _, account := range config.Accounts {
		accounts[account.AuthID] = &accountRuntime{account: account, sessions: make(map[string]string), turn: make(chan struct{}, 1)}
	}
	return &Service{accounts: accounts, conversationAuth: make(map[string]string), factory: factory, sem: make(chan struct{}, config.MaxConcurrent), maxPromptBytes: config.MaxPromptBytes, workspaceRoot: config.WorkspaceRoot, timeout: config.Timeout}, nil
}

// Execute starts or reuses the ACP session for exactly the selected AuthID.
func (s *Service) Execute(ctx context.Context, request Request) (Result, error) {
	if request.AuthID == "" {
		return Result{}, fatal("missing_auth", fmt.Errorf("CLIProxyAPI must select AuthID before Cursor ACP execution"))
	}
	if request.ConversationID == "" {
		return Result{}, fatal("missing_conversation", fmt.Errorf("conversation ID is required"))
	}
	if len(request.Prompt) > s.maxPromptBytes {
		return Result{}, fatal("prompt_too_large", fmt.Errorf("prompt exceeds configured maximum"))
	}
	if request.WorkingDir == "" {
		request.WorkingDir = s.workspaceRoot
	}
	if request.WorkingDir != s.workspaceRoot {
		return Result{}, fatal("invalid_working_directory", fmt.Errorf("working directory must be configured workspace root"))
	}
	s.mu.Lock()
	runtime := s.accounts[request.AuthID]
	if runtime == nil {
		s.mu.Unlock()
		return Result{}, fatal("unknown_auth", ErrUnknownAuth)
	}
	boundAuth := s.conversationAuth[request.ConversationID]
	if boundAuth != "" && boundAuth != request.AuthID {
		s.mu.Unlock()
		return Result{}, fatal("conversation_account_mismatch", fmt.Errorf("conversation is already bound to selected AuthID %q", boundAuth))
	}
	s.mu.Unlock()
	select {
	case runtime.turn <- struct{}{}:
		defer func() { <-runtime.turn }()
	case <-ctx.Done():
		return Result{}, retryable("request_cancelled", ctx.Err())
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return Result{}, retryable("request_cancelled", ctx.Err())
	}
	executionCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	client, err := s.ensureAccountClient(executionCtx, runtime)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	sessionID := runtime.sessions[request.ConversationID]
	s.mu.Unlock()
	if sessionID == "" {
		sessionID, err = client.NewSession(executionCtx, request.WorkingDir)
		if err != nil {
			s.invalidate(request.AuthID, client)
			return Result{}, classifyACPError("session_start_failed", err)
		}
		s.mu.Lock()
		// A competing request may have created the session. Keeping the first
		// session preserves a single affinity per account/conversation.
		if existing := runtime.sessions[request.ConversationID]; existing != "" {
			sessionID = existing
		} else {
			runtime.sessions[request.ConversationID] = sessionID
		}
		s.mu.Unlock()
	}
	if err := s.commitAffinity(request.ConversationID, request.AuthID); err != nil {
		return Result{}, err
	}
	if request.Stateless {
		defer s.releaseStateless(request.AuthID, request.ConversationID, client, sessionID)
	}
	result, err := client.Prompt(executionCtx, sessionID, request.Prompt)
	if err != nil {
		if executionCtx.Err() != nil {
			s.invalidate(request.AuthID, client)
			return Result{}, retryable("request_cancelled", executionCtx.Err())
		}
		s.invalidate(request.AuthID, client)
		return Result{}, classifyACPError("agent_process_failed", err)
	}
	s.mu.Lock()
	runtime.inputTokens += result.InputTokens
	runtime.outputTokens += result.OutputTokens
	s.mu.Unlock()
	return result, nil
}

func (s *Service) commitAffinity(conversationID, authID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bound := s.conversationAuth[conversationID]; bound != "" && bound != authID {
		return fatal("conversation_account_mismatch", fmt.Errorf("conversation is already bound to selected AuthID %q", bound))
	}
	s.conversationAuth[conversationID] = authID
	return nil
}

func (s *Service) ensureAccountClient(ctx context.Context, runtime *accountRuntime) (ACPClient, error) {
	s.mu.Lock()
	if runtime.client != nil {
		client := runtime.client
		s.mu.Unlock()
		return client, nil
	}
	account := runtime.account
	s.mu.Unlock()

	client, err := s.factory.Start(ctx, account)
	if err != nil {
		return nil, classifyACPError("agent_start_failed", err)
	}
	s.mu.Lock()
	if runtime.client == nil {
		runtime.client = client
		s.mu.Unlock()
		return client, nil
	}
	existing := runtime.client
	s.mu.Unlock()
	_ = client.Close()
	_ = existing
	return existing, nil
}

func (s *Service) releaseStateless(authID, conversationID string, client ACPClient, sessionID string) {
	_ = client.CloseSession(context.Background(), sessionID)
	s.mu.Lock()
	if runtime := s.accounts[authID]; runtime != nil && runtime.client == client {
		delete(runtime.sessions, conversationID)
	}
	delete(s.conversationAuth, conversationID)
	s.mu.Unlock()
}

func (s *Service) invalidate(authID string, client ACPClient) {
	s.mu.Lock()
	runtime := s.accounts[authID]
	if runtime != nil && runtime.client == client {
		runtime.client = nil
		runtime.sessions = make(map[string]string)
	}
	s.mu.Unlock()
	_ = client.Close()
}

func classifyACPError(code string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retryable(code, err)
	}
	return retryable(code, err)
}

// Close terminates all account-owned child processes. It never shares a
// process across AuthIDs and is safe to call repeatedly.
func (s *Service) Close() error {
	s.mu.Lock()
	runtimes := make([]*accountRuntime, 0, len(s.accounts))
	for _, runtime := range s.accounts {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		runtime.turn <- struct{}{}
	}
	defer func() {
		for _, runtime := range runtimes {
			<-runtime.turn
		}
	}()
	s.mu.Lock()
	clients := make([]ACPClient, 0, len(s.accounts))
	for _, runtime := range s.accounts {
		if runtime.client != nil {
			clients = append(clients, runtime.client)
			runtime.client = nil
			runtime.sessions = make(map[string]string)
		}
	}
	s.conversationAuth = make(map[string]string)
	s.mu.Unlock()
	var joined error
	for _, client := range clients {
		joined = errors.Join(joined, client.Close())
	}
	return joined
}
