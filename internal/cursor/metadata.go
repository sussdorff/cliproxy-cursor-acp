package cursor

import "time"

// Metadata is safe for CLIProxyAPI generic auth/model management surfaces.
// Exact subscription quota is deliberately unavailable because ACP context use
// is not a Cursor subscription balance.
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
	UpdatedAt                  string `json:"updated_at"`
}

func (s *Service) Metadata(authID string) (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.accounts[authID]
	if runtime == nil {
		return Metadata{}, fatal("unknown_auth", ErrUnknownAuth)
	}
	return Metadata{
		AuthID: authID, Label: runtime.account.Label, Authenticated: true,
		Status: "available", Model: runtime.account.Model,
		ObservedInputTokens: runtime.inputTokens, ObservedOutputTokens: runtime.outputTokens,
		ExactSubscriptionQuota: nil, SubscriptionQuotaAvailable: false,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}
