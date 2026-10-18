package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

func (c openRouterClient) analytics(ctx context.Context, key string, options OpenRouterOptions, report *OpenRouterReport) error {
	payload := map[string]any{"dimensions": []string{"model"}, "granularity": "day", "limit": 1000, "metrics": []string{"request_count", "total_usage", "tokens_prompt", "tokens_completion", "cached_tokens", "reasoning_tokens", "byok_usage", "credits_usage", "byok_fees"}, "time_range": map[string]string{"start": options.Since.UTC().Format(time.RFC3339Nano), "end": options.Until.UTC().Format(time.RFC3339Nano)}}
	data, err := c.request(ctx, key, "/analytics/query", payload)
	if err != nil {
		return err
	}
	rows, err := openRouterRows(data)
	if err != nil {
		return err
	}
	var cachedAt *time.Time
	if raw := data["cachedAt"]; len(raw) > 0 && string(raw) != "null" {
		milliseconds, err := openRouterCount(raw)
		if err != nil {
			return err
		}
		t := time.UnixMilli(milliseconds).UTC()
		if t.Year() > 9999 {
			return errOpenRouterData
		}
		cachedAt = &t
	}
	totals := OpenRouterAmounts{}
	rows = openRouterSortedRows(rows)
	models := map[string]OpenRouterAmounts{}
	days := map[string]OpenRouterAmounts{}
	for _, row := range rows {
		model, date, amounts, err := openRouterRow(row, options)
		if err != nil {
			return err
		}
		total, err := openRouterAdd(totals, amounts)
		if err != nil {
			return err
		}
		totals = total
		combined, err := openRouterAdd(models[model], amounts)
		if err != nil {
			return err
		}
		models[model] = combined
		combined, err = openRouterAdd(days[date], amounts)
		if err != nil {
			return err
		}
		days[date] = combined
	}
	// Publish only after every row and aggregate has passed completeness checks.
	totals.setCacheHitPercent()
	report.Totals = &totals
	report.CachedAt = cachedAt
	for model, amounts := range models {
		amounts.setCacheHitPercent()
		report.Models = append(report.Models, OpenRouterModelUsage{model, amounts})
	}
	for date, amounts := range days {
		amounts.setCacheHitPercent()
		report.Daily = append(report.Daily, OpenRouterDailyUsage{date, amounts})
	}
	sort.Slice(report.Models, func(i, j int) bool {
		a, b := report.Models[i], report.Models[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.Model < b.Model
	})
	sort.Slice(report.Daily, func(i, j int) bool { return report.Daily[i].Date < report.Daily[j].Date })
	if len(rows) == 0 {
		report.Warnings = append(report.Warnings, "OpenRouter returned no activity for the requested period.")
	}
	return nil
}
func openRouterRows(data openRouterObject) ([]openRouterObject, error) {
	var rows []openRouterObject
	if json.Unmarshal(data["data"], &rows) != nil || rows == nil {
		return nil, errOpenRouterData
	}
	metadata, err := openRouterNested(data, "metadata")
	if err != nil {
		return nil, err
	}
	var truncated *bool
	if json.Unmarshal(metadata["truncated"], &truncated) != nil || truncated == nil {
		return nil, errOpenRouterData
	}
	if *truncated {
		return nil, fmt.Errorf("OpenRouter analytics response was truncated; try a narrower date range with --days")
	}
	count, err := openRouterCount(metadata["row_count"])
	if err != nil || count != int64(len(rows)) {
		return nil, errOpenRouterData
	}
	return rows, nil
}
func openRouterRow(row openRouterObject, options OpenRouterOptions) (string, string, OpenRouterAmounts, error) {
	a := OpenRouterAmounts{}
	var model string
	if json.Unmarshal(row["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return "", "", a, errOpenRouterData
	}
	date, err := openRouterDate(row, options)
	if err != nil {
		return "", "", a, err
	}
	counts := map[string]*int64{"request_count": &a.Requests, "tokens_prompt": &a.InputTokens, "tokens_completion": &a.OutputTokens, "cached_tokens": &a.CachedTokens, "reasoning_tokens": &a.ReasoningTokens}
	for name, target := range counts {
		value, err := openRouterCount(row[name])
		if err != nil {
			return "", "", a, err
		}
		*target = value
	}
	err = openRouterMoneyFields(row, map[string]*float64{"total_usage": &a.CostUSD, "byok_usage": &a.BYOKUsageUSD, "credits_usage": &a.CreditsUsageUSD, "byok_fees": &a.BYOKFeesUSD})
	return model, date, a, err
}
func openRouterDate(row openRouterObject, options OpenRouterOptions) (string, error) {
	date := ""
	for _, name := range []string{"created_at__day", "date__day"} {
		raw, ok := row[name]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return "", errOpenRouterData
		}
		normalized, err := openRouterDay(value)
		if err != nil || (date != "" && date != normalized) {
			return "", errOpenRouterData
		}
		date = normalized
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil || day.Format("2006-01-02") != date || !day.Before(options.Until) || !day.AddDate(0, 0, 1).After(options.Since) {
		return "", errOpenRouterData
	}
	return date, nil
}
func openRouterAdd(a, b OpenRouterAmounts) (OpenRouterAmounts, error) {
	counts := [][2]*int64{{&a.Requests, &b.Requests}, {&a.InputTokens, &b.InputTokens}, {&a.OutputTokens, &b.OutputTokens}, {&a.CachedTokens, &b.CachedTokens}, {&a.ReasoningTokens, &b.ReasoningTokens}}
	for _, pair := range counts {
		if *pair[0] > math.MaxInt64-*pair[1] {
			return OpenRouterAmounts{}, errOpenRouterData
		}
		*pair[0] += *pair[1]
	}
	money := [][2]*float64{{&a.CostUSD, &b.CostUSD}, {&a.BYOKUsageUSD, &b.BYOKUsageUSD}, {&a.CreditsUsageUSD, &b.CreditsUsageUSD}, {&a.BYOKFeesUSD, &b.BYOKFeesUSD}}
	for _, pair := range money {
		*pair[0] += *pair[1]
		if math.IsInf(*pair[0], 0) {
			return OpenRouterAmounts{}, errOpenRouterData
		}
	}
	return a, nil
}

// openRouterDay accepts both observed date-only buckets and documented UTC
// midnight timestamps. Non-midnight or non-UTC buckets are ambiguous.
func openRouterDay(value string) (string, error) {
	if day, err := time.Parse("2006-01-02", value); err == nil && day.Format("2006-01-02") == value {
		return value, nil
	}
	instant, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", errOpenRouterData
	}
	_, offset := instant.Zone()
	if offset != 0 || instant.Hour() != 0 || instant.Minute() != 0 || instant.Second() != 0 || instant.Nanosecond() != 0 {
		return "", errOpenRouterData
	}
	return instant.UTC().Format("2006-01-02"), nil
}

// Cache hits are an input-token ratio. Derive only after aggregation so the
// total is token-weighted, not an average of model or daily percentages.
func (a *OpenRouterAmounts) setCacheHitPercent() {
	a.CacheHitPercent = nil
	if a.InputTokens > 0 {
		percent := float64(a.CachedTokens) / float64(a.InputTokens) * 100
		a.CacheHitPercent = &percent
	}
}

// Precompute canonical keys once, rather than marshaling in every comparison.
// This stabilizes floating-point sums even if the API changes row order.
func openRouterSortedRows(rows []openRouterObject) []openRouterObject {
	type keyedRow struct {
		key string
		row openRouterObject
	}
	keyed := make([]keyedRow, len(rows))
	for i, row := range rows {
		key, _ := json.Marshal(row) // Values are raw JSON from a decoded response.
		keyed[i] = keyedRow{string(key), row}
	}
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].key < keyed[j].key })
	for i, row := range keyed {
		rows[i] = row.row
	}
	return rows
}
