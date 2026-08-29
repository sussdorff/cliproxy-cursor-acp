package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/plugin"
)

type dispatchStreamFactory struct{}

func (dispatchStreamFactory) Start(context.Context, cursor.Account) (cursor.ACPClient, error) {
	return dispatchStreamACP{}, nil
}

type dispatchStreamACP struct{}

func (dispatchStreamACP) NewSession(context.Context, string) (string, error) {
	return "stream-session", nil
}
func (dispatchStreamACP) Prompt(context.Context, string, string) (cursor.Result, error) {
	return cursor.Result{Text: "stream response", StopReason: "end_turn"}, nil
}
func (client dispatchStreamACP) PromptWithTools(ctx context.Context, sessionID, prompt string, _ cursor.ToolHandler) (cursor.Result, error) {
	return client.Prompt(ctx, sessionID, prompt)
}
func (dispatchStreamACP) Close() error                               { return nil }
func (dispatchStreamACP) CloseSession(context.Context, string) error { return nil }

func TestABIBounds(t *testing.T) {
	if err := validateABIInput("x", maxABIRequestBytes+1); err == nil {
		t.Fatal("oversized request accepted")
	}
	if err := validateABIInput(string(make([]byte, maxABIMethodBytes+1)), 0); err == nil {
		t.Fatal("oversized method accepted")
	}
}

func TestSafeDispatchRecoversPanic(t *testing.T) {
	result, failed := guarded(func() ([]byte, bool) { panic("boom") })
	if !failed || !strings.Contains(string(result), "internal_error") {
		t.Fatalf("panic guard = %s", result)
	}
}

func guarded(call func() ([]byte, bool)) (result []byte, failed bool) {
	defer func() {
		if recover() != nil {
			result = errorEnvelopeStatus("internal_error", "Cursor plugin internal error", 500, false)
			failed = true
		}
	}()
	return call()
}

func TestDispatchRedactsSecretBearingErrors(t *testing.T) {
	_, failed := dispatch("unsupported.method", []byte(`{"token":"secret-value"}`))
	if !failed {
		t.Fatal("unsupported method succeeded")
	}
	raw, _ := dispatch("unsupported.method", []byte(`{"token":"secret-value"}`))
	if string(raw) == "" || strings.Contains(string(raw), "secret-value") {
		t.Fatalf("public error leaked request data: %s", raw)
	}
}

func configureForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(shutdown)
	config := []byte("data_root: " + t.TempDir() + "\ntimeout: 5s\n")
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: config})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := configure(raw)
	if err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d", registration.SchemaVersion)
	}
	capabilities := registration.Capabilities
	if !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.Executor || !capabilities.UsagePlugin || !capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != "openai" {
		t.Fatalf("formats = %#v", capabilities.ExecutorInputFormats)
	}
}

func TestConfigureAcceptsAnOperatorConfigurationWithoutAccounts(t *testing.T) {
	configureForTest(t)
	request, _ := json.Marshal(pluginapi.ExecutorRequest{AuthID: "cursor-a", Format: "openai", Payload: []byte(`{"messages":[]}`)})
	raw, failed := dispatch(pluginabi.MethodExecutorExecute, request)
	if !failed {
		t.Fatal("malformed OpenAI request succeeded")
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.HTTPStatus != 400 {
		t.Fatalf("validation envelope = %s", raw)
	}
	if envelope.Error.Message != "Cursor request was rejected (invalid_request)" {
		t.Fatalf("validation message = %q", envelope.Error.Message)
	}
	if strings.Contains(envelope.Error.Message, "OpenAI request requires text content") {
		t.Fatalf("validation message leaked internal detail: %q", envelope.Error.Message)
	}
}

func TestPublicErrorIncludesFailureCodeWithoutFailureDetail(t *testing.T) {
	const detail = "credential=secret-value path=/private/profile"
	code, message, status, retry := publicError(cursor.ValidationFailure("model_mismatch", detail))
	if code != "model_mismatch" || message != "Cursor request was rejected (model_mismatch)" || status != 400 || retry {
		t.Fatalf("public error = %q, %q, %d, %t", code, message, status, retry)
	}
	if strings.Contains(message, detail) || strings.Contains(message, "secret-value") || strings.Contains(message, "/private/profile") {
		t.Fatalf("public message leaked failure detail: %q", message)
	}
}

func TestConfigureRejectsAnUntrustedExecutable(t *testing.T) {
	t.Cleanup(shutdown)
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("executable: not/absolute\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = configure(raw); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestConfigureRejectsAnUnknownAgentInstallSource(t *testing.T) {
	t.Cleanup(shutdown)
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("agent_install_source: nightly\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = configure(raw); err == nil {
		t.Fatal("unknown agent install source accepted")
	}
}

func TestDispatchServesManagementRegistrationAndSetupPage(t *testing.T) {
	configureForTest(t)
	registerRequest, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{BasePath: "/v0/management", ResourceBasePath: "/v0/resource/plugins/cliproxy-cursor-acp"})
	raw, failed := dispatch(pluginabi.MethodManagementRegister, registerRequest)
	if failed {
		t.Fatalf("management.register failed: %s", raw)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var registration managementRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Routes) != 2 || len(registration.Resources) != 1 {
		t.Fatalf("registration = %s", envelope.Result)
	}
	if registration.Routes[0].Method == "" || registration.Routes[0].Path == "" {
		t.Fatalf("route = %#v", registration.Routes[0])
	}

	handleRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/cliproxy-cursor-acp/setup"})
	raw, failed = dispatch(pluginabi.MethodManagementHandle, handleRequest)
	if failed {
		t.Fatalf("management.handle failed: %s", raw)
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "<!doctype html>") {
		t.Fatalf("setup page = %d %s", response.StatusCode, response.Body)
	}
}

func TestDispatchReportsSetupRequiredOnLoginStart(t *testing.T) {
	configureForTest(t)
	request, _ := json.Marshal(pluginapi.AuthLoginStartRequest{BaseURL: "http://127.0.0.1:8317/v0/management/oauth-callback"})
	raw, failed := dispatch(pluginabi.MethodAuthLoginStart, request)
	if failed {
		t.Fatalf("auth.login.start failed: %s", raw)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Metadata["setup_required"] != true || !strings.HasSuffix(response.URL, "/setup") {
		t.Fatalf("login start = %#v", response)
	}
	if response.State == "" {
		t.Fatal("login start returned no state")
	}
	for _, character := range response.State {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '-', character == '_', character == '.':
		default:
			t.Fatalf("login state %q is rejected by the host state validator", response.State)
		}
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	configureForTest(t)
	shutdown()
	shutdown()
	if _, err := dispatchValue(pluginabi.MethodAuthIdentifier, nil); err == nil {
		t.Fatal("dispatch after shutdown succeeded")
	}
	_ = os.Args
}

func TestDispatchExecutorStreamReturnsJSONAcknowledgement(t *testing.T) {
	installStreamAdapter(t)
	request, err := json.Marshal(pluginapi.ExecutorRequest{
		AuthID:       "cursor-a",
		Model:        "cursor/" + cursor.DefaultModel,
		Format:       "openai",
		SourceFormat: "openai",
		Stream:       true,
		Payload:      []byte(`{"model":"cursor/auto","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var nativeRequest map[string]any
	if err := json.Unmarshal(request, &nativeRequest); err != nil {
		t.Fatal(err)
	}
	nativeRequest["stream_id"] = "host-stream-1"
	nativeRequest["host_callback_id"] = "callback-1"
	request, err = json.Marshal(nativeRequest)
	if err != nil {
		t.Fatal(err)
	}

	type hostCall struct {
		method  string
		request []byte
	}
	var (
		calls []hostCall
		mu    sync.Mutex
	)
	completed := make(chan error, 1)
	acknowledgement, err := dispatchNativeExecutorStreamWithHost(request, func(method string, raw []byte) error {
		mu.Lock()
		calls = append(calls, hostCall{method: method, request: append([]byte(nil), raw...)})
		mu.Unlock()
		return nil
	}, func(err error) {
		completed <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	acknowledgementRaw, err := json.Marshal(acknowledgement)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgementObject map[string]any
	if err := json.Unmarshal(acknowledgementRaw, &acknowledgementObject); err != nil {
		t.Fatal(err)
	}
	if acknowledgement.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("acknowledgement headers = %#v", acknowledgement.Headers)
	}
	if _, ok := acknowledgementObject["chunks"]; ok {
		t.Fatalf("acknowledgement leaked stream chunks: %s", acknowledgementRaw)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native stream forwarder did not complete")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("host calls = %#v", calls)
	}
	for index, call := range calls {
		if call.method != pluginabi.MethodHostStreamEmit && call.method != pluginabi.MethodHostStreamClose {
			t.Fatalf("host call %d method = %q", index, call.method)
		}
		if call.method == pluginabi.MethodHostStreamEmit {
			var emit streamEmitRequest
			if err := json.Unmarshal(call.request, &emit); err != nil {
				t.Fatal(err)
			}
			if emit.StreamID != "host-stream-1" {
				t.Fatalf("emit stream id = %q", emit.StreamID)
			}
		}
	}
	var closeRequest streamCloseRequest
	if err := json.Unmarshal(calls[len(calls)-1].request, &closeRequest); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1].method != pluginabi.MethodHostStreamClose || closeRequest.StreamID != "host-stream-1" {
		t.Fatalf("close call = %#v", calls[len(calls)-1])
	}
}

func TestDispatchExecutorStreamRejectsEmptyStreamID(t *testing.T) {
	installStreamAdapter(t)
	request, err := json.Marshal(pluginapi.ExecutorRequest{AuthID: "cursor-a", Model: "cursor/" + cursor.DefaultModel, Format: "openai", SourceFormat: "openai", Stream: true, Payload: []byte(`{"model":"cursor/auto","stream":true,"messages":[{"role":"user","content":"hello"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchNativeExecutorStreamWithHost(request, func(string, []byte) error { return nil }, nil); err == nil {
		t.Fatal("native stream request without stream_id succeeded")
	}
}

func TestDispatchExecutorExecuteRemainsSynchronous(t *testing.T) {
	installStreamAdapter(t)
	request, err := json.Marshal(pluginapi.ExecutorRequest{AuthID: "cursor-a", Model: "cursor/" + cursor.DefaultModel, Format: "openai", SourceFormat: "openai", Payload: []byte(`{"model":"cursor/auto","messages":[{"role":"user","content":"hello"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, failed := dispatch(pluginabi.MethodExecutorExecute, request)
	if failed {
		t.Fatalf("executor.execute failed: %s", raw)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ExecutorResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Headers.Get("Content-Type") != "application/json" || !json.Valid(response.Payload) {
		t.Fatalf("executor.execute response = %#v", response)
	}
}

func TestForwardNativeStreamEmitsEveryChunkAndClosesOnce(t *testing.T) {
	chunks := make(chan pluginapi.ExecutorStreamChunk, 3)
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("first")}
	chunks <- pluginapi.ExecutorStreamChunk{Err: errors.New("upstream failed")}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("last")}
	close(chunks)

	type hostCall struct {
		method  string
		request []byte
	}
	var (
		calls []hostCall
		mu    sync.Mutex
	)
	closed := make(chan struct{})
	completed := make(chan error, 1)
	forwardNativeStream("host-stream-1", chunks, func(method string, request []byte) error {
		mu.Lock()
		calls = append(calls, hostCall{method: method, request: append([]byte(nil), request...)})
		mu.Unlock()
		if method == pluginabi.MethodHostStreamClose {
			close(closed)
		}
		return nil
	}, func(err error) { completed <- err })

	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native stream forwarder did not complete")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("host stream was not closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("host calls = %#v", calls)
	}
	for index, want := range []string{pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamClose} {
		if calls[index].method != want {
			t.Fatalf("host call %d method = %q, want %q", index, calls[index].method, want)
		}
	}
	var first, streamError, last streamEmitRequest
	if err := json.Unmarshal(calls[0].request, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(calls[1].request, &streamError); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(calls[2].request, &last); err != nil {
		t.Fatal(err)
	}
	if first.StreamID != "host-stream-1" || string(first.Payload) != "first" || first.Error != "" {
		t.Fatalf("first emit = %#v", first)
	}
	if streamError.StreamID != "host-stream-1" || streamError.Error != "Cursor stream failed" {
		t.Fatalf("error emit = %#v", streamError)
	}
	if last.StreamID != "host-stream-1" || string(last.Payload) != "last" || last.Error != "" {
		t.Fatalf("last emit = %#v", last)
	}
	var closeRequest streamCloseRequest
	if err := json.Unmarshal(calls[3].request, &closeRequest); err != nil {
		t.Fatal(err)
	}
	if closeRequest.StreamID != "host-stream-1" || closeRequest.Error != "" {
		t.Fatalf("stream close = %#v", closeRequest)
	}
}

func TestForwardNativeStreamClosesAfterEmitFailure(t *testing.T) {
	chunks := make(chan pluginapi.ExecutorStreamChunk, 2)
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("first")}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("discarded")}
	close(chunks)

	var calls []string
	closed := make(chan streamCloseRequest, 1)
	completed := make(chan error, 1)
	forwardNativeStream("host-stream-1", chunks, func(method string, request []byte) error {
		calls = append(calls, method)
		if method == pluginabi.MethodHostStreamEmit {
			return errors.New("host unavailable")
		}
		var closeRequest streamCloseRequest
		if err := json.Unmarshal(request, &closeRequest); err != nil {
			t.Fatal(err)
		}
		closed <- closeRequest
		return nil
	}, func(err error) { completed <- err })

	select {
	case closeRequest := <-closed:
		if closeRequest.StreamID != "host-stream-1" || closeRequest.Error != "Cursor stream bridge failed" {
			t.Fatalf("stream close = %#v", closeRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("host stream was not closed after emit failure")
	}
	if err := <-completed; err == nil || !strings.Contains(err.Error(), "emit native stream") {
		t.Fatalf("forwarder error = %v", err)
	}
	if strings.Join(calls, ",") != pluginabi.MethodHostStreamEmit+","+pluginabi.MethodHostStreamClose {
		t.Fatalf("host calls = %#v", calls)
	}
}

func TestForwardNativeStreamClosesNilChunksOnce(t *testing.T) {
	var calls []string
	completed := make(chan error, 1)
	forwardNativeStream("host-stream-1", nil, func(method string, request []byte) error {
		calls = append(calls, method)
		return nil
	}, func(err error) { completed <- err })
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != pluginabi.MethodHostStreamClose {
		t.Fatalf("host calls = %#v", calls)
	}
}

func TestForwardNativeStreamReportsCloseFailure(t *testing.T) {
	completed := make(chan error, 1)
	forwardNativeStream("host-stream-1", nil, func(method string, request []byte) error {
		if method != pluginabi.MethodHostStreamClose {
			return fmt.Errorf("host method = %q", method)
		}
		return errors.New("host close unavailable")
	}, func(err error) { completed <- err })
	if err := <-completed; err == nil || !strings.Contains(err.Error(), "close native stream") {
		t.Fatalf("forwarder error = %v", err)
	}
}

func TestNativeStreamLifecyclePreventsNewForwardersAndWaitsForActiveOne(t *testing.T) {
	var lifecycle nativeStreamLifecycle
	release, ok := lifecycle.begin()
	if !ok {
		t.Fatal("first native stream forwarder was rejected")
	}
	lifecycle.stop()
	if _, ok := lifecycle.begin(); ok {
		t.Fatal("native stream forwarder started after shutdown began")
	}
	finished := make(chan struct{})
	go func() {
		lifecycle.wait()
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("native stream lifecycle did not wait for active forwarder")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("native stream lifecycle did not finish after forwarder completed")
	}
}

func installStreamAdapter(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	paths := cursor.NewPaths(root, filepath.Join(root, "workspace"))
	if err := os.MkdirAll(filepath.Join(root, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := cursor.NormalizeConfig(cursor.Config{MaxPromptBytes: 4096, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service, err := cursor.NewService(config, paths, dispatchStreamFactory{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(profiles, "cursor-a")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterAccount(cursor.Account{AuthID: "cursor-a", ProfileDir: profile, Model: cursor.DefaultModel}); err != nil {
		t.Fatal(err)
	}
	adapter, err := plugin.New(plugin.Options{Service: service, Paths: paths, Login: &cursor.Login{}, Installer: &cursor.Installer{Paths: paths}, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	state.Lock()
	previous := state.adapterState
	state.adapter = adapter
	state.service = service
	state.login = nil
	state.Unlock()
	t.Cleanup(func() {
		state.Lock()
		state.adapterState = previous
		state.Unlock()
		_ = service.Close()
	})
}
