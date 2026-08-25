package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/plugin"
	"gopkg.in/yaml.v3"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}
type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}
type adapterState struct {
	adapter *plugin.Adapter
	service *cursor.Service
}

var state struct {
	sync.Mutex
	adapterState
}

func dispatch(method string, raw []byte) ([]byte, bool) {
	value, err := dispatchValue(method, raw)
	if err != nil {
		code, message, status, canRetry := publicError(err)
		return errorEnvelopeStatus(code, message, status, canRetry), true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errorEnvelope("encoding_error", err.Error(), false), true
	}
	result, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: encoded})
	return result, false
}

func safeDispatch(method string, raw []byte) (result []byte, failed bool) {
	defer func() {
		if recover() != nil {
			result = errorEnvelopeStatus("internal_error", "Cursor plugin internal error", http.StatusInternalServerError, false)
			failed = true
		}
	}()
	return dispatch(method, raw)
}
func errorEnvelope(code, message string, canRetry bool) []byte {
	return errorEnvelopeStatus(code, message, http.StatusBadRequest, canRetry)
}
func errorEnvelopeStatus(code, message string, status int, canRetry bool) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: status, Retryable: canRetry}})
	return raw
}
func publicError(err error) (string, string, int, bool) {
	failure := new(cursor.Failure)
	if errorAs(err, failure) {
		if failure.Kind == cursor.FailureRetryable {
			return failure.Code, "Cursor account is temporarily unavailable", http.StatusBadGateway, true
		}
		return failure.Code, "Cursor request was rejected", http.StatusBadRequest, false
	}
	return "cursor_acp_error", "Cursor plugin request failed", http.StatusBadGateway, false
}
func retryable(err error) bool {
	failure := new(cursor.Failure)
	return errorAs(err, failure) && failure.Kind == cursor.FailureRetryable
}
func errorAs(err error, target *cursor.Failure) bool {
	for err != nil {
		if failure, ok := err.(*cursor.Failure); ok {
			*target = *failure
			return true
		}
		type unwrap interface{ Unwrap() error }
		value, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = value.Unwrap()
	}
	return false
}

func dispatchValue(method string, raw []byte) (any, error) {
	if len(raw) > int(maxABIRequestBytes) {
		return nil, fmt.Errorf("request too large")
	}
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		return configure(raw)
	}
	state.Lock()
	adapter := state.adapter
	state.Unlock()
	if adapter == nil {
		return nil, fmt.Errorf("plugin is not configured")
	}
	ctx := context.Background()
	switch method {
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return map[string]any{}, nil
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return map[string]string{"identifier": cursor.ProviderID}, nil
	case pluginabi.MethodAuthParse:
		var request pluginapi.AuthParseRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.ParseAuth(ctx, request)
	case pluginabi.MethodAuthLoginStart:
		var request pluginapi.AuthLoginStartRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.StartLogin(ctx, request)
	case pluginabi.MethodAuthLoginPoll:
		var request pluginapi.AuthLoginPollRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.PollLogin(ctx, request)
	case pluginabi.MethodAuthRefresh:
		var request pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.RefreshAuth(ctx, request)
	case pluginabi.MethodModelStatic:
		return adapter.StaticModels(ctx, pluginapi.StaticModelRequest{})
	case pluginabi.MethodModelForAuth:
		var request pluginapi.AuthModelRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.ModelsForAuth(ctx, request)
	case pluginabi.MethodExecutorExecute:
		var request pluginapi.ExecutorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.Execute(ctx, request)
	case pluginabi.MethodExecutorExecuteStream:
		var request pluginapi.ExecutorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.ExecuteStream(ctx, request)
	case pluginabi.MethodExecutorCountTokens:
		var request pluginapi.ExecutorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.CountTokens(ctx, request)
	case pluginabi.MethodExecutorHTTPRequest:
		var request pluginapi.ExecutorHTTPRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return adapter.HttpRequest(ctx, request)
	case pluginabi.MethodUsageHandle:
		var request pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		adapter.HandleUsage(ctx, request)
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("unsupported plugin method %q", method)
	}
}

func configure(raw []byte) (registration, error) {
	var request lifecycleRequest
	if len(raw) > 0 {
		if len(raw) > 256<<10 {
			return registration{}, fmt.Errorf("configuration request too large")
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return registration{}, err
		}
	}
	var config cursor.Config
	if err := yaml.Unmarshal(request.ConfigYAML, &config); err != nil {
		return registration{}, fmt.Errorf("parse plugin configuration: %w", err)
	}
	config, err := cursor.NormalizeConfig(config)
	if err != nil {
		return registration{}, err
	}
	factory := cursor.CommandFactory{Executable: config.Executable, MaxOutputBytes: config.MaxOutputBytes, StartupTimeout: config.Timeout}
	service, err := cursor.NewService(config, factory)
	if err != nil {
		return registration{}, err
	}
	adapter, err := plugin.New(service, config.Accounts, config.WorkspaceRoot, factory)
	if err != nil {
		_ = service.Close()
		return registration{}, err
	}
	shutdown()
	state.Lock()
	state.adapter = adapter
	state.service = service
	state.Unlock()
	registered := adapter.Registration()
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: registered.Metadata, Capabilities: capabilities{ModelProvider: true, AuthProvider: true, Executor: true, ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, UsagePlugin: true}}, nil
}
func shutdown() {
	state.Lock()
	service := state.service
	state.adapter = nil
	state.service = nil
	state.Unlock()
	if service != nil {
		_ = service.Close()
	}
}
