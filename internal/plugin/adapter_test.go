package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

type testFactory struct {
	mu       sync.Mutex
	profiles map[string]string
}

func (f *testFactory) Start(_ context.Context, account cursor.Account) (cursor.ACPClient, error) {
	f.mu.Lock()
	if f.profiles == nil {
		f.profiles = map[string]string{}
	}
	f.profiles[account.AuthID] = account.ProfileDir
	f.mu.Unlock()
	return testACP{}, nil
}

func (f *testFactory) profile(authID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profiles[authID]
}

type testACP struct{}

func (testACP) NewSession(context.Context, string) (string, error) { return "session", nil }
func (testACP) Prompt(context.Context, string, string) (cursor.Result, error) {
	return cursor.Result{Text: "ok", InputTokens: 2, OutputTokens: 3}, nil
}
func (testACP) Close() error                               { return nil }
func (testACP) CloseSession(context.Context, string) error { return nil }

type availableProbe struct{}

func (availableProbe) Probe(context.Context, cursor.Account) (bool, error) { return true, nil }

// fakeAgentScript stands in for the official Cursor CLI. It performs no network
// access and writes its opaque marker where the real CLI keeps credentials.
const fakeAgentScript = `#!/bin/sh
marker="$CURSOR_CONFIG_DIR/fake-account"
profile_id=${CURSOR_CONFIG_DIR##*/}
case "$1" in
  login)
    test "$NO_OPEN_BROWSER" = "1" || exit 3
    test -z "$CURSOR_API_KEY" || exit 4
    echo "https://cursor.com/loginDeepControl?challenge=$profile_id"
    printf '%s' "user-$profile_id@example.test" > "$marker"
    ;;
  status)
    if test -f "$marker"; then echo '{"isAuthenticated":true}'; else echo '{"isAuthenticated":false}'; fi
    ;;
  about)
    test -f "$marker" || exit 1
    IFS= read -r email < "$marker"
    printf '{"userEmail":"%s","tier":"pro","version":"2026.08.11"}\n' "$email"
    ;;
  *)
    exit 6
    ;;
esac
`

func writeFakeAgent(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte(fakeAgentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type harness struct {
	adapter  *Adapter
	factory  *testFactory
	paths    *cursor.Paths
	service  *cursor.Service
	dataRoot string
}

// newHarness builds an adapter whose Cursor CLI is either absent (executable
// empty) or the hermetic fake agent script.
func newHarness(t *testing.T, executable string) *harness {
	t.Helper()
	return newHarnessAt(t, executable, t.TempDir())
}

// newHarnessAt builds an adapter over an explicit data root, so a host restart
// can be modelled by pointing a second adapter at the first one's data root.
func newHarnessAt(t *testing.T, executable, dataRoot string) *harness {
	t.Helper()
	// PATH is emptied so a developer machine's own Cursor CLI never leaks in.
	t.Setenv("PATH", t.TempDir())
	paths := cursor.NewPaths(dataRoot, "")
	factory := &testFactory{}
	config, err := cursor.NormalizeConfig(cursor.Config{Executable: executable, MaxPromptBytes: 4096, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service, err := cursor.NewService(config, paths, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	login := &cursor.Login{
		Paths:      paths,
		Executable: func() (string, error) { return cursor.ResolveExecutable(config.Executable, paths) },
		URLTimeout: 5 * time.Second,
		Timeout:    15 * time.Second,
	}
	t.Cleanup(login.Close)
	adapter, err := New(Options{
		Service:   service,
		Paths:     paths,
		Login:     login,
		Installer: &cursor.Installer{Paths: paths},
		Prober:    availableProbe{},
		Config:    config,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{adapter: adapter, factory: factory, paths: paths, service: service, dataRoot: dataRoot}
}

func completeLogin(t *testing.T, adapter *Adapter) pluginapi.AuthData {
	t.Helper()
	start, err := adapter.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{BaseURL: "http://127.0.0.1:8317/v0/management/oauth-callback"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(start.URL, "https://cursor.com/") {
		t.Fatalf("approval URL = %q (metadata %#v)", start.URL, start.Metadata)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		poll, errPoll := adapter.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
		if errPoll != nil {
			t.Fatal(errPoll)
		}
		switch poll.Status {
		case pluginapi.AuthLoginStatusSuccess:
			return poll.Auth
		case pluginapi.AuthLoginStatusError:
			t.Fatalf("login failed: %s", poll.Message)
		}
		if time.Now().After(deadline) {
			t.Fatal("login never completed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRegistrationExposesCLIProxyAPICapabilities(t *testing.T) {
	registration := newHarness(t, "").adapter.Registration()
	capabilities := registration.Capabilities
	if capabilities.AuthProvider == nil || capabilities.ModelProvider == nil || capabilities.Executor == nil || capabilities.UsagePlugin == nil || capabilities.ManagementAPI == nil {
		t.Fatalf("registration misses required capability: %#v", capabilities)
	}
	if capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("scope = %q", capabilities.ExecutorModelScope)
	}
	if registration.Metadata.Version != Version {
		t.Fatalf("metadata version = %q", registration.Metadata.Version)
	}
}

func TestInstallFailureMessageExplainsMissingArtifactPin(t *testing.T) {
	err := cursor.ValidationFailure("agent_package_pin_required", "agent_package_sha256 is required")
	if got := installFailureMessage(err); !strings.Contains(got, "agent_package_sha256") {
		t.Fatalf("missing pin message = %q", got)
	}
}

func TestStartLoginWithoutCursorCLIPointsAtTheSetupPage(t *testing.T) {
	harness := newHarness(t, "")
	start, err := harness.adapter.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{BaseURL: "http://127.0.0.1:8317/v0/management/oauth-callback"})
	if err != nil {
		t.Fatalf("setup-required login start must not fail opaquely: %v", err)
	}
	if start.Metadata["setup_required"] != true {
		t.Fatalf("metadata = %#v", start.Metadata)
	}
	setupURL, _ := start.Metadata["setup_url"].(string)
	if !strings.HasSuffix(setupURL, "/setup") || !strings.HasSuffix(start.URL, "/setup") {
		t.Fatalf("setup url = %q, login url = %q", setupURL, start.URL)
	}
	if start.State == "" || start.Provider != cursor.ProviderID {
		t.Fatalf("start = %#v", start)
	}
	poll, err := harness.adapter.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "setup") {
		t.Fatalf("poll = %#v", poll)
	}
}

func TestLoginCreatesAccountBoundToItsOwnPrivateProfile(t *testing.T) {
	harness := newHarness(t, writeFakeAgent(t))
	auth := completeLogin(t, harness.adapter)
	if auth.Provider != cursor.ProviderID || auth.Prefix != "cursor" || auth.Disabled {
		t.Fatalf("auth = %#v", auth)
	}
	var stored map[string]any
	if err := json.Unmarshal(auth.StorageJSON, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["type"] != cursor.ProviderID || stored["auth_id"] != auth.ID {
		t.Fatalf("storage = %#v", stored)
	}
	profileDir, _ := stored["profile_dir"].(string)
	if profileDir == "" {
		t.Fatalf("storage carries no profile directory: %#v", stored)
	}
	for _, forbidden := range []string{"token", "access_token", "refresh_token", "cookie", "credential"} {
		if _, present := stored[forbidden]; present {
			t.Fatalf("storage carries credential material %q", forbidden)
		}
	}
	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	if _, err := harness.adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: auth.ID, Model: "cursor/auto", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got := harness.factory.profile(auth.ID); got != profileDir {
		t.Fatalf("execution ran under %q, stored record names %q", got, profileDir)
	}
}

func TestSecondLoginProducesASecondAccount(t *testing.T) {
	harness := newHarness(t, writeFakeAgent(t))
	first := completeLogin(t, harness.adapter)
	second := completeLogin(t, harness.adapter)
	if first.ID == second.ID || first.Label == second.Label {
		t.Fatalf("second login reused account %q", first.ID)
	}
	if len(harness.service.Accounts()) != 2 {
		t.Fatalf("registered accounts = %d, want 2", len(harness.service.Accounts()))
	}
}

func TestParseAuthReconstructsAccountsAfterHostRestart(t *testing.T) {
	origin := newHarness(t, writeFakeAgent(t))
	first := completeLogin(t, origin.adapter)
	second := completeLogin(t, origin.adapter)

	// A restart re-resolves the same data root, so the stored profile
	// directories are still the ones this plugin owns.
	restarted := newHarnessAt(t, "", origin.dataRoot)
	for _, auth := range []pluginapi.AuthData{first, second} {
		response, err := restarted.adapter.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: cursor.ProviderID, RawJSON: auth.StorageJSON})
		if err != nil {
			t.Fatal(err)
		}
		if !response.Handled || response.Auth.ID != auth.ID {
			t.Fatalf("parse = %#v", response)
		}
		models, errModels := restarted.adapter.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: auth.ID})
		if errModels != nil {
			t.Fatal(errModels)
		}
		if len(models.Models) != 1 || models.Models[0].ID != "cursor/"+cursor.DefaultModel {
			t.Fatalf("models = %#v", models.Models)
		}
	}

	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	if _, err := restarted.adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: second.ID, Model: "cursor/auto", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.factory.profile(second.ID); got != mustProfileDir(t, second) {
		t.Fatalf("execution ran under %q, stored record names %q", got, mustProfileDir(t, second))
	}
	if _, err := restarted.adapter.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "cursor-unknown", Model: "cursor/auto", Payload: payload}); err == nil {
		t.Fatal("executor accepted an AuthID that was never parsed")
	}
	if _, err := restarted.adapter.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "cursor-unknown"}); err == nil {
		t.Fatal("model discovery accepted an unknown AuthID")
	}
}

func TestParseAuthDoesNotReplaceCurrentAccountWithStaleStoredProfile(t *testing.T) {
	harness := newHarness(t, writeFakeAgent(t))
	currentAuth := completeLogin(t, harness.adapter)
	currentProfile := mustProfileDir(t, currentAuth)

	profiles, err := harness.paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	staleProfile := filepath.Join(profiles, "stale-host-record")
	if err = os.MkdirAll(staleProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(staleProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	var stale storedAuth
	if err = json.Unmarshal(currentAuth.StorageJSON, &stale); err != nil {
		t.Fatal(err)
	}
	stale.ProfileDir = staleProfile
	staleRaw, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}

	response, err := harness.adapter.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: cursor.ProviderID, RawJSON: staleRaw})
	if err != nil || !response.Handled {
		t.Fatalf("ParseAuth() = %#v, %v", response, err)
	}
	account, ok := harness.service.Account(currentAuth.ID)
	if !ok || account.ProfileDir != currentProfile {
		t.Fatalf("current account = %#v, want profile %q", account, currentProfile)
	}
	if got := mustProfileDir(t, response.Auth); got != currentProfile {
		t.Fatalf("returned storage profile = %q, want current profile %q", got, currentProfile)
	}
	if _, err = os.Stat(currentProfile); err != nil {
		t.Fatalf("current profile was removed by stale replay: %v", err)
	}
}

func TestParseAuthIgnoresForeignAndIncompleteRecords(t *testing.T) {
	harness := newHarness(t, "")
	cases := []pluginapi.AuthParseRequest{
		{Provider: "codex", RawJSON: []byte(`{"type":"codex"}`)},
		{Provider: cursor.ProviderID, RawJSON: []byte(`{"type":"cursor-acp"}`)},
		{Provider: cursor.ProviderID, RawJSON: []byte(`{"type":"cursor-acp","auth_id":"cursor-a","profile_dir":"relative"}`)},
		{Provider: cursor.ProviderID, RawJSON: []byte(`not json`)},
	}
	for index, request := range cases {
		response, err := harness.adapter.ParseAuth(context.Background(), request)
		if err != nil || response.Handled {
			t.Fatalf("case %d = %#v %v", index, response, err)
		}
	}
}

func TestParseAuthRefusesRecordsAimedOutsideTheManagedProfilesRoot(t *testing.T) {
	harness := newHarness(t, "")
	authDir := t.TempDir()
	harness.paths.ObserveHost(authDir)
	root, err := harness.paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "stolen")
	if err = os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	// A stored record must not be able to point the Cursor CLI at the host auth
	// directory, the plugin data root, or any unrelated private directory.
	for _, profile := range []string{authDir, root, elsewhere} {
		record, _ := json.Marshal(map[string]string{"type": cursor.ProviderID, "auth_id": "cursor-escape", "profile_dir": profile})
		response, errParse := harness.adapter.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: cursor.ProviderID, RawJSON: record})
		if errParse != nil || response.Handled {
			t.Fatalf("record pointing at %q = %#v %v", profile, response, errParse)
		}
		if _, errModels := harness.adapter.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "cursor-escape"}); errModels == nil {
			t.Fatalf("record pointing at %q became executable", profile)
		}
	}
}

func TestSetupPageIsServedWithoutTouchingHostState(t *testing.T) {
	// A data root that does not exist yet: serving the unauthenticated page must
	// not create it, probe the installer, or scan PATH.
	dataRoot := filepath.Join(t.TempDir(), "not-created-yet")
	harness := newHarnessAt(t, "", dataRoot)
	page, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: defaultResourceBase + "/setup"})
	if err != nil {
		t.Fatal(err)
	}
	if _, errStat := os.Stat(dataRoot); !os.IsNotExist(errStat) {
		t.Fatalf("serving the setup page created host state at %q", dataRoot)
	}
	body := string(page.Body)
	for _, forbidden := range []string{dataRoot, "resolved_executable", "operator_pinned"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("setup page discloses deployment state %q", forbidden)
		}
	}
	headers := page.Headers
	if headers.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'" {
		t.Fatalf("content security policy = %q", headers.Get("Content-Security-Policy"))
	}
	if headers.Get("X-Frame-Options") != "DENY" || headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("setup page headers = %v", headers)
	}
}

func TestSetupPageEscapesItsOwnPath(t *testing.T) {
	harness := newHarness(t, "")
	if _, err := harness.adapter.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{
		BasePath:         "/v0/management",
		ResourceBasePath: `/v0/resource/plugins/<img src=x onerror="alert(1)">`,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: `/v0/resource/plugins/<img src=x onerror="alert(1)">/setup`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page.Body), "<img src=x") {
		t.Fatal("the setup page interpolated its own path without escaping it")
	}
	if !strings.Contains(string(page.Body), "&lt;img") {
		t.Fatalf("the setup page did not render its escaped path: %s", page.Body)
	}
}

func TestManagementAndResourceRoutesCarryDistinctHandlers(t *testing.T) {
	harness := newHarness(t, "")
	registration, err := harness.adapter.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{
		BasePath:         "/v0/management",
		ResourceBasePath: defaultResourceBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	resourceHandler := registration.Resources[0].Handler
	for _, route := range registration.Routes {
		if route.Handler == nil {
			t.Fatalf("management route %q has no handler", route.Path)
		}
		if route.Handler == resourceHandler {
			t.Fatalf("management route %q shares the unauthenticated resource handler", route.Path)
		}
	}
	// The authenticated handler must not serve the browser page, and the
	// resource handler must not serve the authenticated routes.
	response, err := resourceHandler.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBasePath + managementStatusPath})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the resource handler served an authenticated route: %d %s", response.StatusCode, response.Body)
	}
	response, err = registration.Routes[0].Handler.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: defaultResourceBase + "/setup"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the management handler served the browser resource: %d", response.StatusCode)
	}
}

func TestLoginCreatedAccountsStayIsolatedUnderConcurrency(t *testing.T) {
	origin := newHarness(t, writeFakeAgent(t))
	first := completeLogin(t, origin.adapter)
	second := completeLogin(t, origin.adapter)
	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for index := 0; index < 8; index++ {
		auth := first
		if index%2 == 1 {
			auth = second
		}
		conversation := fmt.Sprintf("conversation-%s-%d", auth.ID, index)
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := origin.adapter.Execute(context.Background(), pluginapi.ExecutorRequest{
				AuthID:   auth.ID,
				Model:    "cursor/auto",
				Payload:  payload,
				Metadata: map[string]any{"derived_session_id": conversation},
			})
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if origin.factory.profile(first.ID) == origin.factory.profile(second.ID) {
		t.Fatal("both accounts executed under one profile directory")
	}
}

func TestRefreshAuthReprobesTheStoredRecord(t *testing.T) {
	harness := newHarness(t, writeFakeAgent(t))
	auth := completeLogin(t, harness.adapter)
	refreshed, err := harness.adapter.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: auth.ID, StorageJSON: auth.StorageJSON})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Auth.ID != auth.ID || refreshed.Auth.Disabled {
		t.Fatalf("refresh = %#v", refreshed)
	}
	if refreshed.Auth.Metadata["subscription_quota_available"] != false || refreshed.Auth.Metadata["exact_subscription_quota"] != nil {
		t.Fatalf("quota metadata = %#v", refreshed.Auth.Metadata)
	}
	if _, err = harness.adapter.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "cursor-unknown"}); err == nil {
		t.Fatal("refresh accepted an unknown AuthID")
	}
}

func TestManagementRegistrationExposesSetupRoutesAndPage(t *testing.T) {
	harness := newHarness(t, "")
	registration, err := harness.adapter.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{
		BasePath:         "/v0/management",
		ResourceBasePath: "/v0/resource/plugins/cliproxy-cursor-acp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(registration.Routes) != 2 || len(registration.Resources) != 1 {
		t.Fatalf("registration = %#v", registration)
	}
	for _, route := range registration.Routes {
		if route.Method == http.MethodGet && route.Menu != "" {
			t.Fatalf("GET management route %q declares a menu and would be demoted to a resource", route.Path)
		}
		if !strings.HasPrefix(route.Path, "/plugins/cursor-acp/setup") {
			t.Fatalf("management route path = %q", route.Path)
		}
	}
	if registration.Resources[0].Path != "/setup" || registration.Resources[0].Menu == "" {
		t.Fatalf("resource = %#v", registration.Resources[0])
	}

	page, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/cliproxy-cursor-acp/setup"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(page.Body)
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("setup page = %d %v", page.StatusCode, page.Headers)
	}
	for _, required := range []string{"<!doctype html>", "/v0/management/plugins/cursor-acp/setup/status", "/v0/management/plugins/cursor-acp/setup/install", "Authorization", cursor.PinnedAgentVersion(), "agent_package_sha256"} {
		if !strings.Contains(body, required) {
			t.Fatalf("setup page does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"http://", "https://cdn", "<script src", "<link "} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("setup page is not self-contained: found %q", forbidden)
		}
	}
}

func TestSetupStatusReportsInstallStateAndInstallRequiresConfirmation(t *testing.T) {
	harness := newHarness(t, "")
	status, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/cursor-acp/setup/status"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(status.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["agent_installed"] != false {
		t.Fatalf("status = %#v", decoded)
	}
	if decoded["agent_install_source"] != cursor.InstallSourcePinned {
		t.Fatalf("status install source = %#v", decoded["agent_install_source"])
	}
	if decoded["pinned_agent_version"] != cursor.PinnedAgentVersion() {
		t.Fatalf("status pinned version = %#v", decoded["pinned_agent_version"])
	}
	pinnedDigest, _ := cursor.PinnedAgentDigest(runtime.GOOS, runtime.GOARCH)
	if decoded["pinned_agent_sha256"] != pinnedDigest {
		t.Fatalf("status pinned digest = %#v, want %q", decoded["pinned_agent_sha256"], pinnedDigest)
	}
	if _, present := decoded["installed_agent_sha256"]; !present {
		t.Fatalf("status = %#v", decoded)
	}
	root, err := harness.paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	if decoded["data_root"] != root {
		t.Fatalf("status data root = %#v, want %q", decoded["data_root"], root)
	}
	if _, present := decoded["resolved_executable"]; !present {
		t.Fatalf("status = %#v", decoded)
	}

	refused, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/cursor-acp/setup/install", Body: []byte(`{"confirm":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	if refused.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfirmed install = %d %s", refused.StatusCode, refused.Body)
	}

	unknown, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/cursor-acp/setup/unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route = %d", unknown.StatusCode)
	}
}

func TestSetupStatusReportsAManagedInstall(t *testing.T) {
	harness := newHarness(t, "")
	agent := writeFakeAgent(t)
	status, err := harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/cursor-acp/setup/status"})
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err = json.Unmarshal(status.Body, &before); err != nil {
		t.Fatal(err)
	}
	if before["resolved_executable"] != "" {
		t.Fatalf("resolved executable = %#v before any install", before["resolved_executable"])
	}
	t.Setenv("PATH", filepath.Dir(agent))
	status, err = harness.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/cursor-acp/setup/status"})
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err = json.Unmarshal(status.Body, &after); err != nil {
		t.Fatal(err)
	}
	if after["resolved_executable"] != agent {
		t.Fatalf("resolved executable = %#v, want %q", after["resolved_executable"], agent)
	}
}

func TestMetadataFreeRequestsHaveDistinctStatelessSessions(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	_, first, _, err := decodeRequest(pluginapi.ExecutorRequest{AuthID: "cursor-a", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	_, second, _, err := decodeRequest(pluginapi.ExecutorRequest{AuthID: "cursor-a", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("metadata-free requests reused session identity %q", first)
	}
}

func TestDecodeRequestSessionPriorityAndTranscript(t *testing.T) {
	cases := []struct {
		name      string
		metadata  map[string]any
		payload   string
		wantID    string
		stateless bool
		wantText  string
	}{
		{"derived", map[string]any{"derived_session_id": "derived"}, `{"messages":[{"role":"system","content":"rules"},{"role":"developer","content":"guard"},{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"prior"}]}`, "derived", false, "[system] rules\n[developer] guard\n[user] hello\n[assistant] prior\n"},
		{"execution", map[string]any{"execution_session_id": "execution"}, `{"messages":[{"role":"user","content":"hello"}]}`, "execution", false, "[user] hello\n"},
		{"payload", nil, `{"session_id":"payload","messages":[{"role":"user","content":"hello"}]}`, "payload", false, "[user] hello\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			prompt, id, stateless, err := decodeRequest(pluginapi.ExecutorRequest{Metadata: test.metadata, Payload: []byte(test.payload)})
			if err != nil {
				t.Fatal(err)
			}
			if id != test.wantID || stateless != test.stateless || prompt != test.wantText {
				t.Fatalf("got %q %q %v", prompt, id, stateless)
			}
		})
	}
}

func TestExecutorRejectsUnsupportedSurfaces(t *testing.T) {
	adapter := newHarness(t, "").adapter
	if _, err := adapter.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{}); err == nil {
		t.Fatal("streaming succeeded")
	}
	if _, err := adapter.CountTokens(context.Background(), pluginapi.ExecutorRequest{}); err == nil {
		t.Fatal("token counting succeeded")
	}
	if _, err := adapter.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{}); err == nil {
		t.Fatal("raw HTTP succeeded")
	}
}

func mustProfileDir(t *testing.T, auth pluginapi.AuthData) string {
	t.Helper()
	var stored struct {
		ProfileDir string `json:"profile_dir"`
	}
	if err := json.Unmarshal(auth.StorageJSON, &stored); err != nil {
		t.Fatal(err)
	}
	return stored.ProfileDir
}
