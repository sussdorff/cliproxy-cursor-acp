package plugin

import (
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

// The generic plugin quota contract lets any CLIProxyAPI plugin publish
// normalized quota windows through auth metadata, so a manager UI can render
// them without a provider-specific adapter. It carries consumption only: no
// tokens, cookies, profile paths, or raw upstream response bodies.
const (
	// PluginQuotaMetadataKey is the single auth-metadata key that carries the
	// contract. Consumers must read the payload only through this key.
	PluginQuotaMetadataKey = "plugin_quota"
	// PluginQuotaSchema identifies the payload independently of the provider.
	PluginQuotaSchema = "cliproxy.plugin.quota"
	// PluginQuotaVersion is incremented only for an incompatible field change.
	// Consumers must ignore a payload whose version they do not implement.
	PluginQuotaVersion = 1
	// pluginQuotaTTLSeconds is how long an observation stays displayable. A
	// consumer that sees an older observation must report it as stale rather
	// than presenting a value the provider no longer stands behind.
	pluginQuotaTTLSeconds = 21600
	// maxDisplayTextBytes bounds every free-text field the contract carries.
	maxDisplayTextBytes = 128
)

// Availability separates "the provider could not observe quota" from "the
// credential is unusable". Only the credential's own status controls the
// latter, so an unobservable quota never removes an account from rotation.
const (
	pluginQuotaAvailable   = "available"
	pluginQuotaUnavailable = "unavailable"
)

// unsafeDisplayText strips control characters from provider-supplied labels so
// a manager UI never renders upstream text verbatim.
var unsafeDisplayText = regexp.MustCompile(`[[:cntrl:]]+`)

// pluginQuotaContract is the versioned, provider-neutral payload.
type pluginQuotaContract struct {
	Schema       string              `json:"schema"`
	Version      int                 `json:"version"`
	Provider     string              `json:"provider"`
	Availability string              `json:"availability"`
	ObservedAt   string              `json:"observed_at,omitempty"`
	TTLSeconds   int                 `json:"ttl_seconds"`
	Windows      []pluginQuotaWindow `json:"windows"`
	Spend        *pluginQuotaSpend   `json:"spend,omitempty"`
}

// pluginQuotaSpend is the optional CodexBar-style cost summary. All money
// fields are USD cents so the host can copy them as numbers.
type pluginQuotaSpend struct {
	Currency     string `json:"currency"`
	MeteredCents *int64 `json:"metered_cents,omitempty"`
	TodayCents   *int64 `json:"today_cents,omitempty"`
	PeriodCents  *int64 `json:"period_cents,omitempty"`
	LatestTokens *int64 `json:"latest_tokens,omitempty"`
	PeriodTokens *int64 `json:"period_tokens,omitempty"`
	PeriodDays   int    `json:"period_days,omitempty"`
}

// pluginQuotaWindow is one normalized quota window. Identity is `id`; every
// other field is presentation or measurement and may be absent.
type pluginQuotaWindow struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Kind          string   `json:"kind"`
	Unit          string   `json:"unit,omitempty"`
	Used          *int64   `json:"used,omitempty"`
	Limit         *int64   `json:"limit,omitempty"`
	Remaining     *int64   `json:"remaining,omitempty"`
	UsedPercent   *float64 `json:"used_percent,omitempty"`
	Unlimited     bool     `json:"unlimited"`
	WindowStart   string   `json:"window_start,omitempty"`
	WindowEnd     string   `json:"window_end,omitempty"`
	ResetAt       string   `json:"reset_at,omitempty"`
	ResetAccuracy string   `json:"reset_accuracy"`
}

// buildPluginQuotaContract turns one already-safe account observation into the
// published contract. An absent or failed observation still produces a
// complete, identifiable payload so consumers can distinguish "no contract"
// from "contract reporting no data".
func buildPluginQuotaContract(providerID string, metadata cursor.Metadata) pluginQuotaContract {
	contract := pluginQuotaContract{
		Schema:       PluginQuotaSchema,
		Version:      PluginQuotaVersion,
		Provider:     providerID,
		Availability: pluginQuotaUnavailable,
		TTLSeconds:   pluginQuotaTTLSeconds,
		Windows:      []pluginQuotaWindow{},
	}
	if !metadata.SubscriptionQuotaAvailable || !metadata.Quota.Available {
		return contract
	}
	windows := contractWindows(metadata.Quota)
	if len(windows) == 0 {
		return contract
	}
	contract.Availability = pluginQuotaAvailable
	contract.ObservedAt = observationTime(metadata.ObservedAt)
	contract.Windows = windows
	if spend, ok := contractSpend(metadata.Quota.Spend); ok {
		contract.Spend = spend
	}
	return contract
}

func contractSpend(spend *cursor.QuotaSpend) (*pluginQuotaSpend, bool) {
	if spend == nil {
		return nil, false
	}
	out := &pluginQuotaSpend{Currency: "USD", PeriodDays: spend.PeriodDays}
	if spend.HasMetered && spend.MeteredCents >= 0 {
		cents := spend.MeteredCents
		out.MeteredCents = &cents
	}
	if spend.HasToday && spend.TodayCents >= 0 {
		cents := spend.TodayCents
		out.TodayCents = &cents
	}
	if spend.HasPeriod && spend.PeriodCents >= 0 {
		cents := spend.PeriodCents
		out.PeriodCents = &cents
	}
	if spend.HasLatest && spend.LatestTokens >= 0 {
		tokens := spend.LatestTokens
		out.LatestTokens = &tokens
	}
	if spend.HasPeriodTok && spend.PeriodTokens >= 0 {
		tokens := spend.PeriodTokens
		out.PeriodTokens = &tokens
	}
	if out.MeteredCents == nil && out.TodayCents == nil && out.PeriodCents == nil &&
		out.LatestTokens == nil && out.PeriodTokens == nil {
		return nil, false
	}
	return out, true
}

// contractWindows maps the observed Cursor allowances. Cursor is the main
// interval window; satellite allowances (Third Party, Grok Bot) are published
// without boundaries so a generic UI can render them as other quota items.
// The included-plan Total window and daily histogram are not published.
func contractWindows(quota cursor.Quota) []pluginQuotaWindow {
	windows := make([]pluginQuotaWindow, 0, len(quota.Windows))
	for _, observed := range quota.Windows {
		if displayText(observed.ID) == "total" {
			continue
		}
		window, ok := observedWindow(observed)
		if !ok {
			continue
		}
		if !isPrimaryQuotaWindow(window.ID) {
			window.WindowStart = ""
			window.WindowEnd = ""
			window.ResetAt = ""
			window.ResetAccuracy = "unknown"
		}
		windows = append(windows, window)
	}
	if !hasPrimaryQuotaWindow(windows) {
		if window, ok := subscriptionWindow(quota); ok {
			windows = append([]pluginQuotaWindow{window}, windows...)
		}
	}
	if len(windows) == 0 {
		return nil
	}
	return windows
}

func isPrimaryQuotaWindow(id string) bool {
	return id == "cursor" || id == "subscription"
}

func hasPrimaryQuotaWindow(windows []pluginQuotaWindow) bool {
	for _, window := range windows {
		if isPrimaryQuotaWindow(window.ID) {
			return true
		}
	}
	return false
}

func observedWindow(observed cursor.QuotaWindow) (pluginQuotaWindow, bool) {
	id := displayText(observed.ID)
	label := displayText(observed.Label)
	if id == "" || label == "" {
		return pluginQuotaWindow{}, false
	}
	if observed.HasCounts && (observed.Used < 0 || observed.Limit < 0 || observed.Remaining < 0) {
		return pluginQuotaWindow{}, false
	}
	windowStart := contractTimestamp(observed.WindowStart)
	windowEnd := contractTimestamp(observed.WindowEnd)
	kind := displayText(observed.Kind)
	if kind == "" {
		kind = "billing"
	}
	window := pluginQuotaWindow{
		ID:            id,
		Label:         label,
		Kind:          kind,
		Unit:          displayText(observed.Unit),
		Unlimited:     observed.Unlimited,
		WindowStart:   windowStart,
		WindowEnd:     windowEnd,
		ResetAt:       windowEnd,
		ResetAccuracy: "unknown",
	}
	if windowEnd != "" {
		window.ResetAccuracy = "exact"
	}
	if observed.HasCounts {
		used, limit, remaining := observed.Used, observed.Limit, observed.Remaining
		window.Used, window.Limit, window.Remaining = &used, &limit, &remaining
	}
	if observed.HasUsedPercent && !observed.Unlimited {
		percent := observed.UsedPercent
		window.UsedPercent = &percent
	}
	if window.UsedPercent == nil && !observed.Unlimited && observed.HasCounts && observed.Limit > 0 && observed.Used >= 0 {
		if percent, ok := usedPercent(cursor.Quota{Used: observed.Used, Limit: observed.Limit}); ok {
			window.UsedPercent = &percent
		}
	}
	if window.UsedPercent == nil && !observed.Unlimited && windowStart == "" && windowEnd == "" {
		return pluginQuotaWindow{}, false
	}
	return window, true
}

// subscriptionWindow maps the Cursor billing-cycle observation onto the single
// generic window this provider can describe.
func subscriptionWindow(quota cursor.Quota) (pluginQuotaWindow, bool) {
	if quota.Used < 0 || quota.Limit < 0 || quota.Remaining < 0 {
		return pluginQuotaWindow{}, false
	}
	limitType := displayText(quota.LimitType)
	windowStart := contractTimestamp(quota.WindowStart)
	windowEnd := contractTimestamp(quota.WindowEnd)
	if windowStart == "" && windowEnd == "" {
		return pluginQuotaWindow{}, false
	}
	used, limit, remaining := quota.Used, quota.Limit, quota.Remaining
	window := pluginQuotaWindow{
		ID:            "subscription",
		Label:         subscriptionLabel(limitType),
		Kind:          subscriptionKind(limitType),
		Unit:          "requests",
		Used:          &used,
		Limit:         &limit,
		Remaining:     &remaining,
		Unlimited:     quota.Unlimited,
		WindowStart:   windowStart,
		WindowEnd:     windowEnd,
		ResetAt:       windowEnd,
		ResetAccuracy: "unknown",
	}
	if windowEnd != "" {
		window.ResetAccuracy = "exact"
	}
	if percent, ok := usedPercent(quota); ok {
		window.UsedPercent = &percent
	}
	return window, true
}

// usedPercent is reported only when the provider states a finite allowance.
// An unlimited plan has no meaningful utilization percentage.
func usedPercent(quota cursor.Quota) (float64, bool) {
	if quota.Unlimited || quota.Limit <= 0 || quota.Used < 0 {
		return 0, false
	}
	percent := float64(quota.Used) / float64(quota.Limit) * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, false
	}
	return math.Round(math.Min(percent, 100)*100) / 100, true
}

func subscriptionKind(limitType string) string {
	switch strings.ToLower(limitType) {
	case "monthly", "month":
		return "monthly"
	case "weekly", "week":
		return "weekly"
	case "daily", "day":
		return "daily"
	default:
		return "billing"
	}
}

func subscriptionLabel(limitType string) string {
	if limitType == "" {
		return "Subscription usage"
	}
	return strings.ToUpper(limitType[:1]) + strings.ToLower(limitType[1:]) + " usage"
}

// contractTimestamp normalizes an upstream boundary to RFC3339 UTC and drops
// anything it cannot parse, so consumers never receive a free-text timestamp.
func contractTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

func observationTime(value string) string {
	if normalized := contractTimestamp(value); normalized != "" {
		return normalized
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// displayText bounds and sanitizes one provider-supplied string.
func displayText(value string) string {
	cleaned := strings.TrimSpace(unsafeDisplayText.ReplaceAllString(value, " "))
	if len(cleaned) > maxDisplayTextBytes {
		cleaned = strings.TrimSpace(cleaned[:maxDisplayTextBytes])
	}
	return cleaned
}
