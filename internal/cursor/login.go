package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// CodeSetupRequired marks the failure raised when no official Cursor CLI is
// resolvable. The login UI turns it into a link to the plugin setup page.
const CodeSetupRequired = "setup_required"

const (
	loginStdoutFile = ".login-stdout"
	loginStderrFile = ".login-stderr"
	// defaultLoginCaptureBytes bounds both what the CLI may write to its capture
	// files and what is read back from them. The two must agree: a smaller read
	// window would hide an approval URL printed after a chatty startup.
	defaultLoginCaptureBytes = 1 << 20
)

// approvalURLPattern is anchored on Cursor's own host so no other URL printed
// by the CLI can be presented to an operator as a login approval link.
var approvalURLPattern = regexp.MustCompile(`https://(?:www\.)?cursor\.com/[^\s"'<>` + "`" + `]+`)

// LoginStart is the user-facing result of starting a Cursor login.
type LoginStart struct {
	URL       string
	State     string
	ExpiresAt time.Time
}

// LoginResult reports one poll of a running or finished login.
type LoginResult struct {
	Pending       bool
	Authenticated bool
	Message       string
	Account       Account
	Tier          string
	Version       string
}

type loginSession struct {
	profileDir string
	pgid       int
	expiresAt  time.Time
	done       chan struct{}
	waitErr    error
	overflow   atomic.Bool
	cleanup    sync.Once
}

// terminate kills the login's process group. The group is only signalled by its
// id while the leader is still un-reaped; once the leader has been reaped that
// id may have been recycled, so the group's existence is probed first.
func (s *loginSession) terminate() {
	select {
	case <-s.done:
		terminateRemainingGroup(s.pgid)
	default:
		terminateGroup(s.pgid)
	}
}

// discard kills the login's process group and removes its private profile. It
// runs at most once per session no matter how many callers reach it.
func (s *loginSession) discard() {
	s.cleanup.Do(func() {
		s.terminate()
		<-s.done
		// Cursor's detached worker can outlive the leader; reap what is left of
		// the group now that waiting is over.
		terminateRemainingGroup(s.pgid)
		_ = os.RemoveAll(s.profileDir)
	})
}

// Login runs the official Cursor CLI login in a fresh private profile per
// attempt. The plugin never reads the credential material the CLI writes there.
type Login struct {
	Paths      *Paths
	Executable func() (string, error)
	// LoginArgs, StatusArgs, and AboutArgs default to the official CLI verbs.
	// Tests replace them to drive a fake agent executable.
	LoginArgs  []string
	StatusArgs []string
	AboutArgs  []string
	BaseEnv    []string
	ExtraEnv   []string
	URLTimeout time.Duration
	Expiry     time.Duration
	Timeout    time.Duration
	// MaxCaptureBytes bounds the login capture files on disk, not just the
	// bytes read back from them.
	MaxCaptureBytes int64

	mu       sync.Mutex
	closed   bool
	starting sync.WaitGroup
	sessions map[string]*loginSession
}

func (l *Login) urlTimeout() time.Duration {
	if l.URLTimeout > 0 {
		return l.URLTimeout
	}
	return 15 * time.Second
}

func (l *Login) expiry() time.Duration {
	if l.Expiry > 0 {
		return l.Expiry
	}
	return 15 * time.Minute
}

func (l *Login) probeTimeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return 30 * time.Second
}

func (l *Login) captureLimit() int64 {
	if l.MaxCaptureBytes > 0 {
		return l.MaxCaptureBytes
	}
	return defaultLoginCaptureBytes
}

func (l *Login) arguments(configured []string, verb string) []string {
	if len(configured) > 0 {
		return configured
	}
	switch verb {
	case "login":
		return []string{"login"}
	case "status":
		return []string{"status", "--format", "json"}
	default:
		return []string{"about", "--format", "json"}
	}
}

// Start creates a private profile, launches the official CLI login inside it,
// and returns the single approval URL the CLI printed.
func (l *Login) Start(ctx context.Context) (start LoginStart, err error) {
	l.sweepExpired()
	// Enter the start generation before doing anything observable, so Close can
	// wait for this attempt instead of racing past its process and profile.
	if err = l.enterStart(); err != nil {
		return LoginStart{}, err
	}
	defer l.starting.Done()
	executable, err := l.resolveExecutable()
	if err != nil {
		return LoginStart{}, err
	}
	profilesRoot, err := l.Paths.ProfilesRoot()
	if err != nil {
		return LoginStart{}, err
	}
	profileID, err := randomHex(12)
	if err != nil {
		return LoginStart{}, err
	}
	state, err := randomHex(16)
	if err != nil {
		return LoginStart{}, err
	}
	profileDir := filepath.Join(profilesRoot, profileID)
	if err = os.Mkdir(profileDir, 0o700); err != nil {
		return LoginStart{}, fatal("login_profile_unavailable", fmt.Errorf("create Cursor login profile: %w", err))
	}
	if err = os.Chmod(profileDir, 0o700); err != nil {
		_ = os.RemoveAll(profileDir)
		return LoginStart{}, fatal("login_profile_unavailable", fmt.Errorf("secure Cursor login profile: %w", err))
	}
	workingDir, err := l.Paths.Workspace()
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return LoginStart{}, err
	}

	stdout, stderr, err := openCaptures(profileDir)
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return LoginStart{}, err
	}
	command := exec.Command(executable, l.arguments(l.LoginArgs, "login")...)
	command.Dir = workingDir
	command.Env = append(loginEnv(l.BaseEnv, profileDir), l.ExtraEnv...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = stdout
	command.Stderr = stderr
	if err = command.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.RemoveAll(profileDir)
		return LoginStart{}, fatal("login_start_failed", fmt.Errorf("start Cursor login: %w", err))
	}
	pgid := command.Process.Pid
	done := make(chan struct{})
	session := &loginSession{profileDir: profileDir, pgid: pgid, expiresAt: time.Now().Add(l.expiry()), done: done}
	go func() {
		session.waitErr = command.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		close(done)
	}()
	// Register before the approval URL exists: a concurrent Close must be able
	// to see and clean up this process even while it is still starting up.
	if err = l.register(state, session); err != nil {
		session.discard()
		return LoginStart{}, err
	}
	go l.watchCaptures(session)

	approval, err := l.awaitApprovalURL(ctx, session)
	if err != nil {
		l.drop(state)
		session.discard()
		return LoginStart{}, err
	}
	return LoginStart{URL: approval, State: state, ExpiresAt: session.expiresAt}, nil
}

// enterStart joins the current start generation, refusing once Close has run.
func (l *Login) enterStart() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fatal("login_unavailable", fmt.Errorf("the Cursor login service is shutting down"))
	}
	l.starting.Add(1)
	return nil
}

// register publishes a running login, or refuses when Close ran meanwhile so
// the caller cleans up its own process and profile.
func (l *Login) register(state string, session *loginSession) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fatal("login_unavailable", fmt.Errorf("the Cursor login service is shutting down"))
	}
	if l.sessions == nil {
		l.sessions = make(map[string]*loginSession)
	}
	l.sessions[state] = session
	return nil
}

// watchCaptures enforces the on-disk capture bound for the whole life of the
// login, not only while Start is scanning for the approval URL.
func (l *Login) watchCaptures(session *loginSession) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
			if captureBytes(session.profileDir) > l.captureLimit() {
				session.overflow.Store(true)
				session.terminate()
				return
			}
		}
	}
}

func captureBytes(profileDir string) int64 {
	var total int64
	for _, name := range []string{loginStdoutFile, loginStderrFile} {
		if info, err := os.Stat(filepath.Join(profileDir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// take removes one login from the table and reports whether this caller is the
// one that removed it.
func (l *Login) take(state string) (*loginSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	session, ok := l.sessions[strings.TrimSpace(state)]
	if ok {
		delete(l.sessions, strings.TrimSpace(state))
	}
	return session, ok
}

func (l *Login) drop(state string) {
	l.mu.Lock()
	delete(l.sessions, strings.TrimSpace(state))
	l.mu.Unlock()
}

// sweepExpired removes logins nobody polled to completion, so an abandoned
// attempt cannot keep a child process and a private profile alive.
func (l *Login) sweepExpired() { l.sweepExpiredExcept("") }

// sweepExpiredExcept leaves one state in place so its own poll can report the
// expiry as a typed result instead of an unknown state.
func (l *Login) sweepExpiredExcept(keep string) {
	now := time.Now()
	keep = strings.TrimSpace(keep)
	l.mu.Lock()
	expired := make([]*loginSession, 0, 2)
	for state, session := range l.sessions {
		if state != keep && now.After(session.expiresAt) {
			expired = append(expired, session)
			delete(l.sessions, state)
		}
	}
	l.mu.Unlock()
	for _, session := range expired {
		session.discard()
	}
}

// Poll reports whether the login is still running, failed, or produced an
// authenticated account.
func (l *Login) Poll(ctx context.Context, state string) (LoginResult, error) {
	// Abandoned logins are cleaned up on every poll too, so no operator action
	// is needed to reclaim them.
	l.sweepExpiredExcept(state)
	l.mu.Lock()
	session := l.sessions[strings.TrimSpace(state)]
	l.mu.Unlock()
	if session == nil {
		return LoginResult{}, fatal("unknown_login_state", fmt.Errorf("Cursor login state is unknown or already finished"))
	}
	// Expiry is a hard gate: a login that finished before its deadline is still
	// refused once that deadline passed.
	if time.Now().After(session.expiresAt) {
		if taken, ok := l.take(state); ok {
			taken.discard()
		}
		return LoginResult{Message: "the Cursor login expired before it was completed"}, nil
	}
	select {
	case <-session.done:
	default:
		return LoginResult{Pending: true, Message: "waiting for the Cursor login to be approved"}, nil
	}
	if session.overflow.Load() {
		l.finish(state, session)
		return LoginResult{Message: "the official Cursor CLI login produced more output than the plugin accepts"}, nil
	}
	if session.waitErr != nil {
		l.finish(state, session)
		return LoginResult{Message: "the official Cursor CLI login did not complete"}, nil
	}
	account, tier, version, err := l.confirm(ctx, session.profileDir)
	if err != nil {
		l.finish(state, session)
		return LoginResult{Message: publicLoginMessage(err)}, nil
	}
	// The login leader has already been reaped, but Cursor can leave a worker in
	// its process group. The authenticated profile persists independently.
	terminateRemainingGroup(session.pgid)
	l.drop(state)
	// The approval URL lives in these captures; the authenticated profile keeps
	// only what the official CLI put there.
	for _, name := range []string{loginStdoutFile, loginStderrFile} {
		_ = os.Remove(filepath.Join(session.profileDir, name))
	}
	tightenQuotaCredentialMode(session.profileDir)
	return LoginResult{Authenticated: true, Message: "Cursor account authenticated", Account: account, Tier: tier, Version: version}, nil
}

func tightenQuotaCredentialMode(profileDir string) {
	for _, store := range []string{"cursor", ".cursor"} {
		path := filepath.Join(profileDir, store, "auth.json")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			_ = os.Chmod(path, 0o600)
			_ = os.Chmod(filepath.Dir(path), 0o700)
		}
	}
}

// Close terminates every unfinished login and removes its private profile. It
// waits for in-flight Start calls first, so no login can be published after it
// returns, and later Start calls are refused.
func (l *Login) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.starting.Wait()
	l.mu.Lock()
	sessions := l.sessions
	l.sessions = nil
	l.mu.Unlock()
	for _, session := range sessions {
		session.discard()
	}
}

func (l *Login) resolveExecutable() (string, error) {
	if l.Executable == nil {
		return "", fatal(CodeSetupRequired, fmt.Errorf("no official Cursor CLI resolver is configured"))
	}
	executable, err := l.Executable()
	if err != nil || strings.TrimSpace(executable) == "" {
		return "", fatal(CodeSetupRequired, fmt.Errorf("the official Cursor CLI is not installed"))
	}
	return executable, nil
}

func (l *Login) awaitApprovalURL(ctx context.Context, session *loginSession) (string, error) {
	deadline := time.Now().Add(l.urlTimeout())
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if session.overflow.Load() || captureBytes(session.profileDir) > l.captureLimit() {
			session.overflow.Store(true)
			return "", fatal("login_output_too_large", fmt.Errorf("the official Cursor CLI login produced more output than the plugin accepts"))
		}
		approval, err := scanApprovalURL(session.profileDir, l.captureLimit())
		if err != nil {
			return "", err
		}
		if approval != "" {
			return approval, nil
		}
		select {
		case <-session.done:
			// One last read: the CLI may have printed the URL just before exit.
			if session.overflow.Load() {
				return "", fatal("login_output_too_large", fmt.Errorf("the official Cursor CLI login produced more output than the plugin accepts"))
			}
			approval, err = scanApprovalURL(session.profileDir, l.captureLimit())
			if err != nil {
				return "", err
			}
			if approval != "" {
				return approval, nil
			}
			return "", fatal("approval_url_unavailable", fmt.Errorf("the official Cursor CLI exited without printing an approval URL"))
		case <-ctx.Done():
			return "", retryable("login_cancelled", ctx.Err())
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return "", fatal("approval_url_unavailable", fmt.Errorf("the official Cursor CLI did not print an approval URL"))
		}
	}
}

func scanApprovalURL(profileDir string, limit int64) (string, error) {
	found := make(map[string]struct{})
	for _, name := range []string{loginStdoutFile, loginStderrFile} {
		text, err := readCapture(filepath.Join(profileDir, name), limit)
		if err != nil {
			return "", fatal("login_output_unreadable", err)
		}
		for _, candidate := range approvalURLPattern.FindAllString(text, -1) {
			candidate = strings.TrimRight(candidate, ".,)")
			parsed, errParse := url.Parse(candidate)
			if errParse != nil || parsed.Scheme != "https" {
				continue
			}
			if host := parsed.Hostname(); host != "cursor.com" && host != "www.cursor.com" {
				continue
			}
			found[parsed.String()] = struct{}{}
		}
	}
	if len(found) > 1 {
		return "", fatal("approval_url_ambiguous", fmt.Errorf("the official Cursor CLI printed more than one approval URL"))
	}
	for candidate := range found {
		return candidate, nil
	}
	return "", nil
}

func (l *Login) confirm(ctx context.Context, profileDir string) (Account, string, string, error) {
	status, err := l.runProbe(ctx, profileDir, l.arguments(l.StatusArgs, "status"))
	if err != nil {
		return Account{}, "", "", err
	}
	known, authenticated := parseAuthStatus(status)
	if !known || !authenticated {
		return Account{}, "", "", fatal("login_not_authenticated", fmt.Errorf("the official Cursor CLI did not report an authenticated account"))
	}
	about, err := l.runProbe(ctx, profileDir, l.arguments(l.AboutArgs, "about"))
	if err != nil {
		return Account{}, "", "", err
	}
	email, tier, version := parseAbout(about)
	email = strings.TrimSpace(email)
	if email == "" {
		return Account{}, "", "", fatal("login_account_unavailable", fmt.Errorf("the official Cursor CLI did not report an account email"))
	}
	account := Account{
		AuthID:     accountIdentity(email, filepath.Base(profileDir)),
		Label:      firstNonEmpty(email, filepath.Base(profileDir)),
		ProfileDir: profileDir,
		Model:      DefaultModel,
		Email:      email,
	}
	return account, tier, version, nil
}

func (l *Login) runProbe(ctx context.Context, profileDir string, arguments []string) (string, error) {
	executable, err := l.resolveExecutable()
	if err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, l.probeTimeout())
	defer cancel()
	capture, err := os.CreateTemp(profileDir, ".probe-*")
	if err != nil {
		return "", fatal("login_probe_failed", fmt.Errorf("create Cursor probe capture: %w", err))
	}
	defer func() {
		_ = capture.Close()
		_ = os.Remove(capture.Name())
	}()
	command := exec.Command(executable, arguments...)
	command.Env = append(loginEnv(l.BaseEnv, profileDir), l.ExtraEnv...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = capture
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		return "", fatal("login_probe_failed", fmt.Errorf("start Cursor probe: %w", err))
	}
	pgid := command.Process.Pid
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
		// The leader is reaped here, so its group id may already be free. Only a
		// group that still has members (Cursor's detached worker) is signalled.
		terminateRemainingGroup(pgid)
	case <-probeCtx.Done():
		// The child is still un-reaped, so the group id is certainly ours.
		terminateGroup(pgid)
		<-done
		terminateRemainingGroup(pgid)
		return "", retryable("login_probe_timeout", probeCtx.Err())
	}
	text, readErr := readCapture(capture.Name(), l.captureLimit())
	if readErr != nil {
		return "", fatal("login_probe_failed", readErr)
	}
	if waitErr != nil {
		return text, fatal("login_probe_failed", fmt.Errorf("the official Cursor CLI probe failed"))
	}
	return text, nil
}

// finish removes a failed login from the table and cleans it up exactly once.
func (l *Login) finish(state string, session *loginSession) {
	l.drop(state)
	session.discard()
}

func openCaptures(profileDir string) (*os.File, *os.File, error) {
	stdout, err := os.OpenFile(filepath.Join(profileDir, loginStdoutFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fatal("login_output_unreadable", fmt.Errorf("create Cursor login capture: %w", err))
	}
	stderr, err := os.OpenFile(filepath.Join(profileDir, loginStderrFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fatal("login_output_unreadable", fmt.Errorf("create Cursor login capture: %w", err))
	}
	return stdout, stderr, nil
}

// readCapture reads a bounded prefix of a capture file. Cursor keeps a detached
// worker alive after login, so captures are files rather than inherited pipes.
func readCapture(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = file.Close() }()
	if limit <= 0 {
		limit = defaultLoginCaptureBytes
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// loginEnv keeps the process allowlist used for ACP execution, pins the private
// profile, suppresses browser opening, and blanks any inherited API key.
func loginEnv(base []string, profileDir string) []string {
	return append(isolatedEnv(base, profileDir), "NO_OPEN_BROWSER=1", "CURSOR_API_KEY=")
}

func terminateGroup(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fatal("random_unavailable", fmt.Errorf("no cryptographic randomness available"))
	}
	return hex.EncodeToString(raw), nil
}

// accountIdentity is stable across re-logins of the same Cursor account so a
// repeated login refreshes one host auth record instead of multiplying it.
func accountIdentity(email, profileID string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "cursor-" + profileID
	}
	sum := sha256.Sum256([]byte(email))
	return "cursor-" + hex.EncodeToString(sum[:])[:12]
}

func parseAuthStatus(text string) (known bool, authenticated bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return false, false
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err == nil {
		return authStatusFromJSON(decoded)
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if strings.Contains(line, "not logged in") || strings.Contains(line, "not authenticated") || strings.Contains(line, "login required") {
			return true, false
		}
		if strings.Contains(line, "logged in") || strings.Contains(line, "authenticated") {
			return true, true
		}
	}
	return false, false
}

func authStatusFromJSON(value any) (known bool, authenticated bool) {
	results := make([]bool, 0, 4)
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, nested := range typed {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "isauthenticated", "authenticated", "loggedin":
					if flag, ok := nested.(bool); ok {
						results = append(results, flag)
					}
				}
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		}
	}
	walk(value)
	if len(results) == 0 {
		return false, false
	}
	for _, flag := range results {
		if flag != results[0] {
			return true, false
		}
	}
	return true, results[0]
}

func parseAbout(text string) (email, tier, version string) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded); err != nil {
		return "", "", ""
	}
	email = firstString(decoded, "userEmail", "user_email", "email")
	if email == "" {
		if account, ok := decoded["account"].(map[string]any); ok {
			email = firstString(account, "email")
		}
	}
	if !strings.Contains(email, "@") {
		email = ""
	}
	return email, firstString(decoded, "tier", "plan", "subscriptionTier", "subscription_tier"), firstString(decoded, "version", "cliVersion", "cli_version")
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := raw[key].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// publicLoginMessage keeps child output and paths out of the login UI.
func publicLoginMessage(err error) string {
	switch FailureCode(err) {
	case CodeSetupRequired:
		return "the official Cursor CLI is not installed"
	case "login_not_authenticated":
		return "the official Cursor CLI did not report an authenticated account"
	case "login_probe_timeout":
		return "the official Cursor CLI did not answer the authentication probe in time"
	default:
		return "the Cursor login could not be confirmed"
	}
}
