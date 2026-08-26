package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestPinnedAgentDigestsMatchUpstream re-downloads every pinned Cursor Agent
// artifact and compares it with the digest embedded in install.go. It is the
// only test in this repository that touches the network, so it is skipped
// unless CCA_VERIFY_UPSTREAM_DIGESTS=1 is set. The release workflow sets it, so
// a stale or wrong pin fails the release instead of reaching operators.
func TestPinnedAgentDigestsMatchUpstream(t *testing.T) {
	if os.Getenv("CCA_VERIFY_UPSTREAM_DIGESTS") != "1" {
		t.Skip("set CCA_VERIFY_UPSTREAM_DIGESTS=1 to verify the embedded pins against downloads.cursor.com")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	for platform, expected := range pinnedAgentDigests {
		t.Run(platform, func(t *testing.T) {
			artifactURL := "https://downloads.cursor.com/lab/" + pinnedAgentVersion + "/" + platform + "/agent-cli-package.tar.gz"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s returned HTTP %d", artifactURL, response.StatusCode)
			}
			digest := sha256.New()
			if _, err = io.Copy(digest, response.Body); err != nil {
				t.Fatal(err)
			}
			if actual := hex.EncodeToString(digest.Sum(nil)); actual != expected {
				t.Fatalf("%s\n  embedded pin: %s\n  upstream:     %s\nbump pinnedAgentVersion and pinnedAgentDigests together", artifactURL, expected, actual)
			}
		})
	}
}
