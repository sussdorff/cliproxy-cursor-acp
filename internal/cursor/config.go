// Package cursor provides isolated Cursor Agent ACP account execution.
package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const ProviderID = "cursor-acp"

// Account is one CLIProxyAPI auth record. ProfileDir is private state owned by
// the official Cursor CLI; this package never reads or writes its contents.
type Account struct {
	AuthID     string `yaml:"auth_id" json:"auth_id"`
	Label      string `yaml:"label" json:"label"`
	ProfileDir string `yaml:"profile_dir" json:"profile_dir"`
	Model      string `yaml:"model" json:"model"`
}

func (a Account) validate() error {
	if strings.TrimSpace(a.AuthID) == "" {
		return fmt.Errorf("cursor account AuthID is required")
	}
	if strings.TrimSpace(a.Model) == "" || strings.Contains(a.Model, "/") {
		return fmt.Errorf("cursor account %q model must be a bare Cursor model name", a.AuthID)
	}
	if !filepath.IsAbs(a.ProfileDir) {
		return fmt.Errorf("cursor account %q profile directory must be absolute", a.AuthID)
	}
	return nil
}

// Config contains only non-secret process policy. Authentication stays inside
// a private CURSOR_CONFIG_DIR operated by the official Cursor Agent CLI.
type Config struct {
	Executable     string        `yaml:"executable" json:"executable"`
	Accounts       []Account     `yaml:"accounts" json:"accounts"`
	MaxConcurrent  int           `yaml:"max_concurrent" json:"max_concurrent"`
	MaxPromptBytes int           `yaml:"max_prompt_bytes" json:"max_prompt_bytes"`
	MaxOutputBytes int           `yaml:"max_output_bytes" json:"max_output_bytes"`
	WorkspaceRoot  string        `yaml:"workspace_root" json:"workspace_root"`
	Timeout        time.Duration `yaml:"timeout" json:"timeout"`
}

// NormalizeConfig validates filesystem ownership/permissions and resolves paths.
func NormalizeConfig(c Config) (Config, error) {
	if strings.TrimSpace(c.Executable) == "" {
		return Config{}, fmt.Errorf("Cursor Agent executable is required")
	}
	if !filepath.IsAbs(c.Executable) {
		return Config{}, fmt.Errorf("Cursor Agent executable must be an absolute trusted path")
	}
	if info, err := os.Stat(c.Executable); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return Config{}, fmt.Errorf("Cursor Agent executable is not executable")
	}
	if len(c.Accounts) == 0 {
		return Config{}, fmt.Errorf("at least one Cursor account is required")
	}
	if c.MaxConcurrent < 1 {
		return Config{}, fmt.Errorf("max concurrent must be at least 1")
	}
	if c.MaxPromptBytes < 1 {
		return Config{}, fmt.Errorf("max prompt bytes must be at least 1")
	}
	if c.MaxOutputBytes < 1 {
		return Config{}, fmt.Errorf("max output bytes must be at least 1")
	}
	if c.Timeout <= 0 {
		return Config{}, fmt.Errorf("timeout must be positive")
	}
	workspace, err := secureDirectory(c.WorkspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("workspace root: %w", err)
	}
	c.WorkspaceRoot = workspace
	seen := make(map[string]struct{}, len(c.Accounts))
	for index, account := range c.Accounts {
		if err := account.validate(); err != nil {
			return Config{}, err
		}
		profile, err := secureDirectory(account.ProfileDir)
		if err != nil {
			return Config{}, fmt.Errorf("cursor account %q profile directory: %w", account.AuthID, err)
		}
		account.ProfileDir = profile
		if _, duplicate := seen[account.AuthID]; duplicate {
			return Config{}, fmt.Errorf("duplicate Cursor AuthID %q", account.AuthID)
		}
		seen[account.AuthID] = struct{}{}
		c.Accounts[index] = account
	}
	profiles := make(map[string]string, len(c.Accounts))
	for _, account := range c.Accounts {
		if prior, exists := profiles[account.ProfileDir]; exists {
			return Config{}, fmt.Errorf("cursor accounts %q and %q resolve to the same profile directory", prior, account.AuthID)
		}
		profiles[account.ProfileDir] = account.AuthID
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
