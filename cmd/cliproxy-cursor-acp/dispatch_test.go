package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
