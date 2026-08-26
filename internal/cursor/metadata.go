package cursor

import (
	"context"
	"time"
)

// Metadata is safe for CLIProxyAPI generic auth/model management surfaces.
// It carries the account-scoped subscription observation, never the credential
// material the official Cursor CLI keeps inside the managed profile.
type Metadata struct {
	AuthID                     string `json:"auth_id"`
	Label                      string `json:"label,omitempty"`
	Authenticated              bool   `json:"authenticated"`
	Status                     string `json:"status"`
	Model                      string `json:"model,omitempty"`
	ObservedInputTokens        int64  `json:"observed_input_tokens"`
	ObservedOutputTokens       int64  `json:"observed_output_tokens"`
	ExactSubscriptionQuota     *int64 `json:"exact_subscription_quota,omitempty"`
	SubscriptionQuotaAvailable bool   `json:"subscription_quota_available"`
	Quota                      Quota  `json:"quota"`
	ObservedAt                 string `json:"observed_at,omitempty"`
	UpdatedAt                  string `json:"updated_at"`
}

func (s *Service) Metadata(ctx context.Context, authID string) (Metadata, error) {
	account, metadata, err := s.AccountWithMetadata(authID)
	if err != nil {
		return Metadata{}, err
	}
	return s.MetadataForAccount(ctx, account, metadata), nil
}

// AccountWithMetadata returns an account and its management metadata from one
// runtime snapshot. Callers that perform work outside the service lock can use
// it to verify that the account did not change before serializing a response.
func (s *Service) AccountWithMetadata(authID string) (Account, Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.accounts[authID]
	if runtime == nil {
		return Account{}, Metadata{}, fatal("unknown_auth", ErrUnknownAuth)
	}
	return runtime.account, Metadata{
		AuthID: authID, Label: runtime.account.Label, Authenticated: true,
		Status: "available", Model: runtime.account.Model,
		ObservedInputTokens: runtime.inputTokens, ObservedOutputTokens: runtime.outputTokens,
		ExactSubscriptionQuota: nil, SubscriptionQuotaAvailable: false,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// MetadataForAccount adds the best-effort quota view to a single already-read
// account snapshot. The account itself remains the caller's source of truth;
// a quota read that races a re-login is simply unavailable.
func (s *Service) MetadataForAccount(ctx context.Context, account Account, metadata Metadata) Metadata {
	s.mu.Lock()
	provider := s.quota
	s.mu.Unlock()
	if provider == nil {
		return metadata
	}
	profilesRoot, err := s.paths.ProfilesRoot()
	if err != nil {
		return metadata
	}
	quota, err := provider.Fetch(ctx, QuotaTarget{ProfileDir: account.ProfileDir, ProfilesRoot: profilesRoot})
	if err != nil || !quota.Available {
		return metadata
	}
	remaining := quota.Remaining
	metadata.Quota = quota
	metadata.ExactSubscriptionQuota = &remaining
	metadata.SubscriptionQuotaAvailable = true
	metadata.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	return metadata
}
