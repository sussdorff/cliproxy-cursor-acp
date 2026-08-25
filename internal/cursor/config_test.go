package cursor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeConfigRejectsInsecureAndAliasedProfiles(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	profile := filepath.Join(root, "profile")
	for _, path := range []string{workspace, profile} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	config := Config{Executable: os.Args[0], WorkspaceRoot: workspace, MaxConcurrent: 1, MaxPromptBytes: 10, MaxOutputBytes: 10, Timeout: time.Second, Accounts: []Account{{AuthID: "a", ProfileDir: profile}, {AuthID: "b", ProfileDir: profile}}}
	if _, err := NormalizeConfig(config); err == nil {
		t.Fatal("insecure profile accepted")
	}
	if err := os.Chmod(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeConfig(config); err == nil {
		t.Fatal("duplicate canonical profile accepted")
	}
}
