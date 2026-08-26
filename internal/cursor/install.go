package cursor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OfficialInstallerURL is Cursor's own installer script. It is parsed, never
// executed, and only the canonical artifact URL it names is downloaded.
const OfficialInstallerURL = "https://cursor.com/install"

// AgentExecutableName is the official Cursor Agent CLI command name.
const AgentExecutableName = "agent"

const managedManifestName = ".install.json"

// BUMP-PER-RELEASE. pinnedAgentVersion and pinnedAgentDigests are the official
// Cursor Agent CLI release this plugin installs by default, together with the
// sha256 of each platform's agent-cli-package.tar.gz. Cursor publishes no
// checksum file, so this embedded pin is the only thing standing between a
// compromised or swapped CDN artifact and an executed binary. Never extract or
// execute an artifact whose digest was not verified against this pin or an
// explicit operator pin.
//
// To bump: for every platform below run
//
//	curl -sL https://downloads.cursor.com/lab/<version>/<os>/<arch>/agent-cli-package.tar.gz | shasum -a 256
//
// twice, from different networks if possible, and update both constants and
// plugin-store/registry.json in the same commit.
const pinnedAgentVersion = "2026.08.11-e8db854"

var pinnedAgentDigests = map[string]string{
	"linux/x64":    "bfff4bf6f4e9dd30c1d0ef0a70b6077b074015dd2948e4c50685d53afdcfce5a",
	"linux/arm64":  "ea13f92e295f523a99ce8d8f57d6894d21e5d1e2d030ffad718ccd5955ca2eed",
	"darwin/x64":   "d5c1ce96dd36469e0231d818d4ccf390caac52d94e607c56ebeecc247cab2b1b",
	"darwin/arm64": "46044d6d7bcbd7b49a0cf1cd01aa4ca79aaa2ea5f2c7a32965fc0ebe29841790",
}

// InstallSource selects which artifact the managed installer trusts.
type InstallSource = string

const (
	// InstallSourcePinned installs the release-pinned version verified against
	// the digest embedded in this package. It is the default.
	InstallSourcePinned InstallSource = "pinned"
	// InstallSourceLatest parses cursor.com/install for the current release. It
	// is an explicit operator override and requires an operator sha256 pin,
	// because no trustworthy digest for that artifact exists in this package.
	InstallSourceLatest InstallSource = "latest"
)

// allowedDownloadHosts is the complete set of hosts the installer may contact,
// on the initial request and on every redirect hop.
var allowedDownloadHosts = []string{"cursor.com", "www.cursor.com", "downloads.cursor.com"}

var (
	packageCandidatePattern = regexp.MustCompile(`https?://[^\s"'<>` + "`" + `]+/agent-cli-package\.tar\.gz`)
	packageTemplatePattern  = regexp.MustCompile(`^https://downloads\.cursor\.com/lab/([0-9]{4}\.[0-9]{2}\.[0-9]{2}-[A-Za-z0-9][A-Za-z0-9._-]{2,63})/\$\{OS\}/\$\{ARCH\}/agent-cli-package\.tar\.gz$`)
)

// PinnedAgentVersion returns the official Cursor Agent CLI release this build
// installs by default.
func PinnedAgentVersion() string { return pinnedAgentVersion }

// PinnedAgentDigest returns the embedded sha256 of the pinned artifact for one
// Go platform.
func PinnedAgentDigest(goos, goarch string) (string, bool) {
	platformOS, platformArch, supported := installerPlatform(goos, goarch)
	if !supported {
		return "", false
	}
	digest, ok := pinnedAgentDigests[platformOS+"/"+platformArch]
	return digest, ok
}

// AgentPackage is one canonical official Cursor Agent CLI artifact.
type AgentPackage struct {
	Version string
	URL     string
}

// ManagedInstall describes the managed official CLI under the plugin data root.
type ManagedInstall struct {
	Installed  bool
	Version    string
	BinaryPath string
	SHA256     string
	Source     InstallSource
	Root       string
	Error      string
}

// InstallResult reports one completed managed installation.
type InstallResult struct {
	Version    string        `json:"version"`
	BinaryPath string        `json:"binary_path"`
	SHA256     string        `json:"sha256"`
	Source     InstallSource `json:"source"`
	Bytes      int64         `json:"bytes"`
}

type managedManifest struct {
	Version string        `json:"version"`
	Binary  string        `json:"binary"`
	SHA256  string        `json:"sha256"`
	Source  InstallSource `json:"source"`
}

// Installer downloads and activates the official Cursor Agent CLI inside the
// persistent plugin data root. It never executes fetched shell script content
// and never extracts or runs an artifact whose sha256 it did not verify first.
type Installer struct {
	Paths *Paths
	// Source selects the artifact. Empty means InstallSourcePinned.
	Source InstallSource
	// ExpectedSHA256 is the operator pin. It overrides the embedded digest for
	// InstallSourcePinned and is mandatory for InstallSourceLatest.
	ExpectedSHA256    string
	GOOS              string
	GOARCH            string
	MaxScriptBytes    int64
	MaxPackageBytes   int64
	MaxArchiveEntries int
	MaxExpandedBytes  int64
	Client            *http.Client

	mu sync.Mutex
}

func allowsDownloadHost(host string) bool {
	for _, allowed := range allowedDownloadHosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}

func (i *Installer) source() InstallSource {
	if strings.EqualFold(strings.TrimSpace(i.Source), InstallSourceLatest) {
		return InstallSourceLatest
	}
	return InstallSourcePinned
}

func (i *Installer) limits() (int64, int64, int, int64) {
	scriptBytes := i.MaxScriptBytes
	if scriptBytes <= 0 {
		scriptBytes = 128 << 10
	}
	packageBytes := i.MaxPackageBytes
	if packageBytes <= 0 {
		packageBytes = 256 << 20
	}
	entries := i.MaxArchiveEntries
	if entries <= 0 {
		entries = 4096
	}
	expanded := i.MaxExpandedBytes
	if expanded <= 0 {
		expanded = 512 << 20
	}
	return scriptBytes, packageBytes, entries, expanded
}

func (i *Installer) platform() (string, string) {
	goos, goarch := i.GOOS, i.GOARCH
	if goos == "" {
		goos = "linux"
	}
	if goarch == "" {
		goarch = "amd64"
	}
	return goos, goarch
}

// Status reports the managed installation without touching the network.
func (i *Installer) Status() ManagedInstall {
	root, err := i.Paths.AgentRoot()
	if err != nil {
		return ManagedInstall{Error: err.Error()}
	}
	status := ManagedInstall{Root: root}
	manifest, binary, err := readManagedManifest(filepath.Join(root, "current"))
	if err != nil {
		return status
	}
	status.Installed = true
	status.Version = manifest.Version
	status.BinaryPath = binary
	status.SHA256 = manifest.SHA256
	status.Source = manifest.Source
	return status
}

// resolveArtifact decides which artifact to download and which sha256 must
// match it. It refuses every combination that would leave the digest unknown.
func (i *Installer) resolveArtifact(ctx context.Context, goos, goarch string, scriptBytes int64) (AgentPackage, string, error) {
	operatorPin := strings.ToLower(strings.TrimSpace(i.ExpectedSHA256))
	if i.source() == InstallSourceLatest {
		if operatorPin == "" {
			return AgentPackage{}, "", fatal("agent_package_pin_required", fmt.Errorf("the latest Cursor Agent release has no trusted checksum in this build; set the agent_package_sha256 configuration key to the sha256 you verified yourself"))
		}
		script, err := i.fetch(ctx, OfficialInstallerURL, scriptBytes)
		if err != nil {
			return AgentPackage{}, "", err
		}
		pkg, err := ParseInstallerScript(script, goos, goarch)
		if err != nil {
			return AgentPackage{}, "", err
		}
		return pkg, operatorPin, nil
	}
	platformOS, platformArch, supported := installerPlatform(goos, goarch)
	if !supported {
		return AgentPackage{}, "", fatal("agent_platform_unsupported", fmt.Errorf("unsupported Cursor Agent platform %s/%s", goos, goarch))
	}
	expected := operatorPin
	if expected == "" {
		embedded, ok := pinnedAgentDigests[platformOS+"/"+platformArch]
		if !ok {
			return AgentPackage{}, "", fatal("agent_package_pin_required", fmt.Errorf("this build embeds no Cursor Agent sha256 for %s/%s; set the agent_package_sha256 configuration key", goos, goarch))
		}
		expected = embedded
	}
	return AgentPackage{Version: pinnedAgentVersion, URL: canonicalArtifactURL(pinnedAgentVersion, platformOS, platformArch)}, expected, nil
}

// Install fetches, verifies, extracts, and activates the official CLI. The
// artifact digest is verified before anything is extracted or executed.
func (i *Installer) Install(ctx context.Context) (InstallResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	root, err := i.Paths.AgentRoot()
	if err != nil {
		return InstallResult{}, err
	}
	scriptBytes, packageBytes, maxEntries, maxExpanded := i.limits()
	goos, goarch := i.platform()

	pkg, expected, err := i.resolveArtifact(ctx, goos, goarch, scriptBytes)
	if err != nil {
		return InstallResult{}, err
	}
	payload, err := i.fetch(ctx, pkg.URL, packageBytes)
	if err != nil {
		return InstallResult{}, err
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	if checksum != expected {
		return InstallResult{}, fatal("agent_package_checksum_mismatch", fmt.Errorf("the Cursor Agent package did not match its trusted sha256"))
	}

	staging, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return InstallResult{}, fatal("agent_install_failed", fmt.Errorf("create staging directory: %w", err))
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(staging)
		}
	}()
	if err = os.Chmod(staging, 0o700); err != nil {
		return InstallResult{}, fatal("agent_install_failed", err)
	}
	relativeBinary, err := extractAgentArchive(bytes.NewReader(payload), staging, maxEntries, maxExpanded)
	if err != nil {
		return InstallResult{}, err
	}
	binary := filepath.Join(staging, relativeBinary)
	if err = verifyContainedPath(staging, binary); err != nil {
		return InstallResult{}, err
	}
	if err = verifyAgentVersion(ctx, staging, binary, pkg.Version); err != nil {
		return InstallResult{}, err
	}
	manifest := managedManifest{Version: pkg.Version, Binary: relativeBinary, SHA256: checksum, Source: i.source()}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return InstallResult{}, fatal("agent_install_failed", err)
	}
	if err = os.WriteFile(filepath.Join(staging, managedManifestName), encoded, 0o600); err != nil {
		return InstallResult{}, fatal("agent_install_failed", err)
	}

	versionsDir := filepath.Join(root, "versions")
	if err = os.MkdirAll(versionsDir, 0o700); err != nil {
		return InstallResult{}, fatal("agent_install_failed", err)
	}
	versionDir := filepath.Join(versionsDir, pkg.Version)
	previous := versionDir + ".previous"
	_ = os.RemoveAll(previous)
	restore := false
	if _, errStat := os.Stat(versionDir); errStat == nil {
		if err = os.Rename(versionDir, previous); err != nil {
			return InstallResult{}, fatal("agent_install_failed", fmt.Errorf("replace the previous Cursor Agent version: %w", err))
		}
		restore = true
	}
	if err = os.Rename(staging, versionDir); err != nil {
		if restore {
			_ = os.Rename(previous, versionDir)
		}
		return InstallResult{}, fatal("agent_install_failed", fmt.Errorf("activate the Cursor Agent version directory: %w", err))
	}
	activated = true
	if err = replaceSymlink(filepath.Join(root, "current"), filepath.Join("versions", pkg.Version)); err != nil {
		if restore {
			_ = os.RemoveAll(versionDir)
			_ = os.Rename(previous, versionDir)
		}
		return InstallResult{}, fatal("agent_install_failed", fmt.Errorf("activate the Cursor Agent current pointer: %w", err))
	}
	_ = os.RemoveAll(previous)
	return InstallResult{Version: pkg.Version, BinaryPath: filepath.Join(root, "current", relativeBinary), SHA256: checksum, Source: i.source(), Bytes: int64(len(payload))}, nil
}

// ParseInstallerScript extracts the single canonical artifact URL from Cursor's
// installer script. Anything else is refused rather than guessed at.
func ParseInstallerScript(body []byte, goos, goarch string) (AgentPackage, error) {
	if len(body) == 0 {
		return AgentPackage{}, fatal("agent_installer_unparsable", fmt.Errorf("the Cursor installer script was empty"))
	}
	platformOS, platformArch, supported := installerPlatform(goos, goarch)
	if !supported {
		return AgentPackage{}, fatal("agent_platform_unsupported", fmt.Errorf("unsupported Cursor Agent platform %s/%s", goos, goarch))
	}
	candidates := packageCandidatePattern.FindAll(body, -1)
	unique := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		unique[string(candidate)] = struct{}{}
	}
	if len(unique) != 1 {
		return AgentPackage{}, fatal("agent_installer_unparsable", fmt.Errorf("the Cursor installer script named %d artifact URLs; exactly one is required", len(unique)))
	}
	matched := packageTemplatePattern.FindSubmatch(candidates[0])
	if len(matched) != 2 {
		return AgentPackage{}, fatal("agent_installer_unparsable", fmt.Errorf("the Cursor installer artifact URL is not canonical"))
	}
	version := string(matched[1])
	return AgentPackage{Version: version, URL: canonicalArtifactURL(version, platformOS, platformArch)}, nil
}

func canonicalArtifactURL(version, platformOS, platformArch string) string {
	return fmt.Sprintf("https://downloads.cursor.com/lab/%s/%s/%s/agent-cli-package.tar.gz", version, platformOS, platformArch)
}

func installerPlatform(goos, goarch string) (string, string, bool) {
	platformOS := map[string]string{"linux": "linux", "darwin": "darwin"}[goos]
	platformArch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	return platformOS, platformArch, platformOS != "" && platformArch != ""
}

// ResolveExecutable returns the official Cursor CLI path: explicit
// configuration first, then the managed install, then PATH.
func ResolveExecutable(configured string, paths *Paths) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fatal(CodeSetupRequired, fmt.Errorf("the configured Cursor Agent executable must be an absolute path"))
		}
		info, err := os.Stat(configured)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fatal(CodeSetupRequired, fmt.Errorf("the configured Cursor Agent executable is not executable"))
		}
		return configured, nil
	}
	// The managed install outranks PATH: its artifact digest was verified
	// against a trusted pin, while anything on PATH was not.
	if paths != nil {
		if root, err := paths.AgentRoot(); err == nil {
			if _, binary, errManifest := readManagedManifest(filepath.Join(root, "current")); errManifest == nil {
				return binary, nil
			}
		}
	}
	if found, err := exec.LookPath(AgentExecutableName); err == nil {
		return found, nil
	}
	return "", fatal(CodeSetupRequired, fmt.Errorf("the official Cursor Agent CLI is not installed"))
}

func readManagedManifest(currentDir string) (managedManifest, string, error) {
	raw, err := os.ReadFile(filepath.Join(currentDir, managedManifestName))
	if err != nil {
		return managedManifest{}, "", err
	}
	var manifest managedManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return managedManifest{}, "", err
	}
	relative, err := safeArchivePath(manifest.Binary)
	if err != nil {
		return managedManifest{}, "", err
	}
	binary := filepath.Join(currentDir, relative)
	info, err := os.Stat(binary)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return managedManifest{}, "", fmt.Errorf("managed Cursor Agent binary is not executable")
	}
	return manifest, binary, nil
}

// verifyDownloadTarget enforces HTTPS and the host allowlist. It is applied to
// the initial URL and to every redirect hop, so no hop can drop to cleartext or
// leave Cursor's own hosts.
func verifyDownloadTarget(target *url.URL) error {
	if target == nil || target.Host == "" {
		return fatal("agent_download_failed", fmt.Errorf("the Cursor download URL is invalid"))
	}
	if target.Scheme != "https" {
		return fatal("agent_download_insecure", fmt.Errorf("the Cursor download URL must use HTTPS"))
	}
	if !allowsDownloadHost(target.Hostname()) {
		return fatal("agent_download_host_refused", fmt.Errorf("download host %q is not allowlisted", target.Hostname()))
	}
	return nil
}

func (i *Installer) fetch(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fatal("agent_download_failed", fmt.Errorf("the Cursor download URL is invalid"))
	}
	if err = verifyDownloadTarget(parsed); err != nil {
		return nil, err
	}
	client := i.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	redirectClient := *client
	redirectClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return verifyDownloadTarget(request.URL)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fatal("agent_download_failed", err)
	}
	response, err := redirectClient.Do(request)
	if err != nil {
		if code := FailureCode(err); code != "" {
			return nil, err
		}
		return nil, fatal("agent_download_failed", fmt.Errorf("the Cursor download did not complete"))
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<10)); _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fatal("agent_download_failed", fmt.Errorf("the Cursor download returned HTTP %d", response.StatusCode))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fatal("agent_download_failed", fmt.Errorf("the Cursor download could not be read"))
	}
	if int64(len(payload)) > limit {
		return nil, fatal("agent_download_too_large", fmt.Errorf("the Cursor download exceeded its size limit"))
	}
	return payload, nil
}

// extractAgentArchive expands the package with pure Go and returns the archive
// relative path of the agent binary.
func extractAgentArchive(reader io.Reader, destination string, maxEntries int, maxExpanded int64) (string, error) {
	unzipped, err := gzip.NewReader(reader)
	if err != nil {
		return "", fatal("agent_archive_invalid", fmt.Errorf("the Cursor package is not a gzip archive"))
	}
	defer func() { _ = unzipped.Close() }()
	archive := tar.NewReader(unzipped)
	seen := make(map[string]struct{})
	entries := 0
	var expanded int64
	binary := ""
	for {
		header, errNext := archive.Next()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return "", fatal("agent_archive_invalid", fmt.Errorf("the Cursor package could not be read"))
		}
		entries++
		if entries > maxEntries {
			return "", fatal("agent_archive_invalid", fmt.Errorf("the Cursor package has too many entries"))
		}
		name, errPath := safeArchivePath(header.Name)
		if errPath != nil {
			return "", fatal("agent_archive_unsafe", errPath)
		}
		if _, duplicate := seen[name]; duplicate {
			return "", fatal("agent_archive_unsafe", fmt.Errorf("the Cursor package contains a duplicate entry"))
		}
		seen[name] = struct{}{}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if errMkdir := os.MkdirAll(target, 0o700); errMkdir != nil {
				return "", fatal("agent_archive_invalid", errMkdir)
			}
		case tar.TypeReg:
			expanded += header.Size
			if expanded > maxExpanded {
				return "", fatal("agent_archive_unsafe", fmt.Errorf("the Cursor package exceeds its expanded size limit"))
			}
			if errMkdir := os.MkdirAll(filepath.Dir(target), 0o700); errMkdir != nil {
				return "", fatal("agent_archive_invalid", errMkdir)
			}
			file, errCreate := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveMode(header.FileInfo().Mode()))
			if errCreate != nil {
				return "", fatal("agent_archive_invalid", errCreate)
			}
			_, errCopy := io.CopyN(file, archive, header.Size)
			errClose := file.Close()
			if errCopy != nil && errCopy != io.EOF {
				return "", fatal("agent_archive_invalid", errCopy)
			}
			if errClose != nil {
				return "", fatal("agent_archive_invalid", errClose)
			}
			if base := filepath.Base(name); base == AgentExecutableName || base == "cursor-agent" {
				binary = name
			}
		case tar.TypeSymlink:
			// Symlinks are never followed out of the staging directory.
			return "", fatal("agent_archive_unsafe", fmt.Errorf("the Cursor package contains a symbolic link"))
		case tar.TypeLink:
			return "", fatal("agent_archive_unsafe", fmt.Errorf("the Cursor package contains a hard link"))
		default:
			return "", fatal("agent_archive_unsafe", fmt.Errorf("the Cursor package contains an unsupported entry type"))
		}
	}
	if binary == "" {
		return "", fatal("agent_archive_invalid", fmt.Errorf("the Cursor package does not contain an agent binary"))
	}
	return binary, nil
}

func safeArchivePath(name string) (string, error) {
	cleaned := filepath.Clean(strings.TrimPrefix(strings.TrimSpace(name), "./"))
	if cleaned == "" || cleaned == "." || cleaned == string(filepath.Separator) {
		return "", fmt.Errorf("unsafe archive path")
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "\x00") {
		return "", fmt.Errorf("unsafe archive path")
	}
	for _, segment := range strings.Split(cleaned, string(filepath.Separator)) {
		if segment == ".." {
			return "", fmt.Errorf("unsafe archive path")
		}
	}
	return cleaned, nil
}

func archiveMode(mode os.FileMode) os.FileMode {
	if mode&0o111 != 0 {
		return 0o700
	}
	return 0o600
}

func verifyContainedPath(root, candidate string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fatal("agent_archive_unsafe", err)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fatal("agent_archive_unsafe", err)
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fatal("agent_archive_unsafe", fmt.Errorf("the Cursor Agent binary escapes its install directory"))
	}
	return nil
}

// verifyAgentVersion runs the freshly downloaded binary once. It is the first
// moment downloaded code executes, so it runs with the same isolation as every
// other child: no inherited environment beyond the allowlist, the staging
// directory as its working directory and config directory, and its own process
// group.
func verifyAgentVersion(ctx context.Context, staging, binary, version string) error {
	versionCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.Command(binary, "--version")
	command.Dir = staging
	command.Env = isolatedEnv(nil, staging)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return fatal("agent_version_unverifiable", fmt.Errorf("the installed Cursor Agent binary could not be started"))
	}
	pgid := command.Process.Pid
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		terminateRemainingGroup(pgid)
		if err != nil {
			return fatal("agent_version_unverifiable", fmt.Errorf("the installed Cursor Agent binary did not report a version"))
		}
	case <-versionCtx.Done():
		// The child is still un-reaped here, so the group id cannot have been
		// recycled and is safe to signal.
		terminateGroup(pgid)
		<-done
		return fatal("agent_version_unverifiable", fmt.Errorf("the installed Cursor Agent binary did not report a version in time"))
	}
	if !strings.Contains(output.String(), version) {
		return fatal("agent_version_mismatch", fmt.Errorf("the installed Cursor Agent binary reported a different version"))
	}
	return nil
}

func replaceSymlink(path, target string) error {
	temporary := path + ".switching"
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
