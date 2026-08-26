package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// dataRootDirName is appended to the host auth directory when the plugin
// configuration does not name a data root of its own.
const dataRootDirName = "cliproxy-cursor-acp"

// Paths owns the persistent plugin data root and the directories derived from
// it. CLIProxyAPI only reveals its auth directory on auth and model requests,
// so resolution is deferred until either configuration or the host supplies a
// usable location.
type Paths struct {
	mu            sync.Mutex
	configured    string
	workspaceRoot string
	hostAuthDir   string
}

func NewPaths(dataRoot, workspaceRoot string) *Paths {
	return &Paths{configured: strings.TrimSpace(dataRoot), workspaceRoot: strings.TrimSpace(workspaceRoot)}
}

// ObserveHost records the first non-empty host auth directory. Later values are
// ignored so a restarted account never migrates to a second data root.
func (p *Paths) ObserveHost(authDir string) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" || !filepath.IsAbs(authDir) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hostAuthDir == "" {
		p.hostAuthDir = authDir
	}
}

// Root returns the private plugin data root, creating it when necessary.
func (p *Paths) Root() (string, error) {
	p.mu.Lock()
	configured, hostAuthDir := p.configured, p.hostAuthDir
	p.mu.Unlock()
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", ValidationFailure("data_root_invalid", "the data_root configuration key must be an absolute path")
		}
		return ensurePrivateDir(configured)
	}
	if hostAuthDir != "" {
		return ensurePrivateDir(filepath.Join(hostAuthDir, dataRootDirName))
	}
	return "", ValidationFailure("data_root_unresolved", "set the data_root configuration key to a persistent directory; the host auth directory is not known yet")
}

// Workspace returns the directory offered to the Cursor Agent as its working
// directory. It never contains credential material.
func (p *Paths) Workspace() (string, error) {
	p.mu.Lock()
	configured := p.workspaceRoot
	p.mu.Unlock()
	if configured != "" {
		return secureDirectory(configured)
	}
	root, err := p.Root()
	if err != nil {
		return "", err
	}
	return ensurePrivateDir(filepath.Join(root, "workspace"))
}

// ProfilesRoot returns the parent of every per-login CURSOR_CONFIG_DIR.
func (p *Paths) ProfilesRoot() (string, error) {
	root, err := p.Root()
	if err != nil {
		return "", err
	}
	return ensurePrivateDir(filepath.Join(root, "profiles"))
}

// AgentRoot returns the parent of the managed official Cursor CLI installs.
func (p *Paths) AgentRoot() (string, error) {
	root, err := p.Root()
	if err != nil {
		return "", err
	}
	return ensurePrivateDir(filepath.Join(root, "agent"))
}

func ensurePrivateDir(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("secure %s: %w", filepath.Base(path), err)
	}
	return secureDirectory(path)
}
