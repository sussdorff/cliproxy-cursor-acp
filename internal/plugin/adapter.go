// Package plugin adapts the isolated ACP service to CLIProxyAPI v7's plugin API.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

const Version = "0.3.2"

// authRefreshInterval is how soon the host should call RefreshAuth again.
// The first login record must set this; RefreshAuth then keeps the schedule.
const authRefreshInterval = 5 * time.Minute

func nextAuthRefreshAfter() time.Time {
	return time.Now().Add(authRefreshInterval)
}

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
		ExecutorInputFormats: []string{protocolChat, protocolResponses}, ExecutorOutputFormats: []string{protocolChat, protocolResponses},
	}}
}

func (a *Adapter) Identifier() string { return cursor.ProviderID }

// ParseAuth restores an absent runtime account from a stored auth record. A
// current-process registration wins over a replayed host record so an older
// record cannot replace a profile created by a completed login.
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
	account, err := a.service.RestoreAccountIfAbsent(accountFromStored(stored))
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
	account, err := a.restoreAccountFromStorage(request.AuthID, request.StorageJSON)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	return pluginapi.AuthRefreshResponse{Auth: a.authData(ctx, account), NextRefreshAfter: nextAuthRefreshAfter()}, nil
}

func (a *Adapter) StaticModels(_ context.Context, request pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	return pluginapi.ModelResponse{Provider: cursor.ProviderID}, nil
}

func (a *Adapter) ModelsForAuth(_ context.Context, request pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	a.paths.ObserveHost(request.Host.AuthDir)
	account, err := a.restoreAccountFromStorage(request.AuthID, request.StorageJSON)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return pluginapi.ModelResponse{Provider: cursor.ProviderID, Models: []pluginapi.ModelInfo{{ID: account.Model, Object: "model", OwnedBy: "cursor", Name: account.Model, DisplayName: account.Model, Type: "chat-completion", SupportedInputModalities: []string{"text"}}}}, nil
}

func (a *Adapter) Execute(ctx context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	decoded, event, actualModel, err := a.executeCallerRequest(ctx, request)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	payload, err := marshalCallerResponse(decoded.protocol, decoded.conversationID, actualModel, event)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (a *Adapter) ExecuteStream(ctx context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	decoded, event, actualModel, err := a.executeCallerRequest(ctx, request)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	frames, err := marshalCallerStream(decoded.protocol, decoded.conversationID, actualModel, event)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	chunks := make(chan pluginapi.ExecutorStreamChunk, len(frames))
	for _, frame := range frames {
		chunks <- pluginapi.ExecutorStreamChunk{Payload: frame}
	}
	close(chunks)
	return pluginapi.ExecutorStreamResponse{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, Chunks: chunks}, nil
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

// restoreAccountFromStorage reconstructs an account only from the host's
// selected record. CLIProxyAPI provides this storage on both per-auth model and
// executor calls, including when they arrive before an auth-parse callback.
func (a *Adapter) restoreAccountFromStorage(authID string, storageJSON []byte) (cursor.Account, error) {
	if account, ok := a.service.Account(authID); ok {
		return account, nil
	}
	var stored storedAuth
	if err := json.Unmarshal(storageJSON, &stored); err != nil {
		return cursor.Account{}, cursor.ErrUnknownAuth
	}
	if strings.TrimSpace(authID) == "" || strings.TrimSpace(stored.AuthID) == "" || strings.TrimSpace(stored.ProfileDir) == "" {
		return cursor.Account{}, cursor.ErrUnknownAuth
	}
	if stored.AuthID != authID {
		return cursor.Account{}, cursor.ErrUnknownAuth
	}
	return a.service.RestoreAccountIfAbsent(accountFromStored(stored))
}

const authDataProbeAttempts = 2

func (a *Adapter) authData(ctx context.Context, account cursor.Account) pluginapi.AuthData {
	// A probe can block while a re-login replaces the runtime profile. Re-probe
	// the replacement before serializing so every response field names one
	// account snapshot rather than mixing an old probe with new storage.
	for attempt := 0; attempt < authDataProbeAttempts; attempt++ {
		snapshot, metadata, err := a.service.AccountWithMetadata(account.AuthID)
		if err != nil {
			break
		}
		available := false
		if a.prober != nil {
			available, _ = a.prober.Probe(ctx, snapshot)
		}
		current, _, err := a.service.AccountWithMetadata(snapshot.AuthID)
		if err != nil {
			break
		}
		if current != snapshot {
			account = current
			continue
		}
		// The quota observation also runs outside the service lock, so the
		// account is verified again before its consumption is published.
		metadata = a.service.MetadataForAccount(ctx, snapshot, metadata)
		current, _, err = a.service.AccountWithMetadata(snapshot.AuthID)
		if err == nil && current == snapshot {
			return authDataFromSnapshot(snapshot, metadata, available, true)
		}
		if err != nil {
			break
		}
		account = current
	}

	// Repeated replacements cannot safely inherit a probe result. Return the
	// latest complete snapshot as unavailable instead of combining fields from
	// different profiles.
	if snapshot, metadata, err := a.service.AccountWithMetadata(account.AuthID); err == nil {
		return authDataFromSnapshot(snapshot, metadata, false, false)
	}
	return authDataFromSnapshot(account, cursor.Metadata{AuthID: account.AuthID, Label: account.Label, Status: "unavailable"}, false, false)
}

func authDataFromSnapshot(account cursor.Account, metadata cursor.Metadata, available, stable bool) pluginapi.AuthData {
	storage, _ := json.Marshal(storedAuth{Type: cursor.ProviderID, AuthID: account.AuthID, ProfileDir: account.ProfileDir, Email: account.Email, Label: account.Label, Model: account.Model})
	status := "unavailable"
	if stable && !available {
		status = "unauthenticated"
	}
	if stable && available {
		status = "available"
	}
	// Quota availability is reported independently of credential availability:
	// an unobservable subscription must never remove an account from rotation.
	exactQuota := any(nil)
	if metadata.ExactSubscriptionQuota != nil {
		exactQuota = *metadata.ExactSubscriptionQuota
	}
	return pluginapi.AuthData{
		Provider: cursor.ProviderID, ID: account.AuthID, FileName: account.AuthID + ".json",
		Label: account.Label, Prefix: "cursor", Disabled: !available, StorageJSON: storage,
		NextRefreshAfter: nextAuthRefreshAfter(),
		Metadata: map[string]any{
			"status": status, "subscription_quota_available": metadata.SubscriptionQuotaAvailable, "exact_subscription_quota": exactQuota,
			"observed_input_tokens": metadata.ObservedInputTokens, "observed_output_tokens": metadata.ObservedOutputTokens,
			// profile_dir must be present so a host metadata merge cannot keep a
			// stale path from the previous login of the same AuthID. Quota
			// observation reads only this profile.
			"profile_dir":          account.ProfileDir,
			PluginQuotaMetadataKey: buildPluginQuotaContract(cursor.ProviderID, metadata),
		},
		Attributes: map[string]string{"model": "cursor/" + account.Model},
	}
}

func (a *Adapter) executeCallerRequest(ctx context.Context, request pluginapi.ExecutorRequest) (callerRequest, cursor.ToolTurnEvent, string, error) {
	decoded, err := decodeCallerRequest(request)
	if err != nil {
		return callerRequest{}, cursor.ToolTurnEvent{}, "", err
	}
	account, err := a.restoreAccountFromStorage(request.AuthID, request.StorageJSON)
	if err != nil {
		return callerRequest{}, cursor.ToolTurnEvent{}, "", cursor.ValidationFailure("unknown_auth", "selected account is not registered")
	}
	actualModel := "cursor/" + account.Model
	if request.Model != "" && request.Model != account.Model && request.Model != actualModel {
		return callerRequest{}, cursor.ToolTurnEvent{}, "", cursor.ValidationFailure("model_mismatch", "selected account does not provide requested model")
	}
	if decoded.toolResult != nil {
		event, err := a.service.ResumeTurn(ctx, cursor.ToolResultSubmission{AuthID: request.AuthID, ConversationID: decoded.conversationID, CallID: decoded.toolResult.callID, Result: decoded.toolResult.result})
		if decoded.conversationID == "" {
			decoded.conversationID = event.ConversationID
		}
		return decoded, event, actualModel, err
	}
	serviceRequest := cursor.Request{AuthID: request.AuthID, ConversationID: decoded.conversationID, Prompt: decoded.prompt, Stateless: decoded.stateless}
	if len(decoded.tools) > 0 {
		event, err := a.service.StartTurn(ctx, cursor.ToolTurnRequest{Request: serviceRequest, Tools: decoded.tools})
		return decoded, event, actualModel, err
	}
	result, err := a.service.Execute(ctx, serviceRequest)
	return decoded, cursor.ToolTurnEvent{ConversationID: decoded.conversationID, Result: &result}, actualModel, err
}

func decodeRequest(executor pluginapi.ExecutorRequest) (prompt, conversationID string, stateless bool, err error) {
	decoded, err := decodeCallerRequest(executor)
	if err != nil {
		return "", "", false, err
	}
	return decoded.prompt, decoded.conversationID, decoded.stateless, nil
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
