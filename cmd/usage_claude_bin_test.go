package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestParseClaudeBinUsageOutput(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"term-llm-usage","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":42.4,"resets_at":"2026-08-17T13:30:00Z"}}}}}`,
	}, "\n")

	raw, err := parseClaudeBinUsageOutput([]byte(output))
	if err != nil {
		t.Fatalf("parseClaudeBinUsageOutput: %v", err)
	}
	var report claudeBinUsageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.SubscriptionType == nil || *report.SubscriptionType != "max" {
		t.Fatalf("subscription type = %v, want max", report.SubscriptionType)
	}
	if report.RateLimits == nil || report.RateLimits.FiveHour == nil || report.RateLimits.FiveHour.Utilization == nil {
		t.Fatal("missing five-hour utilization")
	}
	if got := *report.RateLimits.FiveHour.Utilization; got != 42.4 {
		t.Fatalf("five-hour utilization = %v, want 42.4", got)
	}
}

func TestParseClaudeBinUsageOutputReportsControlError(t *testing.T) {
	output := `{"type":"control_response","response":{"subtype":"error","request_id":"term-llm-usage","error":"Unknown control request subtype: get_usage"}}`
	_, err := parseClaudeBinUsageOutput([]byte(output))
	if err == nil || !strings.Contains(err.Error(), "update Claude Code") {
		t.Fatalf("error = %v, want update guidance", err)
	}
}

func TestWriteClaudeBinUsage(t *testing.T) {
	plan := "max"
	fiveHour := 42.4
	week := 81.2
	resetSession := "2026-08-17T13:30:00Z"
	resetWeek := "2026-08-21T00:00:00Z"
	report := claudeBinUsageReport{
		SubscriptionType:    &plan,
		RateLimitsAvailable: true,
		RateLimits: &claudeBinRateLimits{
			FiveHour: &claudeBinRateLimit{Utilization: &fiveHour, ResetsAt: &resetSession},
			SevenDay: &claudeBinRateLimit{Utilization: &week, ResetsAt: &resetWeek},
		},
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := writeClaudeBinUsage(&out, report, now); err != nil {
		t.Fatalf("writeClaudeBinUsage: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Claude",
		"Max plan · Live from Claude Code",
		"Current session",
		"5 hour window",
		"42% used",
		"Resets in 1h 30m",
		"Current week · all models",
		"1 week window",
		"81% used",
		"│",
		"Projected: >100%",
		"▲ Limit likely before reset at current pace",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestWriteClaudeBinUsageASCIIUsesSharedForecastLanguage(t *testing.T) {
	plan := "max"
	week := 63.5
	reset := "2026-08-19T08:00:00Z"
	report := claudeBinUsageReport{
		SubscriptionType:    &plan,
		RateLimitsAvailable: true,
		RateLimits: &claudeBinRateLimits{
			SevenDay: &claudeBinRateLimit{Utilization: &week, ResetsAt: &reset},
		},
	}
	now := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := writeClaudeBinUsageWithOptions(&out, report, now, llm.ProviderUsageFormatOptions{ASCII: true}); err != nil {
		t.Fatalf("writeClaudeBinUsageWithOptions: %v", err)
	}
	for _, want := range []string{"[##############-|------]", "Projected: ~89%"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("ASCII output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWriteClaudeBinUsageExplainsUnavailableLimits(t *testing.T) {
	var out bytes.Buffer
	if err := writeClaudeBinUsage(&out, claudeBinUsageReport{}, time.Now()); err != nil {
		t.Fatalf("writeClaudeBinUsage: %v", err)
	}
	for _, want := range []string{"Plan usage is unavailable", "user:profile", "API-key"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, out.String())
		}
	}
}

func TestClaudeBinUsageRowsDeduplicatesModelScopedLimits(t *testing.T) {
	opus := 20.0
	duplicate := 25.0
	rows := claudeBinUsageRows(claudeBinRateLimits{
		SevenDayOpus: &claudeBinRateLimit{Utilization: &opus},
		ModelScoped: []claudeBinModelLimit{
			{DisplayName: "Opus", Utilization: &duplicate},
		},
	})
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %#v", len(rows), rows)
	}
	if got := *rows[0].Utilization; got != opus {
		t.Fatalf("utilization = %v, want %v", got, opus)
	}
}

func TestWithoutEnvironmentVariable(t *testing.T) {
	got := withoutEnvironmentVariable([]string{"PATH=/bin", "ANTHROPIC_API_KEY=secret", "HOME=/tmp"}, "ANTHROPIC_API_KEY")
	if strings.Join(got, "|") != "PATH=/bin|HOME=/tmp" {
		t.Fatalf("filtered environment = %v", got)
	}
}
