package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigAcceptsEmptyOperatorConfiguration(t *testing.T) {
	config, err := NormalizeConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxConcurrent < 1 || config.MaxPromptBytes < 1 || config.MaxOutputBytes < 1 || config.Timeout <= 0 {
		t.Fatalf("defaults = %#v", config)
	}
	if config.Executable != "" || config.WorkspaceRoot != "" || config.DataRoot != "" {
		t.Fatalf("optional paths were invented: %#v", config)
	}
}

func TestNormalizeConfigRejectsUntrustedExecutable(t *testing.T) {
	if _, err := NormalizeConfig(Config{Executable: "agent"}); err == nil {
		t.Fatal("relative executable accepted")
	}
	if _, err := NormalizeConfig(Config{Executable: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing executable accepted")
	}
	config, err := NormalizeConfig(Config{Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	if config.Executable != os.Args[0] {
		t.Fatalf("executable = %q", config.Executable)
	}
}

func TestNormalizeConfigRejectsInsecureWorkspaceRoot(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeConfig(Config{WorkspaceRoot: workspace, Timeout: time.Second}); err == nil {
		t.Fatal("group-readable workspace accepted")
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeConfig(Config{WorkspaceRoot: workspace, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeConfigRejectsRelativeDataRoot(t *testing.T) {
	if _, err := NormalizeConfig(Config{DataRoot: "relative/data"}); err == nil {
		t.Fatal("relative data root accepted")
	}
}

func TestNormalizeConfigDefaultsToThePinnedAgentInstallSource(t *testing.T) {
	config, err := NormalizeConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentInstallSource != InstallSourcePinned {
		t.Fatalf("install source = %q, want the pinned artifact", config.AgentInstallSource)
	}
	config, err = NormalizeConfig(Config{AgentInstallSource: "LATEST", AgentPackageSHA256: strings.Repeat("A", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentInstallSource != InstallSourceLatest || config.AgentPackageSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("normalized config = %#v", config)
	}
	if _, err = NormalizeConfig(Config{AgentInstallSource: "nightly"}); err == nil {
		t.Fatal("unknown install source accepted")
	}
	if _, err = NormalizeConfig(Config{AgentPackageSHA256: "not-a-digest"}); err == nil {
		t.Fatal("malformed sha256 pin accepted")
	}
}
