// Package plugin adapts the isolated ACP service to CLIProxyAPI v7's plugin API.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

const Version = "0.2.2"

// Options collects the collaborators an adapter needs. Every account is created
// at runtime by the login flow or reconstructed from a stored auth record.
type Options struct {
	Service   *cursor.Service
	Paths     *cursor.Paths
	Login     *cursor.Login
	Installer *cursor.Installer
	Prober    cursor.ProfileProber
	Config    cursor.Config
}

// Adapter is registered for auth, model, execution, usage, and management
// capability surfaces. It deliberately never selects an auth candidate itself.
type Adapter struct {
	service   *cursor.Service
	paths     *cursor.Paths
	login     *cursor.Login
	installer *cursor.Installer
	prober    cursor.ProfileProber
	config    cursor.Config

	mu           sync.Mutex
	resourceBase string
	setupStates  map[string]time.Time
}

// storedAuth is the plugin-owned auth record. It carries no credential
// material: the official Cursor CLI owns everything inside ProfileDir.
type storedAuth struct {
	Type       string `json:"type"`
	AuthID     string `json:"auth_id"`
	ProfileDir string `json:"profile_dir"`
	Email      string `json:"email,omitempty"`
	Label      string `json:"label,omitempty"`
	Model      string `json:"model,omitempty"`
}

func New(options Options) (*Adapter, error) {
	if options.Service == nil {
		return nil, fmt.Errorf("cursor service is required")
	}
	if options.Paths == nil {
		return nil, fmt.Errorf("plugin paths are required")
	}
	if options.Login == nil {
		return nil, fmt.Errorf("cursor login is required")
	}
	if options.Installer == nil {
		return nil, fmt.Errorf("cursor installer is required")
	}
	return &Adapter{
		service:      options.Service,
		paths:        options.Paths,
		login:        options.Login,
		installer:    options.Installer,
		prober:       options.Prober,
		config:       options.Config,
		resourceBase: defaultResourceBase,
		setupStates:  make(map[string]time.Time),
	}, nil
}

func (a *Adapter) Registration() pluginapi.Plugin {
	return pluginapi.Plugin{SchemaVersion: 2, Metadata: pluginapi.Metadata{Name: "cliproxy-cursor-acp", Version: Version, Author: "Malte Sussdorff", GitHubRepository: "https://github.com/sussdorff/cliproxy-cursor-acp"}, Capabilities: pluginapi.Capabilities{
		AuthProvider: a, ModelProvider: a, Executor: a, UsagePlugin: a, ManagementAPI: a,
		ExecutorModelScope:   pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"},
	}}
}

func (a *Adapter) Identifier() string { return cursor.ProviderID }

// ParseAuth rebuilds one runtime account from a stored auth record. This is the
// only path by which an account survives a host restart.
func (a *Adapter) ParseAuth(ctx context.Context, request pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	if request.Provider != "" && !strings.EqualFold(request.Provider, cursor.ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	var stored storedAuth
	if err := json.Unmarshal(request.RawJSON, &stored); err != nil {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.TrimSpace(stored.AuthID) == "" || strings.TrimSpace(stored.ProfileDir) == "" {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	account, err := a.service.RegisterAccount(accountFromStored(stored))
	if err != nil {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: a.authData(ctx, account)}, nil
}

// StartLogin launches the official Cursor CLI login inside a fresh private
// profile. Without a resolvable CLI it reports a setup-required state instead
// of failing opaquely, so the login card can link the setup page.
func (a *Adapter) StartLogin(ctx context.Context, request pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	start, err := a.login.Start(ctx)
	if err != nil {
		if cursor.FailureCode(err) != cursor.CodeSetupRequired {
			return pluginapi.AuthLoginStartResponse{}, err
		}
		return a.setupRequiredResponse(request.BaseURL)
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  cursor.ProviderID,
		URL:       start.URL,
		State:     start.State,
		ExpiresAt: start.ExpiresAt,
		Metadata: map[string]any{
			"setup_required": false,
			"message":        "Approve the Cursor login in your browser, then this card completes automatically.",
		},
	}, nil
}

func (a *Adapter) PollLogin(ctx context.Context, request pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	if a.isSetupState(request.State) {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "the official Cursor CLI is not installed; open the plugin setup page at " + a.setupPath() + " and install it, then start the login again"}, nil
	}
	result, err := a.login.Poll(ctx, request.State)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "the Cursor login state is unknown or has already finished"}, nil
	}
	if result.Pending {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: result.Message}, nil
	}
	if !result.Authenticated {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: result.Message}, nil
	}
	account, err := a.service.RegisterAccount(result.Account)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "the authenticated Cursor profile could not be registered"}, nil
	}
	auth := a.authData(ctx, account)
	if result.Tier != "" {
		auth.Metadata["subscription_tier"] = result.Tier
	}
	if result.Version != "" {
		auth.Metadata["cli_version"] = result.Version
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "Cursor account authenticated", Auth: auth, Auths: []pluginapi.AuthData{auth}}, nil
}

func (a *Adapter) RefreshAuth(ctx context.Context, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	account, ok := a.service.Account(request.AuthID)
	if !ok {
		var stored storedAuth
		if err := json.Unmarshal(request.StorageJSON, &stored); err != nil || strings.TrimSpace(stored.ProfileDir) == "" {
			return pluginapi.AuthRefreshResponse{}, cursor.ErrUnknownAuth
		}
		registered, err := a.service.RegisterAccount(accountFromStored(stored))
		if err != nil {
			return pluginapi.AuthRefreshResponse{}, err
		}
		account = registered
	}
	return pluginapi.AuthRefreshResponse{Auth: a.authData(ctx, account), NextRefreshAfter: time.Now().Add(5 * time.Minute)}, nil
}

func (a *Adapter) StaticModels(_ context.Context, request pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	return pluginapi.ModelResponse{Provider: cursor.ProviderID}, nil
}

func (a *Adapter) ModelsForAuth(_ context.Context, request pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	account, ok := a.service.Account(request.AuthID)
	if !ok {
		return pluginapi.ModelResponse{}, cursor.ErrUnknownAuth
	}
	return pluginapi.ModelResponse{Provider: cursor.ProviderID, Models: []pluginapi.ModelInfo{{ID: "cursor/" + account.Model, Object: "model", OwnedBy: "cursor", Name: account.Model, DisplayName: account.Model, Type: "chat-completion", SupportedInputModalities: []string{"text"}}}}, nil
}

func (a *Adapter) Execute(ctx context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	prompt, conversationID, stateless, err := decodeRequest(request)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	account, ok := a.service.Account(request.AuthID)
	if !ok {
		return pluginapi.ExecutorResponse{}, cursor.ValidationFailure("unknown_auth", "selected account is not registered")
	}
	actualModel := "cursor/" + account.Model
	if request.Model != "" && request.Model != actualModel {
		return pluginapi.ExecutorResponse{}, cursor.ValidationFailure("model_mismatch", "selected account does not provide requested model")
	}
	result, err := a.service.Execute(ctx, cursor.Request{AuthID: request.AuthID, ConversationID: conversationID, Prompt: prompt, Stateless: stateless})
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	payload, err := json.Marshal(map[string]any{"id": conversationID, "object": "chat.completion", "model": actualModel, "choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": result.Text}, "finish_reason": "stop"}}, "usage": map[string]int64{"prompt_tokens": result.InputTokens, "completion_tokens": result.OutputTokens, "total_tokens": result.InputTokens + result.OutputTokens}})
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (a *Adapter) ExecuteStream(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{}, cursorFailure("streaming_not_yet_available", "Cursor ACP output is currently returned after each completed turn")
}
func (a *Adapter) CountTokens(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, cursorFailure("token_count_unavailable", "Cursor ACP does not provide a preflight token count")
}
func (a *Adapter) HttpRequest(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, cursorFailure("raw_http_forbidden", "Cursor private HTTP endpoints are not exposed")
}
func (a *Adapter) HandleUsage(context.Context, pluginapi.UsageRecord) {}

func accountFromStored(stored storedAuth) cursor.Account {
	model := strings.TrimSpace(stored.Model)
	if model == "" {
		model = cursor.DefaultModel
	}
	label := strings.TrimSpace(stored.Label)
	if label == "" {
		label = firstNonEmpty(stored.Email, stored.AuthID)
	}
	return cursor.Account{AuthID: strings.TrimSpace(stored.AuthID), Label: label, ProfileDir: strings.TrimSpace(stored.ProfileDir), Model: model, Email: strings.TrimSpace(stored.Email)}
}

func (a *Adapter) authData(ctx context.Context, account cursor.Account) pluginapi.AuthData {
	metadata, err := a.service.Metadata(account.AuthID)
	if err != nil {
		metadata = cursor.Metadata{AuthID: account.AuthID, Label: account.Label, Status: "unavailable"}
	}
	available := false
	if a.prober != nil {
		available, _ = a.prober.Probe(ctx, account)
	}
	storage, _ := json.Marshal(storedAuth{Type: cursor.ProviderID, AuthID: account.AuthID, ProfileDir: account.ProfileDir, Email: account.Email, Label: account.Label, Model: account.Model})
	status := "unauthenticated"
	if available {
		status = "available"
	}
	return pluginapi.AuthData{
		Provider: cursor.ProviderID, ID: account.AuthID, FileName: account.AuthID + ".json",
		Label: account.Label, Prefix: "cursor", Disabled: !available, StorageJSON: storage,
		Metadata: map[string]any{
			"status": status, "subscription_quota_available": false, "exact_subscription_quota": nil,
			"observed_input_tokens": metadata.ObservedInputTokens, "observed_output_tokens": metadata.ObservedOutputTokens,
		},
		Attributes: map[string]string{"model": "cursor/" + account.Model},
	}
}

func decodeRequest(executor pluginapi.ExecutorRequest) (prompt, conversationID string, stateless bool, err error) {
	payload := executor.Payload
	var request struct {
		SessionID      string `json:"session_id"`
		ConversationID string `json:"conversation_id"`
		PromptCacheKey string `json:"prompt_cache_key"`
		Prompt         string `json:"prompt"`
		Messages       []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(payload, &request); err != nil {
		return "", "", false, cursorFailure("invalid_request", "request must be JSON")
	}
	prompt = request.Prompt
	for _, message := range request.Messages {
		text, ok := contentText(message.Content)
		if !ok {
			return "", "", false, cursorFailure("invalid_request", "OpenAI message content must be text")
		}
		prompt += "[" + message.Role + "] " + text + "\n"
	}
	if strings.TrimSpace(prompt) == "" {
		return "", "", false, cursorFailure("invalid_request", "OpenAI request requires text content")
	}
	for _, key := range []string{"derived_session_id", "execution_session_id"} {
		if candidate, ok := executor.Metadata[key].(string); ok && strings.TrimSpace(candidate) != "" {
			return prompt, candidate, false, nil
		}
	}
	for _, candidate := range []string{request.SessionID, request.ConversationID, request.PromptCacheKey} {
		if strings.TrimSpace(candidate) != "" {
			return prompt, candidate, false, nil
		}
	}
	{
		identifier := make([]byte, 16)
		if _, err := rand.Read(identifier); err != nil {
			return "", "", false, cursor.ValidationFailure("conversation_id_unavailable", "could not create request identity")
		}
		conversationID = "stateless-" + hex.EncodeToString(identifier)
	}
	return prompt, conversationID, true, nil
}

func contentText(value any) (string, bool) {
	if text, ok := value.(string); ok {
		return text, true
	}
	parts, ok := value.([]any)
	if !ok {
		return "", false
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok {
			return "", false
		}
		kind, _ := object["type"].(string)
		text, _ := object["text"].(string)
		if kind != "text" {
			return "", false
		}
		values = append(values, text)
	}
	return strings.Join(values, "\n"), true
}

func cursorFailure(code, message string) error { return cursor.ValidationFailure(code, message) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
