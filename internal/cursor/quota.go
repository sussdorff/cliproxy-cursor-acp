package cursor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxUsageSummaryBytes = 64 << 10

var cursorSessionValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Quota is the safe, account-scoped subscription data published to the host.
// Credential material is deliberately absent from this value.
//
// Used/Limit/Remaining describe the included plan total (Cursor reports those
// counts in cents on token-priced plans). Windows holds the display breakdown:
// Cursor (auto) as the main allowance, Third Party (API), and optionally Grok Bot.
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
	Windows        []QuotaWindow
	Spend          *QuotaSpend
	Daily          []QuotaDaily
}

// QuotaSpend is the CodexBar dashboard cost summary over the recent period.
// Amounts are USD cents. Token counts are summed from usage events.
type QuotaSpend struct {
	MeteredCents int64
	HasMetered   bool
	TodayCents   int64
	HasToday     bool
	PeriodCents  int64
	HasPeriod    bool
	LatestTokens int64
	HasLatest    bool
	PeriodTokens int64
	HasPeriodTok bool
	PeriodDays   int
}

// QuotaDaily is one UTC day of vendor-list cost for the histogram.
type QuotaDaily struct {
	Date      string
	CostCents int64
	Tokens    int64
}

// QuotaWindow is one displayable allowance the contract can publish.
type QuotaWindow struct {
	ID             string
	Label          string
	Kind           string
	Unit           string
	Used           int64
	Limit          int64
	Remaining      int64
	HasCounts      bool
	UsedPercent    float64
	HasUsedPercent bool
	Unlimited      bool
	WindowStart    string
	WindowEnd      string
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
	if sand, ok := c.fetchSandUsage(ctx, request.Header.Get("Cookie")); ok {
		quota.Windows = append(quota.Windows, sand)
	}
	cookie := request.Header.Get("Cookie")
	if spend, daily, ok := c.fetchUsageSpend(ctx, cookie); ok {
		quota.Spend = spend
		quota.Daily = daily
	}
	return quota, nil
}

func (c *UsageSummaryClient) fetchSandUsage(ctx context.Context, cookie string) (QuotaWindow, bool) {
	if strings.TrimSpace(cookie) == "" {
		return QuotaWindow{}, false
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/api/dashboard/get-sand-usage-status", bytes.NewReader([]byte("{}")),
	)
	if err != nil {
		return QuotaWindow{}, false
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", requestOrigin(c.baseURL))
	request.Header.Set("Cookie", cookie)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return QuotaWindow{}, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return QuotaWindow{}, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUsageSummaryBytes+1))
	if err != nil || len(body) > maxUsageSummaryBytes {
		return QuotaWindow{}, false
	}
	return parseSandUsage(body)
}

func requestOrigin(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://cursor.com"
	}
	return parsed.Scheme + "://" + parsed.Host
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

type usagePercent struct {
	value float64
	set   bool
}

func (n *usagePercent) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return nil
	}
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	parsed, err := value.Float64()
	if err != nil {
		return err
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return nil
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
				Enabled          bool         `json:"enabled"`
				Used             usageNumber  `json:"used"`
				Limit            usageNumber  `json:"limit"`
				Remaining        usageNumber  `json:"remaining"`
				AutoPercentUsed  usagePercent `json:"autoPercentUsed"`
				APIPercentUsed   usagePercent `json:"apiPercentUsed"`
				TotalPercentUsed usagePercent `json:"totalPercentUsed"`
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
	kind := billingKind(summary.LimitType)
	quota := Quota{
		Available: true, WindowStart: summary.BillingCycleStart, WindowEnd: summary.BillingCycleEnd,
		MembershipType: summary.MembershipType, LimitType: summary.LimitType, Unlimited: summary.IsUnlimited,
		Used: plan.Used.value, Limit: plan.Limit.value, Remaining: plan.Remaining.value,
	}
	if percent, ok := optionalPercent(plan.AutoPercentUsed); ok {
		quota.Windows = append(quota.Windows, QuotaWindow{
			ID: "cursor", Label: "Cursor", Kind: kind, HasUsedPercent: true, UsedPercent: percent,
			WindowStart: summary.BillingCycleStart, WindowEnd: summary.BillingCycleEnd,
		})
	}
	if percent, ok := optionalPercent(plan.APIPercentUsed); ok {
		quota.Windows = append(quota.Windows, QuotaWindow{
			ID: "third_party", Label: "Third Party", Kind: kind, HasUsedPercent: true, UsedPercent: percent,
		})
	}
	return quota, true
}

func parseSandUsage(body []byte) (QuotaWindow, bool) {
	var status struct {
		UsagePercent            usagePercent `json:"usagePercent"`
		NextResetTimestampUTC   string       `json:"nextResetTimestampUtc"`
		CurrentPeriodStart      string       `json:"currentPeriodStart"`
		HasNonZeroIncludedLimit bool         `json:"hasNonZeroIncludedLimit"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&status); err != nil {
		return QuotaWindow{}, false
	}
	if !status.HasNonZeroIncludedLimit {
		return QuotaWindow{}, false
	}
	percent, ok := optionalPercent(status.UsagePercent)
	if !ok {
		return QuotaWindow{}, false
	}
	if strings.TrimSpace(status.NextResetTimestampUTC) == "" {
		return QuotaWindow{}, false
	}
	return QuotaWindow{
		ID: "grok_bot", Label: "Grok Bot", Kind: "product",
		HasUsedPercent: true, UsedPercent: percent,
	}, true
}

func optionalPercent(value usagePercent) (float64, bool) {
	if !value.set {
		return 0, false
	}
	return clampPercent(value.value)
}

func clampPercent(value float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	rounded := math.Round(math.Min(value, 100)*100) / 100
	return rounded, true
}

func billingKind(limitType string) string {
	switch strings.ToLower(strings.TrimSpace(limitType)) {
	case "monthly", "month":
		return "monthly"
	case "weekly", "week":
		return "weekly"
	case "daily", "day":
		return "daily"
	default:
		return "monthly"
	}
}
