package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxUsageSummaryBytes = 64 << 10

var cursorSessionValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Quota is the safe, account-scoped subscription data published to the host.
// Credential material is deliberately absent from this value.
type Quota struct {
	Available      bool
	WindowStart    string
	WindowEnd      string
	MembershipType string
	LimitType      string
	Unlimited      bool
	Used           int64
	Limit          int64
	Remaining      int64
}

// QuotaProvider fetches subscription data for exactly one registered account.
type QuotaProvider interface {
	Fetch(context.Context, QuotaTarget) (Quota, error)
}

// QuotaTarget is the already-selected account profile and its managed parent.
// Keeping both paths in this narrow value lets the quota reader revalidate
// containment without trusting a profile path from persisted auth storage.
type QuotaTarget struct {
	ProfileDir   string
	ProfilesRoot string
}

// UsageSummaryClient reads only the official Cursor CLI profile's access token
// and sends an in-memory derived cookie to Cursor's exact production origin.
type UsageSummaryClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewUsageSummaryClient constructs the production-only usage-summary client.
func NewUsageSummaryClient() *UsageSummaryClient {
	return newUsageSummaryClient("https://cursor.com", nil)
}

func newUsageSummaryClient(baseURL string, client *http.Client) *UsageSummaryClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	configured := *client
	configured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &UsageSummaryClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &configured}
}

func (c *UsageSummaryClient) Fetch(ctx context.Context, target QuotaTarget) (Quota, error) {
	token, err := readProfileAccessToken(target)
	if err != nil {
		return Quota{}, quotaUnavailable()
	}
	userID, err := cursorUserID(token)
	if err != nil {
		return Quota{}, quotaUnavailable()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/usage-summary", nil)
	if err != nil {
		return Quota{}, quotaUnavailable()
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cookie", "WorkosCursorSessionToken="+userID+"%3A%3A"+token)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Quota{}, quotaUnavailable()
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Quota{}, quotaUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUsageSummaryBytes+1))
	if err != nil || len(body) > maxUsageSummaryBytes {
		return Quota{}, quotaUnavailable()
	}
	quota, ok := parseUsageSummary(body)
	if !ok {
		return Quota{}, quotaUnavailable()
	}
	return quota, nil
}

type quotaUnavailableError struct{}

func (quotaUnavailableError) Error() string { return "Cursor quota is unavailable" }

func quotaUnavailable() error { return quotaUnavailableError{} }

func readProfileAccessToken(target QuotaTarget) (string, error) {
	file, err := openQuotaCredential(target)
	if err != nil {
		return "", quotaUnavailable()
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxUsageSummaryBytes+1))
	if err != nil || len(body) > maxUsageSummaryBytes {
		return "", quotaUnavailable()
	}
	var auth struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &auth); err != nil {
		return "", err
	}
	token := strings.TrimSpace(auth.AccessToken)
	if token == "" || len(token) > 16<<10 || !cursorSessionValue.MatchString(token) {
		return "", quotaUnavailable()
	}
	return token, nil
}

func cursorUserID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", quotaUnavailable()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", quotaUnavailable()
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", quotaUnavailable()
	}
	userID := strings.TrimSpace(claims.Subject)
	if index := strings.LastIndex(userID, "|"); index >= 0 {
		userID = userID[index+1:]
	}
	if userID == "" || !cursorSessionValue.MatchString(userID) {
		return "", quotaUnavailable()
	}
	return userID, nil
}

type usageNumber struct {
	value int64
	set   bool
}

func (n *usageNumber) UnmarshalJSON(raw []byte) error {
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	parsed, err := value.Int64()
	if err != nil {
		return err
	}
	n.value, n.set = parsed, true
	return nil
}

func parseUsageSummary(body []byte) (Quota, bool) {
	var summary struct {
		BillingCycleStart string `json:"billingCycleStart"`
		BillingCycleEnd   string `json:"billingCycleEnd"`
		MembershipType    string `json:"membershipType"`
		LimitType         string `json:"limitType"`
		IsUnlimited       bool   `json:"isUnlimited"`
		IndividualUsage   struct {
			Plan *struct {
				Enabled   bool        `json:"enabled"`
				Used      usageNumber `json:"used"`
				Limit     usageNumber `json:"limit"`
				Remaining usageNumber `json:"remaining"`
			} `json:"plan"`
		} `json:"individualUsage"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&summary); err != nil || summary.IndividualUsage.Plan == nil {
		return Quota{}, false
	}
	plan := summary.IndividualUsage.Plan
	if !plan.Enabled || !plan.Used.set || !plan.Limit.set || !plan.Remaining.set ||
		plan.Used.value < 0 || plan.Limit.value < 0 || plan.Remaining.value < 0 ||
		strings.TrimSpace(summary.BillingCycleStart) == "" || strings.TrimSpace(summary.BillingCycleEnd) == "" {
		return Quota{}, false
	}
	return Quota{
		Available: true, WindowStart: summary.BillingCycleStart, WindowEnd: summary.BillingCycleEnd,
		MembershipType: summary.MembershipType, LimitType: summary.LimitType, Unlimited: summary.IsUnlimited,
		Used: plan.Used.value, Limit: plan.Limit.value, Remaining: plan.Remaining.value,
	}, true
}
