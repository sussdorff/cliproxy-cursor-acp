package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	cliRef = "ghcr.io/sussdorff/cli-proxy-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cpRef  = "ghcr.io/sussdorff/cpa-manager-plus@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func verifier(t *testing.T, mode, fixtureDir string, overrides ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "verify-production-images.sh", mode, "--compose-file", "../deploy/production-images.override.yml", "--fixture-dir", fixtureDir)
	cmd.Env = append(os.Environ(),
		"CLI_PROXY_IMAGE="+cliRef,
		"CLI_PROXY_REVISION=1111111111111111111111111111111111111111",
		"CPAMP_IMAGE="+cpRef,
		"CPAMP_REVISION=2222222222222222222222222222222222222222",
	)
	cmd.Env = append(cmd.Env, overrides...)
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

func TestProductionImageVerifierAcceptsBothApprovedForkImages(t *testing.T) {
	for _, mode := range []string{"pre-deploy", "post-deploy"} {
		output, err := verifier(t, mode, "testdata/provenance")
		if err != nil {
			t.Fatalf("%s verifier failed: %v\n%s", mode, err, output)
		}
		for _, expected := range []string{"cli-proxy-api: " + mode + " verified", "cpa-manager-plus: " + mode + " verified"} {
			if !strings.Contains(output, expected) {
				t.Errorf("output is missing %q: %s", expected, output)
			}
		}
	}
}

func TestProductionImageVerifierRejectsMutableOrForeignReferences(t *testing.T) {
	for name, override := range map[string]string{
		"mutable tag":        "CLI_PROXY_IMAGE=ghcr.io/sussdorff/cli-proxy-api:main",
		"upstream namespace": "CLI_PROXY_IMAGE=ghcr.io/router-for-me/cli-proxy-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		t.Run(name, func(t *testing.T) {
			output, err := verifier(t, "pre-deploy", "testdata/provenance", override)
			if err == nil || !strings.Contains(output, "approved GHCR image pinned by digest") {
				t.Fatalf("expected fail-closed reference rejection, got err=%v output=%s", err, output)
			}
		})
	}
}

func TestProductionImageVerifierRejectsRenderedComposeDrift(t *testing.T) {
	fixtures := t.TempDir()
	entries, err := os.ReadDir("testdata/provenance")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, readErr := os.ReadFile(filepath.Join("testdata/provenance", entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if entry.Name() == "compose.json" {
			raw = []byte(`{"services":{"cli-proxy-api":{"image":"` + cpRef + `"},"cpa-manager-plus":{"image":"` + cliRef + `"}}}`)
		}
		if writeErr := os.WriteFile(filepath.Join(fixtures, entry.Name()), raw, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	output, err := verifier(t, "pre-deploy", fixtures)
	if err == nil || !strings.Contains(output, "service cli-proxy-api has unapproved image") {
		t.Fatalf("expected Compose drift rejection, got err=%v output=%s", err, output)
	}
}

func TestProductionImageVerifierRejectsRevisionAndRunningDigestMismatch(t *testing.T) {
	output, err := verifier(t, "pre-deploy", "testdata/provenance", "CLI_PROXY_REVISION=short")
	if err == nil || !strings.Contains(output, "must be a full 40-character") {
		t.Fatalf("expected incomplete revision rejection, got err=%v output=%s", err, output)
	}

	output, err = verifier(t, "pre-deploy", "testdata/provenance", "CPAMP_REVISION=3333333333333333333333333333333333333333")
	if err == nil || !strings.Contains(output, "OCI revision does not match") {
		t.Fatalf("expected revision rejection, got err=%v output=%s", err, output)
	}

	fixtures := t.TempDir()
	entries, err := os.ReadDir("testdata/provenance")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, readErr := os.ReadFile(filepath.Join("testdata/provenance", entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if entry.Name() == "cli-proxy-api.repo-digests" {
			raw = []byte("ghcr.io/sussdorff/cli-proxy-api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n")
		}
		if writeErr := os.WriteFile(filepath.Join(fixtures, entry.Name()), raw, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	output, err = verifier(t, "post-deploy", fixtures)
	if err == nil || !strings.Contains(output, "image digest does not match") {
		t.Fatalf("expected running digest rejection, got err=%v output=%s", err, output)
	}

	if err = os.WriteFile(filepath.Join(fixtures, "cli-proxy-api.repo-digests"), []byte(cliRef+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongSource := "https://github.com/router-for-me/CLIProxyAPI|1111111111111111111111111111111111111111|1111111111111111111111111111111111111111|2026-08-31T06:00:00Z\n"
	if err = os.WriteFile(filepath.Join(fixtures, "cli-proxy-api.labels"), []byte(wrongSource), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = verifier(t, "pre-deploy", fixtures)
	if err == nil || !strings.Contains(output, "wrong OCI source") {
		t.Fatalf("expected source rejection, got err=%v output=%s", err, output)
	}
}
