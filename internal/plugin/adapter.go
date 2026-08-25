// Package plugin adapts the isolated ACP service to CLIProxyAPI v7's plugin API.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

const Version = "0.1.0"

// Adapter is registered for auth, model, execution, and usage capability
// surfaces. It deliberately never selects an auth candidate itself.
type Adapter struct {
	service  *cursor.Service
	accounts map[string]cursor.Account
}

func New(service *cursor.Service, accounts []cursor.Account) (*Adapter, error) {
	if service == nil {
		return nil, fmt.Errorf("cursor service is required")
	}
	byID := make(map[string]cursor.Account, len(accounts))
	for _, account := range accounts {
		byID[account.AuthID] = account
	}
	return &Adapter{service: service, accounts: byID}, nil
}

func (a *Adapter) Registration() pluginapi.Plugin {
	return pluginapi.Plugin{SchemaVersion: 2, Metadata: pluginapi.Metadata{Name: "cliproxy-cursor-acp", Version: Version, Author: "Malte Sussdorff", GitHubRepository: "https://github.com/sussdorff/cliproxy-cursor-acp"}, Capabilities: pluginapi.Capabilities{
		AuthProvider: a, ModelProvider: a, Executor: a, UsagePlugin: a,
		ExecutorModelScope:   pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"},
	}}
}

func (a *Adapter) Identifier() string { return cursor.ProviderID }

func (a *Adapter) ParseAuth(_ context.Context, request pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	if request.Provider != "" && request.Provider != cursor.ProviderID {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auths := make([]pluginapi.AuthData, 0, len(a.accounts))
	for _, account := range a.accounts {
		auths = append(auths, a.authData(account))
	}
	return pluginapi.AuthParseResponse{Handled: true, Auths: auths}, nil
}

// StartLogin intentionally directs operators to the official CLI. The plugin
// does not read, write, or transport credential material.
func (a *Adapter) StartLogin(_ context.Context, _ pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{Provider: cursor.ProviderID, State: "official-cli-required", ExpiresAt: time.Now().Add(15 * time.Minute), Metadata: map[string]any{"instruction": "Run `CURSOR_CONFIG_DIR=<private-profile> agent login` for the account, then refresh auth."}}, nil
}
func (a *Adapter) PollLogin(_ context.Context, _ pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "authentication remains owned by the official Cursor Agent CLI"}, nil
}
func (a *Adapter) RefreshAuth(_ context.Context, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	account, ok := a.accounts[request.AuthID]
	if !ok {
		return pluginapi.AuthRefreshResponse{}, cursor.ErrUnknownAuth
	}
	return pluginapi.AuthRefreshResponse{Auth: a.authData(account), NextRefreshAfter: time.Now().Add(5 * time.Minute)}, nil
}

func (a *Adapter) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: cursor.ProviderID}, nil
}
func (a *Adapter) ModelsForAuth(_ context.Context, request pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	account, ok := a.accounts[request.AuthID]
	if !ok {
		return pluginapi.ModelResponse{}, cursor.ErrUnknownAuth
	}
	return pluginapi.ModelResponse{Provider: cursor.ProviderID, Models: []pluginapi.ModelInfo{{ID: account.Model, Object: "model", OwnedBy: "cursor", Name: account.Model, DisplayName: account.Model, Type: "chat-completion", SupportedInputModalities: []string{"text"}}}}, nil
}

func (a *Adapter) Execute(ctx context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	prompt, conversationID, workingDir, err := decodeRequest(request.Payload)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	result, err := a.service.Execute(ctx, cursor.Request{AuthID: request.AuthID, ConversationID: conversationID, Prompt: prompt, WorkingDir: workingDir})
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	payload, err := json.Marshal(map[string]any{"id": conversationID, "object": "chat.completion", "model": request.Model, "choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": result.Text}, "finish_reason": "stop"}}, "usage": map[string]int64{"prompt_tokens": result.InputTokens, "completion_tokens": result.OutputTokens, "total_tokens": result.InputTokens + result.OutputTokens}})
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

func (a *Adapter) authData(account cursor.Account) pluginapi.AuthData {
	metadata, err := a.service.Metadata(account.AuthID)
	if err != nil {
		metadata = cursor.Metadata{AuthID: account.AuthID, Label: account.Label, Status: "unavailable", SubscriptionQuotaAvailable: false}
	}
	storage, _ := json.Marshal(map[string]string{"type": cursor.ProviderID, "auth_id": account.AuthID, "profile": "official-cli-private-profile"})
	return pluginapi.AuthData{Provider: cursor.ProviderID, ID: account.AuthID, Label: account.Label, Prefix: "cursor/", StorageJSON: storage, Metadata: map[string]any{"authenticated": metadata.Authenticated, "status": metadata.Status, "subscription_quota_available": false, "exact_subscription_quota": nil, "observed_input_tokens": metadata.ObservedInputTokens, "observed_output_tokens": metadata.ObservedOutputTokens}, Attributes: map[string]string{"model": account.Model}}
}

func decodeRequest(payload []byte) (prompt, conversationID, workingDir string, err error) {
	var request struct {
		ConversationID string `json:"conversation_id"`
		WorkingDir     string `json:"working_directory"`
		Prompt         string `json:"prompt"`
		Messages       []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(payload, &request); err != nil {
		return "", "", "", cursorFailure("invalid_request", "request must be JSON")
	}
	prompt, conversationID, workingDir = request.Prompt, request.ConversationID, request.WorkingDir
	if prompt == "" {
		for _, message := range request.Messages {
			if message.Role == "user" {
				prompt = message.Content
			}
		}
	}
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(workingDir) == "" {
		return "", "", "", cursorFailure("invalid_request", "prompt, conversation_id, and working_directory are required")
	}
	return prompt, conversationID, workingDir, nil
}

func cursorFailure(code, message string) error { return fmt.Errorf("%s: %s", code, message) }
