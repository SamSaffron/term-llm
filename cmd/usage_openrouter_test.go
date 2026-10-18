package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/usage"
	"github.com/spf13/cobra"
)

func openRouterTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	resetUsageTestFlags()
	t.Cleanup(resetUsageTestFlags)
	c := &cobra.Command{}
	c.Flags().IntVar(&usageDays, "days", 30, "")
	c.Flags().BoolVar(&usageDaily, "daily", false, "")
	c.Flags().StringVar(&usageSince, "since", "", "")
	c.Flags().StringVar(&usageUntil, "until", "", "")
	c.Flags().BoolVar(&usageIncludeExternal, "include-external", false, "")
	c.Flags().StringVar(&usageCopilotScope, "copilot-scope", "user", "")
	return c
}

func TestOpenRouterUsagePeriod(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 14, 15, 0, time.FixedZone("offset", 3600))
	for _, tt := range []struct {
		name         string
		flags        []string
		since, until string
		bad          bool
	}{
		{name: "default", since: "2026-09-19T00:00:00Z", until: "2026-10-18T12:14:15Z"},
		{name: "one day", flags: []string{"--days=1"}, since: "2026-10-18T00:00:00Z", until: "2026-10-18T12:14:15Z"},
		{name: "historical until", flags: []string{"--until=20260301"}, since: "2026-01-31T00:00:00Z", until: "2026-03-02T00:00:00Z"},
		{name: "leap day", flags: []string{"--since=20240229", "--until=20240229"}, since: "2024-02-29T00:00:00Z", until: "2024-03-01T00:00:00Z"},
		{name: "explicit default conflict", flags: []string{"--days=30", "--since=20261001"}, bad: true},
		{name: "empty date conflict", flags: []string{"--days=30", "--until="}, bad: true},
		{name: "empty since", flags: []string{"--since="}, bad: true},
		{name: "zero", flags: []string{"--days=0"}, bad: true},
		{name: "negative", flags: []string{"--days=-1"}, bad: true},
		{name: "large", flags: []string{"--days=3661"}, bad: true},
		{name: "future", flags: []string{"--until=20261019"}, bad: true},
		{name: "inverted", flags: []string{"--since=20261018", "--until=20261017"}, bad: true},
		{name: "invalid", flags: []string{"--since=20260230"}, bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := openRouterTestCommand(t)
			if err := c.ParseFlags(tt.flags); err != nil {
				t.Fatal(err)
			}
			since, until, err := openRouterUsagePeriod(c, now)
			if tt.bad {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if since.Format(time.RFC3339) != tt.since || until.Format(time.RFC3339) != tt.until {
				t.Fatalf("period %s / %s", since, until)
			}
		})
	}
}

func TestOpenRouterUsageEarlyValidation(t *testing.T) {
	old := loadProviderUsageConfig
	loadProviderUsageConfig = func() (*config.Config, error) { t.Fatal("must validate before config access"); return nil, nil }
	t.Cleanup(func() { loadProviderUsageConfig = old })
	for _, tt := range []struct {
		provider string
		flags    []string
	}{
		{"unknown", nil}, {"copilot", []string{"--days=30"}}, {"grok", []string{"--daily=false"}}, {"all", []string{"--daily"}},
		{"openrouter", []string{"--include-external=false"}}, {"openrouter", []string{"--copilot-scope=user"}}, {"openrouter", []string{"--days=0"}},
	} {
		t.Run(tt.provider+strings.Join(tt.flags, ""), func(t *testing.T) {
			c := openRouterTestCommand(t)
			usageProvider = tt.provider
			_ = c.ParseFlags(tt.flags)
			if err := runUsage(c, nil); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestOpenRouterUsageCredentials(t *testing.T) {
	old := loadProviderUsageConfig
	t.Cleanup(func() { loadProviderUsageConfig = old })
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "env-management")
	t.Setenv("OPENROUTER_API_KEY", "env-api")
	t.Setenv("OPENROUTER_TEST_EMPTY", "")
	p := config.ProviderConfig{ManagementKey: "configured-management", APIKey: "${OPENROUTER_TEST_EMPTY}", BaseURL: "$(must-not-run)"}
	loadProviderUsageConfig = func() (*config.Config, error) {
		return &config.Config{Providers: map[string]config.ProviderConfig{"openrouter": p}}, nil
	}
	opts, err := openRouterUsageCredentials()
	if err != nil || opts.ManagementKey != "configured-management" || len(opts.CredentialWarnings) != 0 {
		t.Fatalf("management resolution: %v", err)
	}
	if key, err := opts.APIKey(); err == nil || key != "" || strings.Contains(err.Error(), "OPENROUTER_TEST_EMPTY") {
		t.Fatalf("explicit empty key must fail safely: %q %v", key, err)
	}
	p.ManagementKey = "file://./does-not-exist-secret"
	p.APIKey = ""
	opts, err = openRouterUsageCredentials()
	if err != nil || opts.ManagementKey != "" || len(opts.CredentialWarnings) != 1 || strings.Contains(opts.CredentialWarnings[0], "secret") {
		t.Fatalf("unsafe credential result: %v", err)
	}
	if key, err := opts.APIKey(); err != nil || key != "env-api" {
		t.Fatal("env fallback failed")
	}
	p.ManagementKey = ""
	p.ResolvedAPIKey = "resolved-api"
	opts, err = openRouterUsageCredentials()
	if err != nil || opts.ManagementKey != "env-management" {
		t.Fatal("management env fallback failed")
	}
	if key, _ := opts.APIKey(); key != "resolved-api" {
		t.Fatal("resolved fallback failed")
	}
	loadProviderUsageConfig = func() (*config.Config, error) { return nil, errors.New("secret configuration content") }
	if _, err := openRouterUsageCredentials(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe config error %v", err)
	}
}

type openRouterFailWriter struct{}

func (openRouterFailWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func TestOpenRouterUsageOutput(t *testing.T) {
	oldFetch, oldLoad, oldNow := fetchOpenRouterUsage, loadProviderUsageConfig, openRouterUsageNow
	t.Cleanup(func() { fetchOpenRouterUsage, loadProviderUsageConfig, openRouterUsageNow = oldFetch, oldLoad, oldNow })
	loadProviderUsageConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	openRouterUsageNow = func() time.Time { return now }
	amounts := usage.OpenRouterAmounts{Requests: 2, InputTokens: 100, OutputTokens: 20, CachedTokens: 90, ReasoningTokens: 10, CostUSD: 0.012345}
	report := &usage.OpenRouterReport{SchemaVersion: 1, Provider: "openrouter", Source: "openrouter_analytics", Scope: "account", Status: "complete", Totals: &amounts, Models: []usage.OpenRouterModelUsage{{Model: "vendor/model\x1b\n\u202e", OpenRouterAmounts: amounts}}, Daily: []usage.OpenRouterDailyUsage{{Date: "2026-10-18", OpenRouterAmounts: amounts}}, Warnings: []string{"notice\x1b\n"}, Balance: &usage.OpenRouterBalance{Remaining: 5}}
	type contextKey struct{}
	fetchOpenRouterUsage = func(ctx context.Context, opts usage.OpenRouterOptions) (*usage.OpenRouterReport, error) {
		if ctx.Value(contextKey{}) != "forwarded" {
			t.Fatal("context not forwarded")
		}
		if !opts.Until.Equal(now) || !opts.Since.Equal(now.AddDate(0, 0, -29).Truncate(24*time.Hour)) {
			t.Fatal("wrong period")
		}
		report.Period = usage.OpenRouterPeriod{Since: opts.Since, Until: opts.Until, Timezone: "UTC"}
		return report, nil
	}
	c := openRouterTestCommand(t)
	usageProvider = "openrouter"
	c.SetContext(context.WithValue(context.Background(), contextKey{}, "forwarded"))
	var out bytes.Buffer
	c.SetOut(&out)
	if err := runUsage(c, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MODEL", "vendor/model", "100", "20", "$0.012345", "TOTAL", "Account balance", "inclusive", "exclusive"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.ContainsAny(out.String(), "\x1b\u202e") || strings.Contains(out.String(), "190") {
		t.Fatalf("unsafe or double-counted output: %q", out.String())
	}
	out.Reset()
	usageDaily = true
	if err := runUsage(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "UTC DAY") || strings.Contains(out.String(), "vendor/model") {
		t.Fatal(out.String())
	}
	out.Reset()
	usageJSON = true
	if err := runUsage(c, nil); err != nil {
		t.Fatal(err)
	}
	var got usage.OpenRouterReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Models[0].Model != report.Models[0].Model || got.Totals.CachedTokens != 90 {
		t.Fatal("JSON not full report")
	}
	c.SetOut(openRouterFailWriter{})
	if err := runUsage(c, nil); err == nil {
		t.Fatal("JSON writer error lost")
	}
	usageJSON = false
	if err := runUsage(c, nil); err == nil {
		t.Fatal("table writer error lost")
	}
	report.Totals = nil
	report.Models = []usage.OpenRouterModelUsage{}
	report.Daily = []usage.OpenRouterDailyUsage{}
	report.Status = "summary_only"
	report.KeyUsage = &usage.OpenRouterKeyUsage{Usage: 3, UsageDaily: 1}
	out.Reset()
	c.SetOut(&out)
	if err := runUsage(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Requested-period model/daily breakdown unavailable") || !strings.Contains(out.String(), "This key only (not requested-period totals)") {
		t.Fatal(out.String())
	}
	usageJSON = true
	out.Reset()
	if err := runUsage(c, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"totals": null`, `"models": []`, `"daily": []`} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
}

func TestOpenRouterUsageMidnightEmptyPeriod(t *testing.T) {
	c := openRouterTestCommand(t)
	if err := c.ParseFlags([]string{"--days=1"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := openRouterUsagePeriod(c, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "--days 2") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenRouterCacheAndCompactTable(t *testing.T) {
	percent := 97.25
	amounts := usage.OpenRouterAmounts{Requests: 120000, InputTokens: 12780000, OutputTokens: 987398793874, CacheHitPercent: &percent}
	r := &usage.OpenRouterReport{Totals: &amounts, Models: []usage.OpenRouterModelUsage{{Model: "test/model", OpenRouterAmounts: amounts}}}
	var out bytes.Buffer
	if err := writeOpenRouterUsageText(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CACHE %", "97.2%", "120k", "12.78M", "987.4B"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "987398793874") {
		t.Fatal("raw count in terminal output")
	}
	amounts.CacheHitPercent = nil
	r.Totals = &amounts
	r.Models[0].OpenRouterAmounts = amounts
	out.Reset()
	if err := writeOpenRouterUsageText(&out, r, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "—") {
		t.Fatal("unknown cache ratio should be a dash")
	}
	encoded, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(encoded), `"output_tokens":987398793874`) || !strings.Contains(string(encoded), `"cache_hit_percent":null`) {
		t.Fatalf("exact JSON: %s (%v)", encoded, err)
	}
}
