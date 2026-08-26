// Package cursor provides isolated Cursor Agent ACP account execution.
package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const ProviderID = "cursor-acp"

// DefaultModel is the Cursor model alias used when a stored auth record does
// not pin one. Cursor resolves "auto" against the account's own entitlements.
const DefaultModel = "auto"

// Account is one CLIProxyAPI auth record. ProfileDir is private state owned by
// the official Cursor CLI; this package never reads or writes its contents.
// Accounts are created by the login flow and reconstructed from stored auth
// records, never from plugin configuration.
type Account struct {
	AuthID     string `json:"auth_id"`
	Label      string `json:"label"`
	ProfileDir string `json:"profile_dir"`
	Model      string `json:"model"`
	Email      string `json:"email,omitempty"`
}

// modelPattern keeps a stored model name from ever being read as a command-line
// flag or a path by the official CLI.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (a Account) validate() error {
	if strings.TrimSpace(a.AuthID) == "" {
		return fmt.Errorf("cursor account AuthID is required")
	}
	if !modelPattern.MatchString(a.Model) {
		return fmt.Errorf("cursor account %q model must be a bare Cursor model name", a.AuthID)
	}
	if !filepath.IsAbs(a.ProfileDir) {
		return fmt.Errorf("cursor account %q profile directory must be absolute", a.AuthID)
	}
	return nil
}

// Config contains only non-secret process policy. Authentication stays inside a
// private CURSOR_CONFIG_DIR operated by the official Cursor Agent CLI. Every
// field is optional: a plugin installed from the store starts with no operator
// configuration at all.
type Config struct {
	Executable string `yaml:"executable" json:"executable"`
	DataRoot   string `yaml:"data_root" json:"data_root"`
	// AgentInstallSource is "pinned" (default, release-pinned artifact verified
	// against an embedded sha256) or "latest" (parse cursor.com/install, which
	// requires AgentPackageSHA256).
	AgentInstallSource string        `yaml:"agent_install_source" json:"agent_install_source"`
	AgentPackageSHA256 string        `yaml:"agent_package_sha256" json:"agent_package_sha256"`
	MaxConcurrent      int           `yaml:"max_concurrent" json:"max_concurrent"`
	MaxPromptBytes     int           `yaml:"max_prompt_bytes" json:"max_prompt_bytes"`
	MaxOutputBytes     int           `yaml:"max_output_bytes" json:"max_output_bytes"`
	WorkspaceRoot      string        `yaml:"workspace_root" json:"workspace_root"`
	Timeout            time.Duration `yaml:"timeout" json:"timeout"`
}

// NormalizeConfig applies defaults and validates the operator-supplied values
// that carry a filesystem or process risk.
func NormalizeConfig(c Config) (Config, error) {
	c.Executable = strings.TrimSpace(c.Executable)
	if c.Executable != "" {
		if !filepath.IsAbs(c.Executable) {
			return Config{}, fmt.Errorf("Cursor Agent executable must be an absolute trusted path")
		}
		if info, err := os.Stat(c.Executable); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return Config{}, fmt.Errorf("Cursor Agent executable is not executable")
		}
	}
	c.DataRoot = strings.TrimSpace(c.DataRoot)
	if c.DataRoot != "" && !filepath.IsAbs(c.DataRoot) {
		return Config{}, fmt.Errorf("data root must be an absolute path")
	}
	c.AgentPackageSHA256 = strings.ToLower(strings.TrimSpace(c.AgentPackageSHA256))
	if c.AgentPackageSHA256 != "" {
		if _, err := hex.DecodeString(c.AgentPackageSHA256); err != nil || len(c.AgentPackageSHA256) != sha256.Size*2 {
			return Config{}, fmt.Errorf("agent package sha256 must be a 64 character hex digest")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.AgentInstallSource)) {
	case "", InstallSourcePinned:
		c.AgentInstallSource = InstallSourcePinned
	case InstallSourceLatest:
		c.AgentInstallSource = InstallSourceLatest
	default:
		return Config{}, fmt.Errorf("agent install source must be %q or %q", InstallSourcePinned, InstallSourceLatest)
	}
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 2
	}
	if c.MaxPromptBytes < 1 {
		c.MaxPromptBytes = 512 << 10
	}
	if c.MaxOutputBytes < 1 {
		c.MaxOutputBytes = 1 << 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	c.WorkspaceRoot = strings.TrimSpace(c.WorkspaceRoot)
	if c.WorkspaceRoot != "" {
		workspace, err := secureDirectory(c.WorkspaceRoot)
		if err != nil {
			return Config{}, fmt.Errorf("workspace root: %w", err)
		}
		c.WorkspaceRoot = workspace
	}
	return c, nil
}

func secureDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("must be a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("must not grant group or other access")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return "", fmt.Errorf("must be owned by the service user")
	}
	return canonical, nil
}
