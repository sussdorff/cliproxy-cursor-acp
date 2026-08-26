package cursor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testLatestVersion = "2099.01.02-abcdef1"

func installerScript(version string) string {
	return "#!/bin/sh\n# Cursor installer\nDOWNLOAD_URL=\"https://downloads.cursor.com/lab/" + version +
		"/${OS}/${ARCH}/agent-cli-package.tar.gz\"\ncurl -fsSL \"$DOWNLOAD_URL\" | tar -xz\n"
}

type archiveEntry struct {
	name     string
	body     string
	mode     int64
	typeFlag byte
	linkName string
	size     int64
}

func buildArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipper := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(zipper)
	for _, entry := range entries {
		flag := entry.typeFlag
		if flag == 0 {
			flag = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(entry.body))
		if entry.size > 0 {
			size = entry.size
		}
		if flag != tar.TypeReg {
			size = 0
		}
		header := &tar.Header{Name: entry.name, Mode: mode, Size: size, Typeflag: flag, Linkname: entry.linkName}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			written, err := writer.Write([]byte(entry.body))
			if err != nil {
				t.Fatal(err)
			}
			for int64(written) < size {
				chunk := int64(len(entry.body))
				if chunk == 0 {
					chunk = 1
				}
				extra, errWrite := writer.Write(bytes.Repeat([]byte("x"), int(min64(chunk, size-int64(written)))))
				if errWrite != nil {
					t.Fatal(errWrite)
				}
				written += extra
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func agentEntry(version string) archiveEntry {
	return archiveEntry{name: "agent-cli-package/agent", body: "#!/bin/sh\necho \"cursor-agent " + version + "\"\n", mode: 0o755}
}

func validAgentArchive(t *testing.T, version string) []byte {
	t.Helper()
	return buildArchive(t, []archiveEntry{
		{name: "agent-cli-package/", typeFlag: tar.TypeDir, mode: 0o755},
		{name: "agent-cli-package/README", body: "official package\n"},
		agentEntry(version),
	})
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// cursorTransport answers Cursor's own HTTPS URLs from memory. Tests therefore
// exercise the production URLs, the HTTPS requirement, and the host allowlist
// instead of relaxing any of them.
type cursorTransport struct {
	script          string
	archive         []byte
	artifactStatus  int
	artifactLocatio string
	calls           atomic.Int64
}

func (t *cursorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	switch {
	case request.URL.Host == "cursor.com" && request.URL.Path == "/install":
		return canned(request, http.StatusOK, []byte(t.script), "")
	case strings.HasSuffix(request.URL.Path, "/agent-cli-package.tar.gz"):
		if t.artifactStatus != 0 {
			return canned(request, t.artifactStatus, nil, t.artifactLocatio)
		}
		return canned(request, http.StatusOK, t.archive, "")
	default:
		return canned(request, http.StatusNotFound, []byte("not found"), "")
	}
}

func canned(request *http.Request, status int, body []byte, location string) (*http.Response, error) {
	header := http.Header{}
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

func testInstaller(t *testing.T, transport *cursorTransport) *Installer {
	t.Helper()
	return &Installer{
		Paths:  NewPaths(t.TempDir(), ""),
		GOOS:   "linux",
		GOARCH: "amd64",
		Client: &http.Client{Transport: transport},
	}
}

func TestPinnedArtifactIsDeclaredForEverySupportedPlatform(t *testing.T) {
	if PinnedAgentVersion() == "" {
		t.Fatal("no pinned Cursor Agent version is embedded")
	}
	for _, platform := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		digest, ok := PinnedAgentDigest(platform[0], platform[1])
		if !ok || len(digest) != 64 {
			t.Fatalf("no embedded sha256 for %s/%s", platform[0], platform[1])
		}
		if _, err := hex.DecodeString(digest); err != nil {
			t.Fatalf("embedded sha256 for %s/%s is not hex", platform[0], platform[1])
		}
	}
	if _, ok := PinnedAgentDigest("plan9", "mips"); ok {
		t.Fatal("an unsupported platform reports an embedded sha256")
	}
}

func TestInstallerInstallsThePinnedArtifact(t *testing.T) {
	archive := validAgentArchive(t, PinnedAgentVersion())
	transport := &cursorTransport{archive: archive}
	installer := testInstaller(t, transport)
	// The operator pin stands in for the embedded digest of the real artifact,
	// which this hermetic archive cannot reproduce.
	installer.ExpectedSHA256 = digestOf(archive)
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != PinnedAgentVersion() || result.Source != InstallSourcePinned {
		t.Fatalf("install result = %#v", result)
	}
	if result.SHA256 != digestOf(archive) || result.Bytes != int64(len(archive)) {
		t.Fatalf("install result = %#v", result)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("pinned install made %d requests; it must not fetch the installer script", transport.calls.Load())
	}
	status := installer.Status()
	if !status.Installed || status.Version != PinnedAgentVersion() || status.SHA256 != digestOf(archive) || status.Source != InstallSourcePinned {
		t.Fatalf("status = %#v", status)
	}
	t.Setenv("PATH", t.TempDir())
	executable, err := ResolveExecutable("", installer.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if executable != status.BinaryPath {
		t.Fatalf("resolved %q, managed install reports %q", executable, status.BinaryPath)
	}
	root, err := installer.Paths.AgentRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(executable, filepath.Join(root, "current")+string(filepath.Separator)) {
		t.Fatalf("executable %q is not resolved through the current pointer", executable)
	}
	if _, errRepeat := installer.Install(context.Background()); errRepeat != nil {
		t.Fatalf("re-installing the active version failed: %v", errRepeat)
	}
}

func TestInstallerRefusesAnArtifactThatDoesNotMatchTheEmbeddedPin(t *testing.T) {
	installer := testInstaller(t, &cursorTransport{archive: validAgentArchive(t, PinnedAgentVersion())})
	if _, err := installer.Install(context.Background()); FailureCode(err) != "agent_package_checksum_mismatch" {
		t.Fatalf("unpinned artifact error = %#v", err)
	}
	if installer.Status().Installed {
		t.Fatal("an unverified artifact was activated")
	}
}

func TestInstallerVerifiesTheDigestBeforeExtractingOrExecuting(t *testing.T) {
	hostile := buildArchive(t, []archiveEntry{agentEntry(PinnedAgentVersion()), {name: "../escaped", body: "x"}})
	installer := testInstaller(t, &cursorTransport{archive: hostile})
	// No operator pin: the embedded digest cannot match, so the archive must be
	// rejected before any entry is written and before --version is executed.
	if _, err := installer.Install(context.Background()); FailureCode(err) != "agent_package_checksum_mismatch" {
		t.Fatalf("hostile archive reached extraction: %#v", err)
	}
	root, err := installer.Paths.AgentRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unverified artifact left %d entries under the agent root", len(entries))
	}
}

func TestInstallerLatestSourceRequiresAnOperatorPin(t *testing.T) {
	transport := &cursorTransport{script: installerScript(testLatestVersion), archive: validAgentArchive(t, testLatestVersion)}
	installer := testInstaller(t, transport)
	installer.Source = InstallSourceLatest
	_, err := installer.Install(context.Background())
	if FailureCode(err) != "agent_package_pin_required" {
		t.Fatalf("unpinned latest install error = %#v", err)
	}
	if !strings.Contains(err.Error(), "agent_package_sha256") {
		t.Fatalf("error %q must name the agent_package_sha256 configuration key", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("unpinned latest install made %d requests", transport.calls.Load())
	}
}

func TestInstallerLatestSourceUsesTheParsedVersionWithAnOperatorPin(t *testing.T) {
	archive := validAgentArchive(t, testLatestVersion)
	transport := &cursorTransport{script: installerScript(testLatestVersion), archive: archive}
	installer := testInstaller(t, transport)
	installer.Source = InstallSourceLatest
	installer.ExpectedSHA256 = digestOf(archive)
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != testLatestVersion || result.Source != InstallSourceLatest {
		t.Fatalf("install result = %#v", result)
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("latest install made %d requests, want the script and the artifact", transport.calls.Load())
	}
}

func TestInstallerRefusesPlaintextAndForeignRedirects(t *testing.T) {
	archive := validAgentArchive(t, PinnedAgentVersion())
	cases := map[string]string{
		"cleartext redirect":    "http://downloads.cursor.com/lab/x/linux/x64/agent-cli-package.tar.gz",
		"foreign host redirect": "https://downloads.evil.example/lab/x/linux/x64/agent-cli-package.tar.gz",
	}
	for name, location := range cases {
		t.Run(name, func(t *testing.T) {
			transport := &cursorTransport{archive: archive, artifactStatus: http.StatusFound, artifactLocatio: location}
			installer := testInstaller(t, transport)
			installer.ExpectedSHA256 = digestOf(archive)
			if _, err := installer.Install(context.Background()); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
			if installer.Status().Installed {
				t.Fatal("unsafe redirect produced an install")
			}
		})
	}
}

func TestParseInstallerScriptAcceptsOnlyCanonicalPackageURLs(t *testing.T) {
	pkg, err := ParseInstallerScript([]byte(installerScript(testLatestVersion)), "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Version != testLatestVersion {
		t.Fatalf("version = %q", pkg.Version)
	}
	if pkg.URL != "https://downloads.cursor.com/lab/"+testLatestVersion+"/linux/x64/agent-cli-package.tar.gz" {
		t.Fatalf("package URL = %q", pkg.URL)
	}
	cases := map[string]string{
		"foreign host":  "#!/bin/sh\nURL=\"https://downloads.evil.example/lab/" + testLatestVersion + "/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n",
		"two URLs":      installerScript(testLatestVersion) + installerScript("2026.01.01-aaaaaaa"),
		"no URL":        "#!/bin/sh\necho nothing\n",
		"not canonical": "#!/bin/sh\nURL=\"https://downloads.cursor.com/lab/latest/linux/x64/agent-cli-package.tar.gz\"\n",
		"cleartext":     "#!/bin/sh\nURL=\"http://downloads.cursor.com/lab/" + testLatestVersion + "/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			if _, errParse := ParseInstallerScript([]byte(script), "linux", "amd64"); errParse == nil {
				t.Fatal("hostile installer script accepted")
			}
		})
	}
	if _, errPlatform := ParseInstallerScript([]byte(installerScript(testLatestVersion)), "plan9", "mips"); errPlatform == nil {
		t.Fatal("unsupported platform accepted")
	}
}

func TestInstallerRefusesAnUnsupportedPlatform(t *testing.T) {
	installer := testInstaller(t, &cursorTransport{})
	installer.GOOS = "plan9"
	installer.GOARCH = "mips"
	if _, err := installer.Install(context.Background()); FailureCode(err) != "agent_platform_unsupported" {
		t.Fatalf("unsupported platform error = %#v", err)
	}
}

func TestResolveExecutablePrefersExplicitConfiguration(t *testing.T) {
	paths := NewPaths(t.TempDir(), "")
	if _, err := ResolveExecutable("relative/agent", paths); err == nil {
		t.Fatal("relative executable accepted")
	}
	resolved, err := ResolveExecutable(os.Args[0], paths)
	if err != nil || resolved != os.Args[0] {
		t.Fatalf("resolved = %q, %v", resolved, err)
	}
}

// writeStubAgent drops an executable named "agent" into its own directory and
// returns that directory, so it can be placed on PATH.
func writeStubAgent(t *testing.T, body string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, AgentExecutableName), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestResolveExecutablePrefersTheVerifiedManagedInstallOverPATH(t *testing.T) {
	archive := validAgentArchive(t, PinnedAgentVersion())
	installer := testInstaller(t, &cursorTransport{archive: archive})
	installer.ExpectedSHA256 = digestOf(archive)
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An unverified Cursor CLI on PATH must not outrank the digest-verified
	// managed install.
	onPath := writeStubAgent(t, "#!/bin/sh\necho other\n")
	t.Setenv("PATH", onPath)
	resolved, err := ResolveExecutable("", installer.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != result.BinaryPath {
		t.Fatalf("resolved %q, want the managed install %q", resolved, result.BinaryPath)
	}
	explicit := filepath.Join(onPath, AgentExecutableName)
	if resolved, err = ResolveExecutable(explicit, installer.Paths); err != nil || resolved != explicit {
		t.Fatalf("explicit configuration = %q, %v", resolved, err)
	}
}

func TestResolveExecutableFallsBackToPATHWithoutAManagedInstall(t *testing.T) {
	paths := NewPaths(t.TempDir(), "")
	onPath := writeStubAgent(t, "#!/bin/sh\necho other\n")
	t.Setenv("PATH", onPath)
	resolved, err := ResolveExecutable("", paths)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(onPath, AgentExecutableName) {
		t.Fatalf("resolved = %q, want the PATH candidate", resolved)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err = ResolveExecutable("", paths); FailureCode(err) != CodeSetupRequired {
		t.Fatalf("missing CLI error = %#v", err)
	}
}

func TestInstallerVerifiesTheVersionWithAnIsolatedProcess(t *testing.T) {
	// The extracted binary refuses to report a version when it can see an
	// environment variable that only the plugin process has.
	t.Setenv("CCA_PARENT_ONLY", "leaked")
	body := "#!/bin/sh\ntest -z \"$CCA_PARENT_ONLY\" || exit 9\ntest \"$PWD\" = \"$CURSOR_CONFIG_DIR\" || exit 10\necho \"cursor-agent " + PinnedAgentVersion() + "\"\n"
	archive := buildArchive(t, []archiveEntry{{name: "agent-cli-package/agent", body: body, mode: 0o755}})
	installer := testInstaller(t, &cursorTransport{archive: archive})
	installer.ExpectedSHA256 = digestOf(archive)
	if _, err := installer.Install(context.Background()); err != nil {
		t.Fatalf("version verification did not isolate the child process: %v", err)
	}
}

func TestInstallerRefusesOversizedPackage(t *testing.T) {
	archive := validAgentArchive(t, PinnedAgentVersion())
	installer := testInstaller(t, &cursorTransport{archive: archive})
	installer.ExpectedSHA256 = digestOf(archive)
	installer.MaxPackageBytes = 32
	if _, err := installer.Install(context.Background()); FailureCode(err) != "agent_download_too_large" {
		t.Fatalf("oversized package error = %#v", err)
	}
}

func TestInstallerRefusesHostileArchives(t *testing.T) {
	safeBinary := agentEntry(PinnedAgentVersion())
	cases := map[string][]archiveEntry{
		"path traversal":   {safeBinary, {name: "../escaped", body: "x"}},
		"absolute path":    {safeBinary, {name: "/etc/passwd", body: "x"}},
		"escaping symlink": {safeBinary, {name: "agent-cli-package/link", typeFlag: tar.TypeSymlink, linkName: "../../../../etc/passwd"}},
		"absolute symlink": {safeBinary, {name: "agent-cli-package/link", typeFlag: tar.TypeSymlink, linkName: "/etc/passwd"}},
		"hard link":        {safeBinary, {name: "agent-cli-package/hard", typeFlag: tar.TypeLink, linkName: "agent-cli-package/agent"}},
		"device node":      {safeBinary, {name: "agent-cli-package/dev", typeFlag: tar.TypeChar}},
		"duplicate entry":  {safeBinary, safeBinary},
		"no agent binary":  {{name: "agent-cli-package/README", body: "nothing here"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			archive := buildArchive(t, entries)
			installer := testInstaller(t, &cursorTransport{archive: archive})
			installer.ExpectedSHA256 = digestOf(archive)
			if _, err := installer.Install(context.Background()); err == nil {
				t.Fatal("hostile archive accepted")
			}
			if installer.Status().Installed {
				t.Fatal("hostile archive was activated")
			}
		})
	}
}

func TestExtractAgentArchiveRejectsAPreexistingSymlinkParent(t *testing.T) {
	destination := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(destination, "agent-cli-package")); err != nil {
		t.Fatal(err)
	}
	archive := validAgentArchive(t, PinnedAgentVersion())
	_, err := extractAgentArchive(bytes.NewReader(archive), destination, 16, 1<<20)
	if FailureCode(err) != "agent_archive_unsafe" {
		t.Fatalf("preexisting symlink parent error = %#v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, AgentExecutableName)); !os.IsNotExist(err) {
		t.Fatalf("archive entry escaped through the symlink parent: %v", err)
	}
}

func TestInstallerBoundsArchiveEntriesAndExpandedSize(t *testing.T) {
	entries := []archiveEntry{agentEntry(PinnedAgentVersion())}
	for index := 0; index < 20; index++ {
		entries = append(entries, archiveEntry{name: fmt.Sprintf("agent-cli-package/file-%d", index), body: "padding"})
	}
	archive := buildArchive(t, entries)
	installer := testInstaller(t, &cursorTransport{archive: archive})
	installer.ExpectedSHA256 = digestOf(archive)
	installer.MaxArchiveEntries = 5
	if _, err := installer.Install(context.Background()); err == nil {
		t.Fatal("archive with too many entries accepted")
	}

	bomb := buildArchive(t, []archiveEntry{
		agentEntry(PinnedAgentVersion()),
		{name: "agent-cli-package/bomb", body: "x", size: 1 << 20},
	})
	installer = testInstaller(t, &cursorTransport{archive: bomb})
	installer.ExpectedSHA256 = digestOf(bomb)
	installer.MaxExpandedBytes = 4096
	if _, err := installer.Install(context.Background()); err == nil {
		t.Fatal("archive exceeding the expanded size limit accepted")
	}
}

func TestInstallerRefusesVersionMismatch(t *testing.T) {
	archive := validAgentArchive(t, "1999.01.01-deadbee")
	installer := testInstaller(t, &cursorTransport{archive: archive})
	installer.ExpectedSHA256 = digestOf(archive)
	if _, err := installer.Install(context.Background()); FailureCode(err) != "agent_version_mismatch" {
		t.Fatalf("version mismatch error = %#v", err)
	}
}
