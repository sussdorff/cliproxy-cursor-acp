package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	usageEventsPath     = "/api/dashboard/get-filtered-usage-events"
	usageEventsPageSize = 100
	usageEventsMaxPages = 20
	usageEventsPeriod   = 30 * 24 * time.Hour
	maxUsageEventsBytes = 2 << 20
)

func (c *UsageSummaryClient) fetchUsageSpend(ctx context.Context, cookie string) (*QuotaSpend, []QuotaDaily, bool) {
	if strings.TrimSpace(cookie) == "" {
		return nil, nil, false
	}
	until := time.Now().UTC()
	since := until.Add(-usageEventsPeriod)
	var events []usageEvent
	for page := 1; page <= usageEventsMaxPages; page++ {
		pageEvents, total, ok := c.fetchUsageEventsPage(ctx, cookie, page, since, until)
		if !ok {
			if page == 1 {
				return nil, nil, false
			}
			break
		}
		events = append(events, pageEvents...)
		if len(pageEvents) < usageEventsPageSize || (total > 0 && len(events) >= total) {
			break
		}
	}
	if len(events) == 0 {
		return nil, nil, false
	}
	return aggregateUsageEvents(events, until)
}

func (c *UsageSummaryClient) fetchUsageEventsPage(
	ctx context.Context, cookie string, page int, since, until time.Time,
) ([]usageEvent, int, bool) {
	payload, err := json.Marshal(map[string]any{
		"page":      page,
		"pageSize":  usageEventsPageSize,
		"startDate": strconv.FormatInt(since.UnixMilli(), 10),
		"endDate":   strconv.FormatInt(until.UnixMilli(), 10),
	})
	if err != nil {
		return nil, 0, false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+usageEventsPath, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, false
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", requestOrigin(c.baseURL))
	request.Header.Set("Cookie", cookie)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, 0, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, 0, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUsageEventsBytes+1))
	if err != nil || len(body) > maxUsageEventsBytes {
		return nil, 0, false
	}
	return parseUsageEventsPage(body)
}

type usageEvent struct {
	when         time.Time
	chargedCents *float64
	listCents    *float64
	tokens       int64
	hasTokens    bool
}

func parseUsageEventsPage(body []byte) ([]usageEvent, int, bool) {
	var page struct {
		TotalUsageEventsCount json.Number       `json:"totalUsageEventsCount"`
		UsageEventsDisplay    []json.RawMessage `json:"usageEventsDisplay"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&page); err != nil {
		return nil, 0, false
	}
	total, _ := page.TotalUsageEventsCount.Int64()
	events := make([]usageEvent, 0, len(page.UsageEventsDisplay))
	for _, raw := range page.UsageEventsDisplay {
		event, ok := parseUsageEvent(raw)
		if !ok {
			continue
		}
		events = append(events, event)
	}
	return events, int(total), true
}

func parseUsageEvent(raw []byte) (usageEvent, bool) {
	var payload struct {
		Timestamp    json.RawMessage `json:"timestamp"`
		ChargedCents json.Number     `json:"chargedCents"`
		TokenUsage   *struct {
			InputTokens      json.Number `json:"inputTokens"`
			OutputTokens     json.Number `json:"outputTokens"`
			CacheWriteTokens json.Number `json:"cacheWriteTokens"`
			CacheReadTokens  json.Number `json:"cacheReadTokens"`
			TotalCents       json.Number `json:"totalCents"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return usageEvent{}, false
	}
	when, ok := parseEventTime(payload.Timestamp)
	if !ok {
		return usageEvent{}, false
	}
	event := usageEvent{when: when}
	if cents, ok := parseNonNegativeFloat(payload.ChargedCents); ok {
		event.chargedCents = &cents
	}
	if payload.TokenUsage != nil {
		if cents, ok := parseNonNegativeFloat(payload.TokenUsage.TotalCents); ok {
			event.listCents = &cents
		}
		tokens := nonNegativeInt(payload.TokenUsage.InputTokens) +
			nonNegativeInt(payload.TokenUsage.OutputTokens) +
			nonNegativeInt(payload.TokenUsage.CacheWriteTokens) +
			nonNegativeInt(payload.TokenUsage.CacheReadTokens)
		if tokens > 0 {
			event.tokens = tokens
			event.hasTokens = true
		}
	}
	return event, true
}

func aggregateUsageEvents(events []usageEvent, now time.Time) (*QuotaSpend, []QuotaDaily, bool) {
	today := now.UTC().Format("2006-01-02")
	spend := &QuotaSpend{PeriodDays: 30, HasMetered: true}
	type dayAcc struct {
		cost   int64
		tokens int64
	}
	days := map[string]*dayAcc{}
	var latest time.Time
	for _, event := range events {
		if event.chargedCents == nil {
			spend.HasMetered = false
		} else if spend.HasMetered {
			spend.MeteredCents += roundCents(*event.chargedCents)
		}
		if event.listCents != nil {
			cents := roundCents(*event.listCents)
			spend.PeriodCents += cents
			spend.HasPeriod = true
			day := event.when.UTC().Format("2006-01-02")
			acc := days[day]
			if acc == nil {
				acc = &dayAcc{}
				days[day] = acc
			}
			acc.cost += cents
			if day == today {
				spend.TodayCents += cents
				spend.HasToday = true
			}
		}
		if event.hasTokens {
			spend.PeriodTokens += event.tokens
			spend.HasPeriodTok = true
			day := event.when.UTC().Format("2006-01-02")
			acc := days[day]
			if acc == nil {
				acc = &dayAcc{}
				days[day] = acc
			}
			acc.tokens += event.tokens
			if latest.IsZero() || event.when.After(latest) {
				latest = event.when
				spend.LatestTokens = event.tokens
				spend.HasLatest = true
			}
		}
	}
	if !spend.HasMetered {
		spend.MeteredCents = 0
	}
	daily := make([]QuotaDaily, 0, len(days))
	for day := range days {
		daily = append(daily, QuotaDaily{Date: day, CostCents: days[day].cost, Tokens: days[day].tokens})
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })
	if !spend.HasMetered && !spend.HasToday && !spend.HasPeriod && !spend.HasLatest && !spend.HasPeriodTok {
		return nil, nil, false
	}
	return spend, daily, true
}

func parseEventTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		millis, err := asNumber.Int64()
		if err != nil {
			if parsed, errFloat := asNumber.Float64(); errFloat == nil {
				millis = int64(parsed)
			} else {
				return time.Time{}, false
			}
		}
		if millis <= 0 {
			return time.Time{}, false
		}
		return time.UnixMilli(millis).UTC(), true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return time.Time{}, false
	}
	millis, err := strconv.ParseInt(strings.TrimSpace(asString), 10, 64)
	if err != nil || millis <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(millis).UTC(), true
}

func parseNonNegativeFloat(value json.Number) (float64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := value.Float64()
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

func nonNegativeInt(value json.Number) int64 {
	if value == "" {
		return 0
	}
	parsed, err := value.Int64()
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func roundCents(value float64) int64 {
	return int64(math.Round(value))
}
