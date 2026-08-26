package cursor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAccessToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ3b3Jrb3N8Y3Vyc29yLXVzZXIiLCJleHAiOjQxMDI0NDQ4MDB9.signature"

func writeProfileToken(t *testing.T, profile string) {
	t.Helper()
	if err := os.Chmod(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(profile, "cursor")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(`{"accessToken":"`+testAccessToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newQuotaProfile(t *testing.T) QuotaTarget {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(root, "profiles")
	profile := filepath.Join(profiles, "account")
	for _, directory := range []string{profiles, profile} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return QuotaTarget{ProfileDir: profile, ProfilesRoot: profiles}
}

func TestUsageSummaryMapsCurrentBillingWindowWithoutLeakingCredentials(t *testing.T) {
	target := newQuotaProfile(t)
	writeProfileToken(t, target.ProfileDir)
	if token, err := readProfileAccessToken(target); err != nil || token != testAccessToken {
		t.Fatalf("profile token was not accepted: %v", err)
	}
	if userID, err := cursorUserID(testAccessToken); err != nil || userID != "cursor-user" {
		t.Fatalf("profile identity was not accepted: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/usage-summary" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if got, want := request.Header.Get("Cookie"), "WorkosCursorSessionToken=cursor-user%3A%3A"+testAccessToken; got != want {
			t.Fatalf("cookie was not derived from this profile: %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"billingCycleStart":"2026-08-01T00:00:00.000Z",
			"billingCycleEnd":"2026-09-01T00:00:00.000Z",
			"membershipType":"pro",
			"limitType":"monthly",
			"individualUsage":{"plan":{"enabled":true,"used":125,"limit":500,"remaining":375}}
		}`))
	}))
	defer server.Close()

	quota, err := newUsageSummaryClient(server.URL, server.Client()).Fetch(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !quota.Available || quota.Used != 125 || quota.Limit != 500 || quota.Remaining != 375 {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.WindowStart != "2026-08-01T00:00:00.000Z" || quota.WindowEnd != "2026-09-01T00:00:00.000Z" || quota.MembershipType != "pro" {
		t.Fatalf("quota window = %#v", quota)
	}
}

func TestParseUsageSummaryRejectsNegativeCounts(t *testing.T) {
	for name, counts := range map[string]string{
		"used":      `"used":-1,"limit":500,"remaining":375`,
		"limit":     `"used":125,"limit":-1,"remaining":375`,
		"remaining": `"used":125,"limit":500,"remaining":-1`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{
				"billingCycleStart":"2026-08-01T00:00:00.000Z",
				"billingCycleEnd":"2026-09-01T00:00:00.000Z",
				"individualUsage":{"plan":{"enabled":true,` + counts + `}}
			}`)
			if _, ok := parseUsageSummary(body); ok {
				t.Fatal("negative subscription count produced an available quota")
			}
		})
	}
}

func TestUsageSummaryUnavailableFailuresNeverExposeCredentials(t *testing.T) {
	cases := []struct {
		name       string
		writeToken bool
		response   string
		status     int
		redirect   bool
		oversized  bool
	}{
		{name: "missing credential"},
		{name: "malformed response", writeToken: true, response: "{"},
		{name: "non-success response", writeToken: true, status: http.StatusUnauthorized, response: `{"error":"expired"}`},
		{name: "redirect", writeToken: true, redirect: true},
		{name: "oversized response", writeToken: true, oversized: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			target := newQuotaProfile(t)
			if testCase.writeToken {
				writeProfileToken(t, target.ProfileDir)
			}
			redirectHit := false
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/redirect-target" {
					redirectHit = true
					if request.Header.Get("Cookie") != "" {
						t.Fatal("credentialed redirect was followed")
					}
					return
				}
				if testCase.redirect {
					http.Redirect(writer, request, "/redirect-target", http.StatusFound)
					return
				}
				if testCase.oversized {
					_, _ = writer.Write([]byte(strings.Repeat("x", maxUsageSummaryBytes+1)))
					return
				}
				if testCase.status != 0 {
					writer.WriteHeader(testCase.status)
				}
				_, _ = writer.Write([]byte(testCase.response))
			}))
			defer server.Close()

			_, err := newUsageSummaryClient(server.URL, server.Client()).Fetch(context.Background(), target)
			if err == nil {
				t.Fatal("quota refresh unexpectedly succeeded")
			}
			if redirectHit {
				t.Fatal("redirect target was requested")
			}
			for _, forbidden := range []string{testAccessToken, "cursor-user", target.ProfileDir, "expired"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("quota error leaks %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestUsageSummaryRejectsAProfileReplacedBySymlinkBeforeRequest(t *testing.T) {
	target := newQuotaProfile(t)
	writeProfileToken(t, target.ProfileDir)
	foreign := t.TempDir()
	writeProfileToken(t, foreign)
	if err := os.RemoveAll(target.ProfileDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, target.ProfileDir); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	_, err := newUsageSummaryClient(server.URL, server.Client()).Fetch(context.Background(), target)
	if err == nil {
		t.Fatal("symlinked profile quota refresh unexpectedly succeeded")
	}
	if requests != 0 {
		t.Fatalf("quota client sent a cookie after profile substitution: %d requests", requests)
	}
}

func TestUsageSummaryRejectsCredentialAndDirectorySymlinksBeforeRequest(t *testing.T) {
	for _, testCase := range []struct {
		name string
		swap func(*testing.T, QuotaTarget)
	}{
		{
			name: "final credential", swap: func(t *testing.T, target QuotaTarget) {
				t.Helper()
				foreign := filepath.Join(t.TempDir(), "foreign-auth.json")
				if err := os.WriteFile(foreign, []byte(`{"accessToken":"foreign"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				credential := filepath.Join(target.ProfileDir, "cursor", "auth.json")
				if err := os.Remove(credential); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, credential); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cursor directory", swap: func(t *testing.T, target QuotaTarget) {
				t.Helper()
				foreign := t.TempDir()
				writeProfileToken(t, foreign)
				directory := filepath.Join(target.ProfileDir, "cursor")
				if err := os.RemoveAll(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(foreign, "cursor"), directory); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newQuotaProfile(t)
			writeProfileToken(t, target.ProfileDir)
			testCase.swap(t, target)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			if _, err := newUsageSummaryClient(server.URL, server.Client()).Fetch(context.Background(), target); err == nil {
				t.Fatal("symlinked credential component quota refresh unexpectedly succeeded")
			}
			if requests != 0 {
				t.Fatalf("quota client sent a request through a symlinked component: %d", requests)
			}
		})
	}
}

func TestOpenedQuotaCredentialRemainsTheValidatedFileAfterAPathSwap(t *testing.T) {
	target := newQuotaProfile(t)
	writeProfileToken(t, target.ProfileDir)
	credential, err := openQuotaCredential(target)
	if err != nil {
		t.Fatal(err)
	}
	defer credential.Close()
	foreign := filepath.Join(t.TempDir(), "foreign-auth.json")
	if err := os.WriteFile(foreign, []byte(`{"accessToken":"foreign"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.ProfileDir, "cursor", "auth.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, path); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(credential)
	if err != nil || !strings.Contains(string(body), testAccessToken) || strings.Contains(string(body), "foreign") {
		t.Fatalf("opened credential followed a later path swap: %v", err)
	}
}

func TestUsageSummaryProductionOriginIsPinned(t *testing.T) {
	client := NewUsageSummaryClient()
	if got, want := client.baseURL, "https://cursor.com"; got != want {
		t.Fatalf("base URL = %q, want %q", got, want)
	}
	if client.httpClient.CheckRedirect == nil {
		t.Fatal("production client follows redirects")
	}
	if err := client.httpClient.CheckRedirect(nil, nil); err == nil {
		t.Fatal("production client permits redirects")
	}
}
