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
	StopReason   string
}

type accountRuntime struct {
	account      Account
	client       ACPClient
	sessions     map[string]string
	inputTokens  int64
	outputTokens int64
	turn         chan struct{}
	generation   uint64
}

// Service keeps every ACP process and ACP session inside the selected AuthID.
// It intentionally has no account-selection algorithm: CLIProxyAPI owns that.
// Accounts arrive at runtime from login completion and stored auth records.
type Service struct {
	mu                 sync.Mutex
	accounts           map[string]*accountRuntime
	conversationAuth   map[string]string
	factory            Factory
	paths              *Paths
	sem                chan struct{}
	maxPromptBytes     int
	maxToolResultBytes int
	timeout            time.Duration
	quota              QuotaProvider
	pendingTurns       map[string]*pendingToolTurn
	pendingCalls       map[string]*pendingToolCall
	nextTurn           uint64
	closed             bool
}

// Factory starts a new ACP peer using the selected account only.
type Factory interface {
	Start(context.Context, Account) (ACPClient, error)
}

// ACPClient is the protocol-level session boundary used by the account pool.
type ACPClient interface {
	NewSession(context.Context, string) (string, error)
	Prompt(context.Context, string, string) (Result, error)
	PromptWithTools(context.Context, string, string, ToolHandler) (Result, error)
	CloseSession(context.Context, string) error
	Close() error
}

// ServiceOption adjusts one collaborator after the service is constructed.
type ServiceOption func(*Service)

// WithQuotaProvider replaces the production quota client for hermetic tests.
func WithQuotaProvider(provider QuotaProvider) ServiceOption {
	return func(service *Service) { service.quota = provider }
}

func NewService(config Config, paths *Paths, factory Factory, options ...ServiceOption) (*Service, error) {
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
	service := &Service{accounts: make(map[string]*accountRuntime), conversationAuth: make(map[string]string), factory: factory, paths: paths, sem: make(chan struct{}, config.MaxConcurrent), maxPromptBytes: config.MaxPromptBytes, maxToolResultBytes: config.MaxOutputBytes, timeout: config.Timeout, quota: NewUsageSummaryClient(), pendingTurns: make(map[string]*pendingToolTurn), pendingCalls: make(map[string]*pendingToolCall)}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
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
	return s.execute(ctx, request, nil, nil)
}

func (s *Service) execute(ctx context.Context, request Request, handler ToolHandler, turn *pendingToolTurn) (Result, error) {
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
	if turn != nil {
		s.mu.Lock()
		turn.processGen = runtime.generation
		turn.sessionID = sessionID
		s.mu.Unlock()
	}
	var result Result
	if handler == nil {
		result, err = client.Prompt(executionCtx, sessionID, request.Prompt)
	} else {
		result, err = client.PromptWithTools(executionCtx, sessionID, request.Prompt, handler)
	}
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
		runtime.generation++
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
		runtime.generation++
		runtime.sessions = make(map[string]string)
	}
	s.mu.Unlock()
	_ = client.Close()
}

// StartTurn starts one bounded ACP turn independently of the initiating HTTP
// request. It returns at the next caller-owned tool pause or final result.
func (s *Service) StartTurn(ctx context.Context, request ToolTurnRequest) (ToolTurnEvent, error) {
	if err := ctx.Err(); err != nil {
		return ToolTurnEvent{}, retryable("request_cancelled", err)
	}
	if len(request.Tools) == 0 {
		return ToolTurnEvent{}, fatal("missing_tools", fmt.Errorf("caller tool definitions are required"))
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ToolTurnEvent{}, retryable("service_closed", fmt.Errorf("Cursor ACP service is closed"))
	}
	if existing := s.pendingTurns[request.Request.ConversationID]; existing != nil {
		s.mu.Unlock()
		return ToolTurnEvent{}, fatal("turn_already_pending", fmt.Errorf("conversation already has a pending ACP turn"))
	}
	s.nextTurn++
	turnCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
	turn := &pendingToolTurn{authID: request.Request.AuthID, conversationID: request.Request.ConversationID, generation: s.nextTurn, tools: cloneToolNames(request.Tools), ctx: turnCtx, cancel: cancel, events: make(chan toolTurnOutcome, 1)}
	s.pendingTurns[turn.conversationID] = turn
	s.mu.Unlock()
	go s.runToolTurn(request.Request, turn)
	return s.awaitToolTurn(ctx, turn)
}

// ResumeTurn consumes one opaque result exactly once and resumes only the ACP
// callback that created it.
func (s *Service) ResumeTurn(ctx context.Context, submission ToolResultSubmission) (ToolTurnEvent, error) {
	if submission.CallID == "" {
		return ToolTurnEvent{}, fatal("malformed_tool_result", fmt.Errorf("tool call ID is required"))
	}
	s.mu.Lock()
	call := s.pendingCalls[submission.CallID]
	if call == nil {
		s.mu.Unlock()
		return ToolTurnEvent{}, fatal("unknown_tool_call", fmt.Errorf("tool call is unknown, stale, or already consumed"))
	}
	turn := call.turn
	if turn.authID != submission.AuthID || (submission.ConversationID != "" && turn.conversationID != submission.ConversationID) {
		s.mu.Unlock()
		return ToolTurnEvent{}, fatal("tool_result_mismatch", fmt.Errorf("tool result does not match its AuthID and conversation"))
	}
	runtime := s.accounts[turn.authID]
	currentTurn := s.pendingTurns[turn.conversationID]
	if runtime == nil || runtime.generation != call.processGen || runtime.sessions[turn.conversationID] != call.sessionID || currentTurn != turn || turn.generation != call.turnGen || turn.ctx.Err() != nil {
		delete(s.pendingCalls, submission.CallID)
		s.mu.Unlock()
		return ToolTurnEvent{}, fatal("stale_tool_call", fmt.Errorf("tool result belongs to an expired ACP turn"))
	}
	if len(submission.Result.Output) > s.maxToolResultBytes {
		delete(s.pendingCalls, submission.CallID)
		turn.cancel()
		s.mu.Unlock()
		return ToolTurnEvent{}, fatal("tool_result_too_large", fmt.Errorf("caller tool result exceeds configured maximum"))
	}
	delete(s.pendingCalls, submission.CallID)
	s.mu.Unlock()
	select {
	case call.result <- submission.Result:
	case <-turn.ctx.Done():
		return ToolTurnEvent{}, retryable("turn_cancelled", turn.ctx.Err())
	case <-ctx.Done():
		turn.cancel()
		return ToolTurnEvent{}, retryable("request_cancelled", ctx.Err())
	}
	return s.awaitToolTurn(ctx, turn)
}

func (s *Service) runToolTurn(request Request, turn *pendingToolTurn) {
	result, err := s.execute(turn.ctx, request, boundToolHandler{service: s, turn: turn}, turn)
	outcome := toolTurnOutcome{err: err, final: true}
	if err == nil {
		outcome.event = ToolTurnEvent{ConversationID: turn.conversationID, Result: &result}
	}
	select {
	case turn.events <- outcome:
	case <-turn.ctx.Done():
	}
	s.finishToolTurn(turn)
}

func (s *Service) handleToolCall(ctx context.Context, turn *pendingToolTurn, request ToolRequest) (ToolResult, error) {
	request, err := s.confineToolRequest(turn.authID, request)
	if err != nil {
		return ToolResult{}, err
	}
	name := turn.tools[request.Kind]
	if name == "" {
		return ToolResult{}, fmt.Errorf("caller did not provide an exact mapping for ACP %s", request.Kind)
	}
	callID, err := opaqueToolCallID()
	if err != nil {
		return ToolResult{}, err
	}
	call := &pendingToolCall{turn: turn, turnGen: turn.generation, processGen: turn.processGen, sessionID: turn.sessionID, result: make(chan ToolResult, 1)}
	s.mu.Lock()
	if s.pendingTurns[turn.conversationID] != turn || turn.ctx.Err() != nil {
		s.mu.Unlock()
		return ToolResult{}, fmt.Errorf("ACP turn expired before its tool callback")
	}
	s.pendingCalls[callID] = call
	s.mu.Unlock()
	select {
	case turn.events <- toolTurnOutcome{event: ToolTurnEvent{ConversationID: turn.conversationID, ToolCall: &ToolCall{ID: callID, Name: name, Request: request}}}:
	case <-turn.ctx.Done():
		s.removePendingCall(callID, call)
		return ToolResult{}, turn.ctx.Err()
	case <-ctx.Done():
		s.removePendingCall(callID, call)
		return ToolResult{}, ctx.Err()
	}
	select {
	case result := <-call.result:
		if result.IsError {
			return ToolResult{}, fmt.Errorf("caller refused or failed the ACP tool operation")
		}
		return result, nil
	case <-turn.ctx.Done():
		s.removePendingCall(callID, call)
		return ToolResult{}, turn.ctx.Err()
	case <-ctx.Done():
		s.removePendingCall(callID, call)
		return ToolResult{}, ctx.Err()
	}
}

func (s *Service) confineToolRequest(authID string, request ToolRequest) (ToolRequest, error) {
	workspace, err := s.paths.Workspace()
	if err != nil {
		return ToolRequest{}, err
	}
	switch request.Kind {
	case ToolRead, ToolWrite:
		request.Path, err = confinedPath(workspace, request.Path)
		if err != nil {
			return ToolRequest{}, fmt.Errorf("ACP filesystem callback is outside the caller workspace")
		}
	case ToolShell:
		request.WorkingDir, err = confinedPath(workspace, request.WorkingDir)
		if err != nil {
			return ToolRequest{}, fmt.Errorf("ACP terminal working directory is outside the caller workspace")
		}
		profilesRoot, profilesErr := s.paths.ProfilesRoot()
		if profilesErr != nil {
			return ToolRequest{}, profilesErr
		}
		s.mu.Lock()
		runtime := s.accounts[authID]
		profileDir := ""
		if runtime != nil {
			profileDir = runtime.account.ProfileDir
		}
		s.mu.Unlock()
		for _, value := range append([]string{request.Command}, request.Args...) {
			if strings.Contains(value, profilesRoot) || (profileDir != "" && strings.Contains(value, profileDir)) {
				return ToolRequest{}, fmt.Errorf("ACP terminal callback references private account state")
			}
		}
	default:
		return ToolRequest{}, fmt.Errorf("unsupported ACP caller-owned tool")
	}
	return request, nil
}

func confinedPath(workspace, candidate string) (string, error) {
	if strings.TrimSpace(candidate) == "" {
		candidate = workspace
	} else if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace, candidate)
	}
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(workspace, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return candidate, nil
}

type boundToolHandler struct {
	service *Service
	turn    *pendingToolTurn
}

func (h boundToolHandler) Call(ctx context.Context, request ToolRequest) (ToolResult, error) {
	return h.service.handleToolCall(ctx, h.turn, request)
}

func (s *Service) awaitToolTurn(ctx context.Context, turn *pendingToolTurn) (ToolTurnEvent, error) {
	select {
	case outcome := <-turn.events:
		if outcome.final {
			turn.cancel()
		}
		return outcome.event, outcome.err
	case <-turn.ctx.Done():
		return ToolTurnEvent{}, retryable("turn_cancelled", turn.ctx.Err())
	case <-ctx.Done():
		turn.cancel()
		return ToolTurnEvent{}, retryable("request_cancelled", ctx.Err())
	}
}

func (s *Service) finishToolTurn(turn *pendingToolTurn) {
	s.mu.Lock()
	if s.pendingTurns[turn.conversationID] == turn {
		delete(s.pendingTurns, turn.conversationID)
	}
	for id, call := range s.pendingCalls {
		if call.turn == turn {
			delete(s.pendingCalls, id)
		}
	}
	s.mu.Unlock()
}

func (s *Service) removePendingCall(id string, call *pendingToolCall) {
	s.mu.Lock()
	if s.pendingCalls[id] == call {
		delete(s.pendingCalls, id)
	}
	s.mu.Unlock()
}

func cloneToolNames(source ToolNames) ToolNames {
	result := make(ToolNames, len(source))
	for kind, name := range source {
		result[kind] = name
	}
	return result
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
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	turns := make([]*pendingToolTurn, 0, len(s.pendingTurns))
	for _, turn := range s.pendingTurns {
		turns = append(turns, turn)
	}
	runtimes := make([]*accountRuntime, 0, len(s.accounts))
	for _, runtime := range s.accounts {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, turn := range turns {
		turn.cancel()
	}
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
	s.pendingTurns = make(map[string]*pendingToolTurn)
	s.pendingCalls = make(map[string]*pendingToolCall)
	s.mu.Unlock()
	var joined error
	for _, client := range clients {
		joined = errors.Join(joined, client.Close())
	}
	return joined
}
