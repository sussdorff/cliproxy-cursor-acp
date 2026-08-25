package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestABIBounds(t *testing.T) {
	if err := validateABIInput("x", maxABIRequestBytes+1); err == nil {
		t.Fatal("oversized request accepted")
	}
	if err := validateABIInput(string(make([]byte, maxABIMethodBytes+1)), 0); err == nil {
		t.Fatal("oversized method accepted")
	}
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

func TestConfigureRegistersRequiredCLIProxyCapabilities(t *testing.T) {
	defer shutdown()
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	workspace := filepath.Join(root, "workspace")
	for _, path := range []string{profile, workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := []byte("executable: agent\nmax_concurrent: 1\nmax_prompt_bytes: 100\nmax_output_bytes: 100\ntimeout: 1s\nworkspace_root: " + workspace + "\naccounts:\n  - auth_id: cursor-a\n    label: A\n    profile_dir: " + profile + "\n    model: cursor/auto\n")
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: config})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := configure(raw)
	if err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion || !registration.Capabilities.AuthProvider || !registration.Capabilities.ModelProvider || !registration.Capabilities.Executor || !registration.Capabilities.UsagePlugin {
		t.Fatalf("registration = %#v", registration)
	}
}
