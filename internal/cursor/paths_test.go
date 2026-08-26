package cursor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathsPreferConfiguredDataRootOverHostAuthDir(t *testing.T) {
	root := t.TempDir()
	configured := filepath.Join(root, "configured")
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := NewPaths(configured, "")
	paths.ObserveHost(authDir)
	resolved, err := paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(resolved) != "configured" {
		t.Fatalf("root = %q, want the configured data root", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("data root mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestPathsDeriveDataRootFromHostAuthDir(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := NewPaths("", "")
	paths.ObserveHost("")
	paths.ObserveHost(authDir)
	paths.ObserveHost(filepath.Join(authDir, "later"))
	resolved, err := paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(canonical, "cliproxy-cursor-acp") {
		t.Fatalf("root = %q, want it derived from the first observed AuthDir", resolved)
	}
}

func TestPathsReportUnresolvedDataRootWithConfigKey(t *testing.T) {
	paths := NewPaths("", "")
	_, err := paths.Root()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "data_root_unresolved" {
		t.Fatalf("root error = %#v", err)
	}
	if !strings.Contains(err.Error(), "data_root") {
		t.Fatalf("error %q must name the data_root configuration key", err)
	}
}

func TestPathsDeriveWorkspaceAndProfilesFromDataRoot(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "data")
	paths := NewPaths(configured, "")
	workspace, err := paths.Workspace()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	root, err := paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	if workspace != filepath.Join(root, "workspace") || profiles != filepath.Join(root, "profiles") {
		t.Fatalf("workspace = %q profiles = %q", workspace, profiles)
	}
	for _, path := range []string{workspace, profiles} {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatal(errStat)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want 0700", path, info.Mode().Perm())
		}
	}
}

func TestPathsHonourConfiguredWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := NewPaths(filepath.Join(root, "data"), workspace)
	resolved, err := paths.Workspace()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != canonical {
		t.Fatalf("workspace = %q, want the configured root %q", resolved, canonical)
	}
}
