// Package cursor provides isolated Cursor Agent ACP account execution.
package cursor

import (
	"fmt"
	"path/filepath"
	"strings"
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
	if !filepath.IsAbs(a.ProfileDir) {
		return fmt.Errorf("cursor account %q profile directory must be absolute", a.AuthID)
	}
	return nil
}

// Config contains only non-secret process policy. Authentication stays inside
// a private CURSOR_CONFIG_DIR operated by the official Cursor Agent CLI.
type Config struct {
	Executable     string    `yaml:"executable" json:"executable"`
	Accounts       []Account `yaml:"accounts" json:"accounts"`
	MaxConcurrent  int       `yaml:"max_concurrent" json:"max_concurrent"`
	MaxPromptBytes int       `yaml:"max_prompt_bytes" json:"max_prompt_bytes"`
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Executable) == "" {
		return fmt.Errorf("Cursor Agent executable is required")
	}
	if len(c.Accounts) == 0 {
		return fmt.Errorf("at least one Cursor account is required")
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("max concurrent must be at least 1")
	}
	if c.MaxPromptBytes < 1 {
		return fmt.Errorf("max prompt bytes must be at least 1")
	}
	seen := make(map[string]struct{}, len(c.Accounts))
	for _, account := range c.Accounts {
		if err := account.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[account.AuthID]; duplicate {
			return fmt.Errorf("duplicate Cursor AuthID %q", account.AuthID)
		}
		seen[account.AuthID] = struct{}{}
	}
	return nil
}
