//go:build !darwin && !linux

package cursor

import "os"

// Cursor ACP releases support Linux. Other platforms deliberately report quota
// as unavailable instead of falling back to pathname-based credential reads.
func openQuotaCredential(QuotaTarget) (*os.File, error) {
	return nil, quotaUnavailable()
}
