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
	pluginQuotaTTLSeconds = 900
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
	window, ok := subscriptionWindow(metadata.Quota)
	if !ok {
		return contract
	}
	contract.Availability = pluginQuotaAvailable
	contract.ObservedAt = observationTime(metadata.ObservedAt)
	contract.Windows = []pluginQuotaWindow{window}
	return contract
}

// subscriptionWindow maps the Cursor billing-cycle observation onto the single
// generic window this provider can describe.
func subscriptionWindow(quota cursor.Quota) (pluginQuotaWindow, bool) {
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
