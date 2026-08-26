package cursor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestAgentHelperProcess impersonates the official Cursor CLI. It never touches
// the network and stores its "credential" as an opaque marker inside the
// CURSOR_CONFIG_DIR it was given, exactly where the real CLI keeps its own.
func TestAgentHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_FAKE_AGENT") != "1" {
		return
	}
	profile := os.Getenv("CURSOR_CONFIG_DIR")
	marker := filepath.Join(profile, "fake-account")
	email := "user-" + filepath.Base(profile) + "@example.test"
	switch os.Args[len(os.Args)-1] {
	case "login":
		if os.Getenv("NO_OPEN_BROWSER") != "1" {
			os.Exit(3)
		}
		if os.Getenv("CURSOR_API_KEY") != "" {
			os.Exit(4)
		}
		if profile == "" {
			os.Exit(5)
		}
		approval := "https://cursor.com/loginDeepControl?challenge=" + filepath.Base(profile)
		switch os.Getenv("FAKE_AGENT_LOGIN") {
		case "nourl":
			fmt.Println("Opening browser...")
			time.Sleep(3 * time.Second)
		case "multi":
			fmt.Println(approval)
			fmt.Println("https://cursor.com/loginDeepControl?challenge=other")
		case "fail":
			fmt.Println(approval)
			time.Sleep(50 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "login rejected for token sk-secret-value")
			os.Exit(1)
		case "slow":
			fmt.Println(approval)
			time.Sleep(700 * time.Millisecond)
			_ = os.WriteFile(marker, []byte(email), 0o600)
		case "flood":
			fmt.Println(approval)
			for index := 0; index < 4096; index++ {
				fmt.Println(strings.Repeat("noise ", 64))
			}
			time.Sleep(5 * time.Second)
			_ = os.WriteFile(marker, []byte(email), 0o600)
		case "floodnourl":
			for index := 0; index < 4096; index++ {
				fmt.Println(strings.Repeat("noise ", 64))
			}
			time.Sleep(5 * time.Second)
		case "linger":
			fmt.Println(approval)
			time.Sleep(30 * time.Second)
			_ = os.WriteFile(marker, []byte(email), 0o600)
		case "chatty":
			// More than the historical 64 KiB read window before the URL.
			for index := 0; index < 2048; index++ {
				fmt.Println(strings.Repeat("progress ", 8))
			}
			fmt.Println(approval)
			_ = os.WriteFile(marker, []byte(email), 0o600)
		case "detached":
			fmt.Println(approval)
			_ = os.WriteFile(marker, []byte(email), 0o600)
			detached := exec.Command(os.Args[0], "-test.run=TestAgentHelperProcess", "--", "detached")
			detached.Env = append(os.Environ(), "GO_WANT_FAKE_AGENT=1", "CURSOR_CONFIG_DIR="+profile)
			if err := detached.Start(); err != nil {
				os.Exit(7)
			}
			_ = os.WriteFile(filepath.Join(profile, "detached-pid"), []byte(strconv.Itoa(detached.Process.Pid)), 0o600)
		default:
			fmt.Println(approval)
			_ = os.WriteFile(marker, []byte(email), 0o600)
		}
	case "status":
		if _, err := os.Stat(marker); err != nil {
			fmt.Println(`{"isAuthenticated":false}`)
			os.Exit(0)
		}
		fmt.Println(`{"isAuthenticated":true}`)
	case "about":
		if os.Getenv("FAKE_AGENT_ABOUT") == "fail" {
			os.Exit(8)
		}
		stored, err := os.ReadFile(marker)
		if err != nil {
			os.Exit(1)
		}
		if os.Getenv("FAKE_AGENT_ABOUT") == "empty" {
			fmt.Println(`{"tier":"pro","version":"2026.08.11"}`)
			break
		}
		fmt.Printf("{\"userEmail\":%q,\"tier\":\"pro\",\"version\":\"2026.08.11\"}\n", string(stored))
	case "detached":
		time.Sleep(30 * time.Second)
	default:
		os.Exit(6)
	}
	os.Exit(0)
}

func testLogin(t *testing.T, behaviour string) *Login {
	t.Helper()
	extra := []string{"GO_WANT_FAKE_AGENT=1"}
	if behaviour != "" {
		extra = append(extra, "FAKE_AGENT_LOGIN="+behaviour)
	}
	helper := []string{"-test.run=TestAgentHelperProcess", "--"}
	return &Login{
		Paths:      NewPaths(t.TempDir(), ""),
		Executable: func() (string, error) { return os.Args[0], nil },
		LoginArgs:  append(append([]string{}, helper...), "login"),
		StatusArgs: append(append([]string{}, helper...), "status"),
		AboutArgs:  append(append([]string{}, helper...), "about"),
		ExtraEnv:   extra,
		URLTimeout: 4 * time.Second,
		Timeout:    10 * time.Second,
	}
}

func waitForLogin(t *testing.T, login *Login, state string) LoginResult {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		result, err := login.Poll(context.Background(), state)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if !result.Pending {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatal("login poll never left the pending state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLoginStartReturnsApprovalURLAndPollCreatesPrivateAccount(t *testing.T) {
	login := testLogin(t, "slow")
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(start.URL, "https://cursor.com/") {
		t.Fatalf("approval URL = %q", start.URL)
	}
	if len(start.State) < 16 || strings.ContainsAny(start.State, "/\\ ") {
		t.Fatalf("login state = %q", start.State)
	}
	if !start.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry = %v", start.ExpiresAt)
	}
	if pending, errPoll := login.Poll(context.Background(), start.State); errPoll != nil || !pending.Pending {
		t.Fatalf("running login = %#v %v", pending, errPoll)
	}
	result := waitForLogin(t, login, start.State)
	if !result.Authenticated {
		t.Fatalf("login result = %#v", result)
	}
	if result.Account.Email != "user-"+filepath.Base(result.Account.ProfileDir)+"@example.test" {
		t.Fatalf("account = %#v", result.Account)
	}
	if !strings.HasPrefix(result.Account.AuthID, "cursor-") || result.Account.Model != DefaultModel {
		t.Fatalf("account identity = %#v", result.Account)
	}
	info, err := os.Stat(result.Account.ProfileDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("profile mode = %v, want 0700", info.Mode().Perm())
	}
	if result.Tier != "pro" || result.Version != "2026.08.11" {
		t.Fatalf("about enrichment = %#v", result)
	}
	for _, name := range []string{".login-stdout", ".login-stderr"} {
		if _, errStat := os.Stat(filepath.Join(result.Account.ProfileDir, name)); errStat == nil {
			t.Fatalf("authenticated profile still holds the login capture %q", name)
		}
	}
}

func TestPollReapsDetachedLoginProcessGroupAfterSuccess(t *testing.T) {
	login := testLogin(t, "detached")
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := waitForLogin(t, login, start.State)
	if !result.Authenticated {
		t.Fatalf("login result = %#v", result)
	}
	rawPID, err := os.ReadFile(filepath.Join(result.Account.ProfileDir, "detached-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, syscall.Signal(0))
		if err == syscall.ESRCH {
			return
		}
		if err != nil {
			t.Fatalf("probe detached login process: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached login process %d survived a successful poll", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestPollRejectsFailedOrIncompleteAboutProbe(t *testing.T) {
	for _, mode := range []string{"fail", "empty"} {
		t.Run(mode, func(t *testing.T) {
			login := testLogin(t, "")
			login.ExtraEnv = append(login.ExtraEnv, "FAKE_AGENT_ABOUT="+mode)
			defer login.Close()
			start, err := login.Start(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result := waitForLogin(t, login, start.State)
			if result.Authenticated {
				t.Fatalf("incomplete about probe authenticated an account: %#v", result)
			}
			if result.Message != "the Cursor login could not be confirmed" {
				t.Fatalf("failure message = %q", result.Message)
			}
			assertNoProfilesLeft(t, login)
		})
	}
}

func TestAbandonedLoginIsSweptOnTheNextStart(t *testing.T) {
	login := testLogin(t, "slow")
	login.Expiry = 10 * time.Millisecond
	defer login.Close()
	abandoned, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := login.Paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("profiles after the first start = %d, want 1", len(entries))
	}
	time.Sleep(50 * time.Millisecond)
	if _, err = login.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("profiles after the second start = %d, want the abandoned one swept", len(entries))
	}
	if _, err = login.Poll(context.Background(), abandoned.State); FailureCode(err) != "unknown_login_state" {
		t.Fatalf("swept login state = %#v", err)
	}
}

func TestSecondLoginCreatesDistinctAccountAndProfile(t *testing.T) {
	login := testLogin(t, "")
	defer login.Close()
	first, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstResult := waitForLogin(t, login, first.State)
	second, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.State == first.State {
		t.Fatal("second login reused the first login state")
	}
	secondResult := waitForLogin(t, login, second.State)
	if firstResult.Account.AuthID == secondResult.Account.AuthID {
		t.Fatalf("both logins produced AuthID %q", firstResult.Account.AuthID)
	}
	if firstResult.Account.ProfileDir == secondResult.Account.ProfileDir {
		t.Fatalf("both logins shared profile %q", firstResult.Account.ProfileDir)
	}
}

func TestFailedLoginRemovesProfileAndRedactsChildOutput(t *testing.T) {
	login := testLogin(t, "fail")
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := waitForLogin(t, login, start.State)
	if result.Authenticated {
		t.Fatalf("failed login reported success: %#v", result)
	}
	if result.Message == "" || strings.Contains(result.Message, "sk-secret-value") {
		t.Fatalf("failure message leaked child output: %q", result.Message)
	}
	profiles, err := login.Paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed login left %d profile directories behind", len(entries))
	}
}

func TestLoginStartRefusesAmbiguousApprovalURLs(t *testing.T) {
	login := testLogin(t, "multi")
	defer login.Close()
	_, err := login.Start(context.Background())
	if FailureCode(err) != "approval_url_ambiguous" {
		t.Fatalf("ambiguous approval URL error = %#v", err)
	}
	assertNoProfilesLeft(t, login)
}

func TestLoginStartTimesOutWithoutApprovalURL(t *testing.T) {
	login := testLogin(t, "nourl")
	login.URLTimeout = 300 * time.Millisecond
	defer login.Close()
	_, err := login.Start(context.Background())
	if FailureCode(err) != "approval_url_unavailable" {
		t.Fatalf("missing approval URL error = %#v", err)
	}
	assertNoProfilesLeft(t, login)
}

func TestLoginStartReportsSetupRequiredWithoutExecutable(t *testing.T) {
	login := testLogin(t, "")
	login.Executable = func() (string, error) { return "", fmt.Errorf("not installed") }
	defer login.Close()
	_, err := login.Start(context.Background())
	if FailureCode(err) != CodeSetupRequired {
		t.Fatalf("missing executable error = %#v", err)
	}
}

func TestPollRefusesUnknownLoginState(t *testing.T) {
	login := testLogin(t, "")
	defer login.Close()
	if _, err := login.Poll(context.Background(), "not-a-state"); FailureCode(err) != "unknown_login_state" {
		t.Fatalf("unknown state error = %#v", err)
	}
}

func assertNoProfilesLeft(t *testing.T, login *Login) {
	t.Helper()
	profiles, err := login.Paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed login start left %d profile directories behind", len(entries))
	}
}

func TestLoginBoundsOnDiskCaptureWhilePending(t *testing.T) {
	login := testLogin(t, "flood")
	login.MaxCaptureBytes = 8 << 10
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		// The flood may outrun the URL scan; either refusal is acceptable, but
		// it must be the bound that fired and nothing may be left behind.
		if FailureCode(err) != "login_output_too_large" {
			t.Fatalf("flooding login start error = %#v", err)
		}
		assertNoProfilesLeft(t, login)
		return
	}
	result := waitForLogin(t, login, start.State)
	if result.Authenticated {
		t.Fatal("a login that flooded its capture files was accepted")
	}
	if !strings.Contains(result.Message, "output") {
		t.Fatalf("failure message = %q", result.Message)
	}
	assertNoProfilesLeft(t, login)
}

func TestLoginFindsAnApprovalURLPrintedAfterMuchOutput(t *testing.T) {
	login := testLogin(t, "chatty")
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("an approval URL printed after 64 KiB of output was missed: %v", err)
	}
	if !strings.HasPrefix(start.URL, "https://cursor.com/") {
		t.Fatalf("approval URL = %q", start.URL)
	}
	result := waitForLogin(t, login, start.State)
	if !result.Authenticated {
		t.Fatalf("login result = %#v", result)
	}
}

func TestLoginStartRefusesAFloodBeforeTheApprovalURL(t *testing.T) {
	login := testLogin(t, "floodnourl")
	login.MaxCaptureBytes = 8 << 10
	login.URLTimeout = 5 * time.Second
	defer login.Close()
	_, err := login.Start(context.Background())
	if FailureCode(err) != "login_output_too_large" {
		t.Fatalf("flooding login start error = %#v", err)
	}
	assertNoProfilesLeft(t, login)
}

func TestPollRefusesACompletedLoginPastItsExpiry(t *testing.T) {
	login := testLogin(t, "")
	login.Expiry = 150 * time.Millisecond
	defer login.Close()
	start, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The child has already finished successfully; expiry must still refuse it.
	time.Sleep(250 * time.Millisecond)
	result, err := login.Poll(context.Background(), start.State)
	if err != nil {
		t.Fatal(err)
	}
	if result.Authenticated || !strings.Contains(result.Message, "expired") {
		t.Fatalf("expired login poll = %#v", result)
	}
	assertNoProfilesLeft(t, login)
	if _, err = login.Poll(context.Background(), start.State); FailureCode(err) != "unknown_login_state" {
		t.Fatalf("expired login state was not deleted: %#v", err)
	}
}

func TestPollSweepsOtherAbandonedLogins(t *testing.T) {
	login := testLogin(t, "linger")
	login.Expiry = 150 * time.Millisecond
	defer login.Close()
	if _, err := login.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := login.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := login.Paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("profiles after two starts = %d, want 2", len(entries))
	}
	time.Sleep(250 * time.Millisecond)
	// Polling one state must also clean up the other abandoned attempt.
	if _, err = login.Poll(context.Background(), live.State); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("profiles after polling past expiry = %d, want every abandoned login swept", len(entries))
	}
}

func TestCloseCleansUpLoginsRacingWithIt(t *testing.T) {
	login := testLogin(t, "linger")
	profiles, err := login.Paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := 0; index < 6; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, errStart := login.Start(context.Background()); errStart != nil && FailureCode(errStart) != "login_unavailable" {
				t.Errorf("racing login start error = %#v", errStart)
			}
		}()
	}
	time.Sleep(60 * time.Millisecond)
	login.Close()
	group.Wait()
	// Close must have waited for every in-flight start, so nothing can appear
	// after it returned.
	entries, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Close left %d profile directories behind", len(entries))
	}
	if _, err = login.Start(context.Background()); FailureCode(err) != "login_unavailable" {
		t.Fatalf("login start after Close = %#v", err)
	}
	entries, err = os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("login start after Close left %d profile directories behind", len(entries))
	}
}
