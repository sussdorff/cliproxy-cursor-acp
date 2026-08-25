package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestConfigureRegistersRequiredCLIProxyCapabilities(t *testing.T) {
	defer shutdown()
	config := []byte("executable: agent\nmax_concurrent: 1\nmax_prompt_bytes: 100\naccounts:\n  - auth_id: cursor-a\n    label: A\n    profile_dir: /private/a\n    model: cursor/auto\n")
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
