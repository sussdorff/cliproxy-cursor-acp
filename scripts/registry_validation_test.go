// Package scripts holds repository-level checks that CI runs with the rest of
// the test suite.
package scripts

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sussdorff/cliproxy-cursor-acp/internal/plugin"
)

// pluginIDPattern and the checks below mirror CLIProxyAPI's own plugin store
// contract in internal/pluginstore/registry.go.
var (
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
)

type registry struct {
	SchemaVersion int             `json:"schema_version"`
	Plugins       []registryEntry `json:"plugins"`
}

type registryEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Repository  string   `json:"repository"`
	Version     string   `json:"version"`
	License     string   `json:"license"`
	Homepage    string   `json:"homepage"`
	Tags        []string `json:"tags"`
}

func loadRegistry(t *testing.T) registry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "plugin-store", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var decoded registry
	if err = decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPluginStoreRegistrySatisfiesTheCLIProxyAPIContract(t *testing.T) {
	decoded := loadRegistry(t)
	if decoded.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", decoded.SchemaVersion)
	}
	if len(decoded.Plugins) != 1 {
		t.Fatalf("plugins = %d, want exactly this repository's plugin", len(decoded.Plugins))
	}
	entry := decoded.Plugins[0]
	for field, value := range map[string]string{"id": entry.ID, "name": entry.Name, "description": entry.Description, "author": entry.Author, "repository": entry.Repository, "license": entry.License} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("registry entry is missing required field %s", field)
		}
	}
	if !pluginIDPattern.MatchString(entry.ID) {
		t.Fatalf("plugin id %q does not match the store id pattern", entry.ID)
	}
	if entry.ID != "cliproxy-cursor-acp" {
		t.Fatalf("plugin id = %q; the release archive name depends on it", entry.ID)
	}
	if strings.HasPrefix(entry.Version, "v") || strings.HasPrefix(entry.Version, "V") {
		t.Fatalf("plugin version %q must not carry a leading v", entry.Version)
	}
	if !pluginVersionPattern.MatchString(entry.Version) {
		t.Fatalf("plugin version %q does not match the store version pattern", entry.Version)
	}
	if entry.Version != plugin.Version {
		t.Fatalf("registry version %q does not match the plugin Version constant %q", entry.Version, plugin.Version)
	}
	if len(entry.Tags) == 0 {
		t.Fatal("registry entry declares no tags")
	}
}

func TestPluginStoreRegistryRepositoryIsAnExactGitHubURL(t *testing.T) {
	entry := loadRegistry(t).Plugins[0]
	parsed, err := url.Parse(entry.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("repository %q must be https://github.com/{owner}/{repo}", entry.Repository)
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" || strings.HasSuffix(segments[1], ".git") {
		t.Fatalf("repository %q must be https://github.com/{owner}/{repo}", entry.Repository)
	}
}

func TestReleaseWorkflowPublishesTheExpectedAssets(t *testing.T) {
	entry := loadRegistry(t).Plugins[0]
	workflow := readWorkflow(t, "release.yml")
	for _, required := range []string{
		entry.ID + "_${VERSION}_linux_amd64.zip",
		entry.ID + ".so",
		"checksums.txt",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("release workflow does not produce %q", required)
		}
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// actionReferencePattern matches every `uses:` value in a workflow.
var actionReferencePattern = regexp.MustCompile(`uses:\s*(\S+)`)

// commitSHAPattern is a full 40 character git object id.
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestWorkflowsPinEveryActionByCommitSHA(t *testing.T) {
	for _, name := range []string{"ci.yml", "release.yml"} {
		t.Run(name, func(t *testing.T) {
			workflow := readWorkflow(t, name)
			matches := actionReferencePattern.FindAllStringSubmatch(workflow, -1)
			if len(matches) == 0 {
				t.Fatal("workflow declares no actions")
			}
			for _, match := range matches {
				reference := match[1]
				_, version, found := strings.Cut(reference, "@")
				if !found {
					t.Fatalf("action %q is not pinned", reference)
				}
				if !commitSHAPattern.MatchString(version) {
					t.Fatalf("action %q is not pinned by a full commit SHA", reference)
				}
			}
		})
	}
}

func TestReleaseWorkflowPinsTheBuilderImageAndAttestsTheArchive(t *testing.T) {
	workflow := readWorkflow(t, "release.yml")
	digest := regexp.MustCompile(`golang@sha256:[0-9a-f]{64}`)
	if !digest.MatchString(workflow) {
		t.Fatal("the release workflow does not pin the golang builder image by digest")
	}
	if strings.Contains(workflow, `"golang:${GO_VERSION}"`) {
		t.Fatal("the release workflow still runs a mutable golang tag")
	}
	if !strings.Contains(workflow, "attest-build-provenance") {
		t.Fatal("the release workflow does not attest build provenance for the archive")
	}
	for _, permission := range []string{"attestations: write", "id-token: write", "contents: write"} {
		if !strings.Contains(workflow, permission) {
			t.Fatalf("the release workflow does not grant %q", permission)
		}
	}
}

func TestReleaseWorkflowVerifiesTheEmbeddedCursorDigests(t *testing.T) {
	workflow := readWorkflow(t, "release.yml")
	if !strings.Contains(workflow, "CCA_VERIFY_UPSTREAM_DIGESTS") {
		t.Fatal("the release workflow does not verify the embedded Cursor Agent digests against upstream")
	}
	if !strings.Contains(workflow, "TestPinnedAgentDigestsMatchUpstream") {
		t.Fatal("the release workflow does not run the upstream digest verification test")
	}
}
