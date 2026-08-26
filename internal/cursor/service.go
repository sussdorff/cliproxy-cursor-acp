package cursor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// Accounts arrive at runtime from login completion and stored auth records.
type Service struct {
	mu               sync.Mutex
	accounts         map[string]*accountRuntime
	conversationAuth map[string]string
	factory          Factory
	paths            *Paths
	sem              chan struct{}
	maxPromptBytes   int
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

func NewService(config Config, paths *Paths, factory Factory) (*Service, error) {
	var err error
	config, err = NormalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return nil, fmt.Errorf("ACP factory is required")
	}
	if paths == nil {
		return nil, fmt.Errorf("plugin paths are required")
	}
	return &Service{accounts: make(map[string]*accountRuntime), conversationAuth: make(map[string]string), factory: factory, paths: paths, sem: make(chan struct{}, config.MaxConcurrent), maxPromptBytes: config.MaxPromptBytes, timeout: config.Timeout}, nil
}

// RegisterAccount adds or replaces one runtime account. Registration is the
// only way an AuthID becomes executable. The profile directory must be private,
// owned by the service user, and a direct child of the managed profiles root:
// a stored auth record must never be able to aim the official CLI at the host
// auth directory or any other path the plugin does not own.
func (s *Service) RegisterAccount(account Account) (Account, error) {
	account, err := s.normalizeManagedAccount(account)
	if err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	for authID, runtime := range s.accounts {
		if authID != account.AuthID && runtime.account.ProfileDir == account.ProfileDir {
			s.mu.Unlock()
			return Account{}, fatal("profile_conflict", fmt.Errorf("cursor account %q already owns that profile directory", authID))
		}
	}
	existing := s.accounts[account.AuthID]
	if existing != nil && existing.account.ProfileDir == account.ProfileDir {
		existing.account = account
		s.mu.Unlock()
		return account, nil
	}
	var stale ACPClient
	replaced := ""
	if existing != nil {
		stale = existing.client
		replaced = existing.account.ProfileDir
	}
	s.accounts[account.AuthID] = &accountRuntime{account: account, sessions: make(map[string]string), turn: make(chan struct{}, 1)}
	s.mu.Unlock()
	if stale != nil {
		_ = stale.Close()
	}
	// Re-authenticating one Cursor account moves it to a new profile. The old
	// profile still holds live credential material, so it is removed once its
	// process is gone. Only managed paths are ever deleted.
	if replaced != "" && replaced != account.ProfileDir {
		if _, err = s.managedProfileDir(replaced); err == nil {
			_ = os.RemoveAll(replaced)
		}
	}
	return account, nil
}

// RestoreAccountIfAbsent restores an account from a stored host record only
// when that AuthID has no current runtime. The deciding absence check and
// insertion share one lock so a replay cannot replace a completed login.
func (s *Service) RestoreAccountIfAbsent(account Account) (Account, error) {
	return s.restoreAccountIfAbsent(account, s.normalizeManagedAccount)
}

func (s *Service) restoreAccountIfAbsent(account Account, normalize func(Account) (Account, error)) (Account, error) {
	authID := account.AuthID
	if existing, ok := s.Account(authID); ok {
		return existing, nil
	}
	normalized, err := normalize(account)
	if err != nil {
		if existing, ok := s.Account(authID); ok {
			return existing, nil
		}
		return Account{}, err
	}
	account = normalized
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.accounts[account.AuthID]; existing != nil {
		return existing.account, nil
	}
	for authID, runtime := range s.accounts {
		if authID != account.AuthID && runtime.account.ProfileDir == account.ProfileDir {
			return Account{}, fatal("profile_conflict", fmt.Errorf("cursor account %q already owns that profile directory", authID))
		}
	}
	s.accounts[account.AuthID] = &accountRuntime{account: account, sessions: make(map[string]string), turn: make(chan struct{}, 1)}
	return account, nil
}

func (s *Service) normalizeManagedAccount(account Account) (Account, error) {
	if strings.TrimSpace(account.Model) == "" {
		account.Model = DefaultModel
	}
	if err := account.validate(); err != nil {
		return Account{}, fatal("invalid_account", err)
	}
	profile, err := s.managedProfileDir(account.ProfileDir)
	if err != nil {
		return Account{}, fatal("invalid_profile", fmt.Errorf("cursor account %q profile directory: %w", account.AuthID, err))
	}
	account.ProfileDir = profile
	return account, nil
}

// managedProfileDir canonicalizes a profile directory and requires it to be a
// direct child of the managed profiles root.
func (s *Service) managedProfileDir(path string) (string, error) {
	profile, err := secureDirectory(path)
	if err != nil {
		return "", err
	}
	profiles, err := s.paths.ProfilesRoot()
	if err != nil {
		return "", err
	}
	if filepath.Dir(profile) != profiles {
		return "", fmt.Errorf("must be a directory created by the plugin login flow under %s", profiles)
	}
	return profile, nil
}

// Account returns the registered account for a host-selected AuthID.
func (s *Service) Account(authID string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.accounts[authID]
	if runtime == nil {
		return Account{}, false
	}
	return runtime.account, true
}

// Accounts returns every registered account.
func (s *Service) Accounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]Account, 0, len(s.accounts))
	for _, runtime := range s.accounts {
		accounts = append(accounts, runtime.account)
	}
	return accounts
}

// Workspace returns the working directory offered to every ACP child process.
func (s *Service) Workspace() (string, error) { return s.paths.Workspace() }

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
	workspaceRoot, err := s.paths.Workspace()
	if err != nil {
		return Result{}, err
	}
	if request.WorkingDir == "" {
		request.WorkingDir = workspaceRoot
	}
	if request.WorkingDir != workspaceRoot {
		return Result{}, fatal("invalid_working_directory", fmt.Errorf("working directory must be the plugin workspace root"))
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
	cleanupLimit := s.timeout
	if cleanupLimit > time.Second {
		cleanupLimit = time.Second
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
	err := client.CloseSession(cleanupCtx, sessionID)
	cancel()
	s.mu.Lock()
	if runtime := s.accounts[authID]; runtime != nil && runtime.client == client {
		delete(runtime.sessions, conversationID)
	}
	delete(s.conversationAuth, conversationID)
	s.mu.Unlock()
	if err != nil {
		s.invalidate(authID, client)
	}
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
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "invalid params") || strings.Contains(text, "method not found") || strings.Contains(text, "authentication") || strings.Contains(text, "model") {
		return fatal(code, err)
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
