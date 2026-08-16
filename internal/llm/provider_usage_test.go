package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestFetchProviderUsageChatGPT(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credentials.SaveChatGPTCredentials(&credentials.ChatGPTCredentials{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		AccountID:   "account-123",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	originalClient := providerUsageHTTPClient
	defer func() { providerUsageHTTPClient = originalClient }()
	providerUsageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != chatGPTUsageURL {
			t.Fatalf("URL = %q, want %q", req.URL, chatGPTUsageURL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-ID"); got != "account-123" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"plan_type":"pro",
				"rate_limit":{
					"allowed":true,
					"limit_reached":false,
					"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":2000000000},
					"secondary_window":{"used_percent":17,"limit_window_seconds":604800,"reset_at":2000600000}
				},
				"credits":{"has_credits":true,"unlimited":false,"balance":"12.5"}
			}`)),
		}, nil
	})}

	report, err := FetchProviderUsage(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("FetchProviderUsage: %v", err)
	}
	if report.Provider != "chatgpt" || report.Plan != "pro" {
		t.Fatalf("report identity = %#v", report)
	}
	if len(report.Limits) != 1 {
		t.Fatalf("limits = %#v", report.Limits)
	}
	limit := report.Limits[0]
	if limit.PrimaryWindow == nil || limit.PrimaryWindow.DurationMinutes != 300 || limit.PrimaryWindow.UsedPercent != 42 {
		t.Fatalf("primary window = %#v", limit.PrimaryWindow)
	}
	if limit.SecondaryWindow == nil || limit.SecondaryWindow.DurationMinutes != 10080 || limit.SecondaryWindow.UsedPercent != 17 {
		t.Fatalf("secondary window = %#v", limit.SecondaryWindow)
	}
	if report.Credits == nil || report.Credits.Balance != "12.5" {
		t.Fatalf("credits = %#v", report.Credits)
	}
}

func TestFetchProviderUsageOpenCodeGo(t *testing.T) {
	originalClient := providerUsageHTTPClient
	defer func() { providerUsageHTTPClient = originalClient }()
	providerUsageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != openCodeGoUsageURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer go-key" {
			t.Fatalf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"usage": {
					"rolling": {"status":"ok","percent":42,"resetsAt":"2026-08-17T15:00:00Z"},
					"weekly": {"status":"ok","percent":18,"resetsAt":"2026-08-23T00:00:00Z"},
					"monthly": {"status":"rate-limited","percent":100,"resetsAt":"2026-08-24T08:00:00Z"}
				}
			}`)),
		}, nil
	})}

	report, err := FetchProviderUsageWithAPIKey(context.Background(), "opencode-go", " go-key ")
	if err != nil {
		t.Fatalf("FetchProviderUsageWithAPIKey: %v", err)
	}
	if report.Provider != "opencode-go" || report.Source != "" || len(report.Limits) != 3 {
		t.Fatalf("report = %#v", report)
	}
	rolling, weekly, monthly := report.Limits[0], report.Limits[1], report.Limits[2]
	if rolling.Name != "Rolling usage" || rolling.PrimaryWindow.DurationMinutes != 300 || rolling.PrimaryWindow.UsedPercent != 42 {
		t.Fatalf("rolling = %#v", rolling)
	}
	if weekly.PrimaryWindow.DurationMinutes != 10080 || weekly.PrimaryWindow.UsedPercent != 18 {
		t.Fatalf("weekly = %#v", weekly)
	}
	if !monthly.LimitReached || monthly.Allowed || monthly.PrimaryWindow.Label != "Monthly window" {
		t.Fatalf("monthly = %#v", monthly)
	}
	if want := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC); !monthly.PrimaryWindow.ResetsAt.Equal(want) {
		t.Fatalf("monthly reset = %s, want %s", monthly.PrimaryWindow.ResetsAt, want)
	}
}

func TestFetchProviderUsageOpenCodeGoRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	_, err := FetchProviderUsage(context.Background(), "opencode-go")
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("error = %v", err)
	}
}

func TestFormatProviderUsage(t *testing.T) {
	now := time.Date(2026, time.August, 17, 8, 0, 0, 0, time.Local)
	report := &ProviderUsage{
		Provider: "chatgpt",
		Plan:     "plus",
		Limits: []ProviderUsageLimit{{
			ID:   "codex",
			Name: "Codex",
			PrimaryWindow: &ProviderUsageWindow{
				UsedPercent:     42,
				DurationMinutes: 300,
				ResetsAt:        now.Add(2*time.Hour + 30*time.Minute),
			},
			SecondaryWindow: &ProviderUsageWindow{
				UsedPercent:     17,
				DurationMinutes: 10080,
				ResetsAt:        now.Add(72 * time.Hour),
			},
		}},
	}

	got := FormatProviderUsage(report, now)
	for _, want := range []string{
		"ChatGPT Codex",
		"Plus plan",
		"  5 hour window\n  ██████████░░░░░░░░░░░░  42% used\n  Resets in 2h 30m",
		"  1 week window\n  ████░░░░░░░░│░░░░░░░░░  17% used\n  Resets in 3d · Projected: ~30%",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted usage missing %q:\n%s", want, got)
		}
	}
}

func TestFormatProviderUsageProjectionMarkerAndWarning(t *testing.T) {
	now := time.Date(2026, time.August, 17, 8, 0, 0, 0, time.UTC)
	report := &ProviderUsage{
		Provider: "chatgpt",
		Limits: []ProviderUsageLimit{
			{
				Name: "Watching",
				PrimaryWindow: &ProviderUsageWindow{
					UsedPercent:     63.5,
					DurationMinutes: 7 * 24 * 60,
					ResetsAt:        now.Add(2 * 24 * time.Hour),
				},
			},
			{
				Name: "Hot",
				PrimaryWindow: &ProviderUsageWindow{
					UsedPercent:     60,
					DurationMinutes: 7 * 24 * 60,
					ResetsAt:        now.Add(7 * 24 * time.Hour / 2),
				},
			},
		},
	}

	got := FormatProviderUsage(report, now)
	for _, want := range []string{
		"██████████████░│░░░░░░  64% used",
		"Resets in 2d · Projected: ~89%",
		"Projected: >100%",
		"▲ Limit likely before reset at current pace",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("projected usage missing %q:\n%s", want, got)
		}
	}
}

func TestFormatProviderUsageSuppressesUnreliableProjections(t *testing.T) {
	now := time.Date(2026, time.August, 17, 8, 0, 0, 0, time.UTC)
	report := &ProviderUsage{
		Provider: "chatgpt",
		Limits: []ProviderUsageLimit{
			{Name: "Short rolling", PrimaryWindow: &ProviderUsageWindow{UsedPercent: 90, DurationMinutes: 5 * 60, ResetsAt: now.Add(time.Hour)}},
			{Name: "Too early", PrimaryWindow: &ProviderUsageWindow{UsedPercent: 20, DurationMinutes: 7 * 24 * 60, ResetsAt: now.Add(6*24*time.Hour + 12*time.Hour)}},
		},
	}

	got := FormatProviderUsage(report, now)
	if strings.Contains(got, "Projected:") || strings.Contains(got, "│") {
		t.Fatalf("unreliable projection should be suppressed:\n%s", got)
	}
}

func TestFormatProviderUsageASCIIAndNarrowFallback(t *testing.T) {
	report := &ProviderUsage{
		Provider: "chatgpt",
		Plan:     "pro",
		Credits:  &ProviderUsageCredits{Balance: "0"},
		Limits: []ProviderUsageLimit{{
			Name:          "Codex",
			PrimaryWindow: &ProviderUsageWindow{UsedPercent: 34, DurationMinutes: 10080},
		}},
	}

	ascii := FormatProviderUsageWithOptions(report, time.Time{}, ProviderUsageFormatOptions{Width: 40, ASCII: true})
	for _, want := range []string{"Pro plan", "[########--------------]  34% used"} {
		if !strings.Contains(ascii, want) {
			t.Fatalf("ASCII usage missing %q:\n%s", want, ascii)
		}
	}
	if strings.Contains(ascii, "credits") {
		t.Fatalf("zero credit balance should be omitted:\n%s", ascii)
	}

	report.Credits.Balance = "12.5"
	withCredits := FormatProviderUsageWithOptions(report, time.Time{}, ProviderUsageFormatOptions{Width: 40, ASCII: true})
	if !strings.Contains(withCredits, "Pro plan · 12.5 credits") {
		t.Fatalf("positive credit balance missing:\n%s", withCredits)
	}

	narrow := FormatProviderUsageWithOptions(report, time.Time{}, ProviderUsageFormatOptions{Width: 18})
	if strings.ContainsAny(narrow, "█░") || !strings.Contains(narrow, "  34% used") {
		t.Fatalf("narrow usage should omit meter:\n%s", narrow)
	}
}

func TestFetchProviderUsageRejectsUnsupportedProvider(t *testing.T) {
	_, err := FetchProviderUsage(context.Background(), "openai")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v", err)
	}
}
