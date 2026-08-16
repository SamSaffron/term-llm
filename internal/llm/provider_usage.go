package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

const (
	chatGPTUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	openCodeGoUsageURL = opencodeGoBaseURL + "/usage"
)

// ProviderUsage is a live usage snapshot reported by a provider account.
type ProviderUsage struct {
	Provider   string                `json:"provider"`
	Plan       string                `json:"plan,omitempty"`
	Source     string                `json:"source,omitempty"`
	Limits     []ProviderUsageLimit  `json:"limits,omitempty"`
	Credits    *ProviderUsageCredits `json:"credits,omitempty"`
	LimitState string                `json:"limit_state,omitempty"`
}

// ProviderUsageLimit describes one provider-enforced usage limit.
type ProviderUsageLimit struct {
	ID              string               `json:"id"`
	Name            string               `json:"name,omitempty"`
	Allowed         bool                 `json:"allowed"`
	LimitReached    bool                 `json:"limit_reached"`
	PrimaryWindow   *ProviderUsageWindow `json:"primary_window,omitempty"`
	SecondaryWindow *ProviderUsageWindow `json:"secondary_window,omitempty"`
}

// ProviderUsageWindow describes one provider usage window.
type ProviderUsageWindow struct {
	Label           string    `json:"label,omitempty"`
	UsedPercent     float64   `json:"used_percent"`
	DurationMinutes int       `json:"duration_minutes,omitempty"`
	ResetsAt        time.Time `json:"resets_at,omitempty"`
	Detail          string    `json:"detail,omitempty"`
}

// ProviderUsageCredits describes optional account credits returned by the provider.
type ProviderUsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type chatGPTUsageResponse struct {
	PlanType             string                        `json:"plan_type"`
	RateLimit            *chatGPTUsageRateLimit        `json:"rate_limit"`
	AdditionalRateLimits []chatGPTAdditionalUsageLimit `json:"additional_rate_limits"`
	Credits              *ProviderUsageCredits         `json:"credits"`
	RateLimitReachedType *struct {
		Type string `json:"type"`
	} `json:"rate_limit_reached_type"`
}

type chatGPTAdditionalUsageLimit struct {
	LimitName      string                 `json:"limit_name"`
	MeteredFeature string                 `json:"metered_feature"`
	RateLimit      *chatGPTUsageRateLimit `json:"rate_limit"`
}

type chatGPTUsageRateLimit struct {
	Allowed         bool                `json:"allowed"`
	LimitReached    bool                `json:"limit_reached"`
	PrimaryWindow   *chatGPTUsageWindow `json:"primary_window"`
	SecondaryWindow *chatGPTUsageWindow `json:"secondary_window"`
}

type chatGPTUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type openCodeGoUsageResponse struct {
	Usage struct {
		Rolling openCodeGoUsageWindow `json:"rolling"`
		Weekly  openCodeGoUsageWindow `json:"weekly"`
		Monthly openCodeGoUsageWindow `json:"monthly"`
	} `json:"usage"`
}

type openCodeGoUsageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

var providerUsageHTTPClient = chatGPTHTTPClient

// FetchProviderUsage fetches current account-level usage directly from provider.
func FetchProviderUsage(ctx context.Context, provider string) (*ProviderUsage, error) {
	return FetchProviderUsageWithAPIKey(ctx, provider, "")
}

// FetchProviderUsageWithAPIKey fetches live usage with an explicitly resolved
// API key when the provider requires one. OAuth providers ignore apiKey.
func FetchProviderUsageWithAPIKey(ctx context.Context, provider, apiKey string) (*ProviderUsage, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "chatgpt":
		return fetchChatGPTProviderUsage(ctx)
	case "opencode-go":
		if strings.TrimSpace(apiKey) == "" {
			apiKey = os.Getenv("OPENCODE_API_KEY")
		}
		return fetchOpenCodeGoProviderUsage(ctx, apiKey)
	default:
		return nil, fmt.Errorf("live usage is not supported for provider %q", provider)
	}
}

func fetchChatGPTProviderUsage(ctx context.Context) (*ProviderUsage, error) {
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return nil, fmt.Errorf("load ChatGPT credentials: %w (run 'term-llm auth login chatgpt')", err)
	}
	if creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			return nil, fmt.Errorf("refresh ChatGPT credentials: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatGPTUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create ChatGPT usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", creds.AccountID)
	}
	req.Header.Set("originator", chatGPTCodexOriginator)
	req.Header.Set("User-Agent", chatGPTCodexUserAgent)
	req.Header.Set("version", chatGPTCodexClientVersion)

	client := providerUsageHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch ChatGPT usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := fmt.Sprintf("ChatGPT usage request failed: %s", resp.Status)
		if len(body) > 0 {
			message += ": " + string(body)
		}
		return nil, newHTTPStatusErrorMessage(message, resp, body)
	}

	var decoded chatGPTUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode ChatGPT usage: %w", err)
	}
	return normalizeChatGPTProviderUsage(decoded), nil
}

func fetchOpenCodeGoProviderUsage(ctx context.Context, apiKey string) (*ProviderUsage, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("OpenCode Go usage requires an API key (configure providers.opencode-go.api_key or set OPENCODE_API_KEY)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openCodeGoUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create OpenCode Go usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := providerUsageHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenCode Go usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := fmt.Sprintf("OpenCode Go usage request failed: %s", resp.Status)
		if len(body) > 0 {
			message += ": " + string(body)
		}
		return nil, newHTTPStatusErrorMessage(message, resp, body)
	}

	var decoded openCodeGoUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode OpenCode Go usage: %w", err)
	}
	return normalizeOpenCodeGoProviderUsage(decoded)
}

func normalizeOpenCodeGoProviderUsage(raw openCodeGoUsageResponse) (*ProviderUsage, error) {
	report := &ProviderUsage{Provider: "opencode-go"}
	windows := []struct {
		id       string
		name     string
		label    string
		duration int
		usage    openCodeGoUsageWindow
	}{
		{id: "rolling", name: "Rolling usage", label: "5 hour window", duration: 5 * 60, usage: raw.Usage.Rolling},
		{id: "weekly", name: "Weekly usage", label: "1 week window", duration: 7 * 24 * 60, usage: raw.Usage.Weekly},
		{id: "monthly", name: "Monthly usage", label: "Monthly window", usage: raw.Usage.Monthly},
	}
	for _, item := range windows {
		reset, err := time.Parse(time.RFC3339, strings.TrimSpace(item.usage.ResetsAt))
		if err != nil {
			return nil, fmt.Errorf("decode OpenCode Go %s reset time %q: %w", item.id, item.usage.ResetsAt, err)
		}
		reached := strings.EqualFold(strings.TrimSpace(item.usage.Status), "rate-limited")
		report.Limits = append(report.Limits, ProviderUsageLimit{
			ID:           item.id,
			Name:         item.name,
			Allowed:      !reached,
			LimitReached: reached,
			PrimaryWindow: &ProviderUsageWindow{
				Label:           item.label,
				UsedPercent:     item.usage.Percent,
				DurationMinutes: item.duration,
				ResetsAt:        reset,
			},
		})
	}
	return report, nil
}

func normalizeChatGPTProviderUsage(raw chatGPTUsageResponse) *ProviderUsage {
	report := &ProviderUsage{Provider: "chatgpt", Plan: raw.PlanType, Credits: raw.Credits}
	if raw.RateLimitReachedType != nil {
		report.LimitState = raw.RateLimitReachedType.Type
	}
	if raw.RateLimit != nil {
		report.Limits = append(report.Limits, normalizeChatGPTUsageLimit("codex", "Codex", raw.RateLimit))
	}
	for _, additional := range raw.AdditionalRateLimits {
		if additional.RateLimit == nil {
			continue
		}
		id := strings.TrimSpace(additional.MeteredFeature)
		if id == "" {
			id = strings.TrimSpace(additional.LimitName)
		}
		report.Limits = append(report.Limits, normalizeChatGPTUsageLimit(id, additional.LimitName, additional.RateLimit))
	}
	return report
}

func normalizeChatGPTUsageLimit(id, name string, raw *chatGPTUsageRateLimit) ProviderUsageLimit {
	return ProviderUsageLimit{
		ID:              id,
		Name:            name,
		Allowed:         raw.Allowed,
		LimitReached:    raw.LimitReached,
		PrimaryWindow:   normalizeChatGPTUsageWindow(raw.PrimaryWindow),
		SecondaryWindow: normalizeChatGPTUsageWindow(raw.SecondaryWindow),
	}
}

func normalizeChatGPTUsageWindow(raw *chatGPTUsageWindow) *ProviderUsageWindow {
	if raw == nil {
		return nil
	}
	window := &ProviderUsageWindow{
		UsedPercent:     raw.UsedPercent,
		DurationMinutes: int(raw.LimitWindowSeconds / 60),
	}
	if raw.ResetAt > 0 {
		window.ResetsAt = time.Unix(raw.ResetAt, 0)
	}
	return window
}

// ProviderUsageFormatOptions controls the shared CLI and TUI usage layout.
type ProviderUsageFormatOptions struct {
	// Width is the available content width. Zero uses the default meter width.
	Width int
	// ASCII replaces block meter glyphs for limited terminals.
	ASCII bool
}

// FormatProviderUsage formats a live provider usage report using default meter options.
func FormatProviderUsage(report *ProviderUsage, now time.Time) string {
	return FormatProviderUsageWithOptions(report, now, ProviderUsageFormatOptions{})
}

// FormatProviderUsageWithOptions formats a responsive stacked provider usage report.
func FormatProviderUsageWithOptions(report *ProviderUsage, now time.Time, opts ProviderUsageFormatOptions) string {
	if report == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(providerUsageDisplayName(report.Provider))
	if metadata := formatProviderUsageMetadata(report); metadata != "" {
		b.WriteString("\n")
		b.WriteString(metadata)
	}
	for _, limit := range report.Limits {
		name := strings.TrimSpace(limit.Name)
		if name == "" {
			name = limit.ID
		}
		fmt.Fprintf(&b, "\n\n%s", name)
		if limit.PrimaryWindow != nil {
			formatProviderUsageWindow(&b, limit.PrimaryWindow, now, opts)
		}
		if limit.SecondaryWindow != nil {
			formatProviderUsageWindow(&b, limit.SecondaryWindow, now, opts)
		}
		if limit.LimitReached {
			b.WriteString("\n  ! Limit reached")
		}
	}
	if report.LimitState != "" {
		fmt.Fprintf(&b, "\n\n! %s", formatUsageLabel(report.LimitState))
	}
	return b.String()
}

func formatProviderUsageMetadata(report *ProviderUsage) string {
	parts := make([]string, 0, 2)
	if plan := formatUsageLabel(report.Plan); plan != "" {
		parts = append(parts, plan+" plan")
	}
	if report.Credits != nil {
		switch {
		case report.Credits.Unlimited:
			parts = append(parts, "Unlimited credits")
		case positiveCreditBalance(report.Credits.Balance):
			parts = append(parts, strings.TrimSpace(report.Credits.Balance)+" credits")
		}
	}
	if report.Source != "" {
		parts = append(parts, report.Source)
	}
	return strings.Join(parts, " · ")
}

func positiveCreditBalance(balance string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(balance), 64)
	return err == nil && value > 0
}

func formatProviderUsageWindow(b *strings.Builder, window *ProviderUsageWindow, now time.Time, opts ProviderUsageFormatOptions) {
	label := strings.TrimSpace(window.Label)
	if label == "" {
		label = formatUsageWindowDuration(window.DurationMinutes)
	}
	fmt.Fprintf(b, "\n  %s", label)
	usedPercent := min(100, max(0, window.UsedPercent))
	elapsedFraction, projected, hasProjection := providerUsageProjection(window, now)
	barWidth := providerUsageBarWidth(opts)
	if barWidth > 0 {
		var marker *float64
		if hasProjection {
			marker = &elapsedFraction
		}
		fmt.Fprintf(b, "\n  %s  %.0f%% used", formatProviderUsageMeter(usedPercent, barWidth, opts.ASCII, marker), usedPercent)
	} else {
		fmt.Fprintf(b, "\n  %.0f%% used", usedPercent)
	}
	if window.Detail != "" {
		fmt.Fprintf(b, "\n  %s", window.Detail)
	}
	if !window.ResetsAt.IsZero() {
		fmt.Fprintf(b, "\n  Resets %s", formatUsageReset(window.ResetsAt, now))
		if hasProjection {
			if projected > 100 {
				b.WriteString(" · Projected: >100%")
			} else {
				fmt.Fprintf(b, " · Projected: ~%.0f%%", projected)
			}
		}
	}
	if hasProjection && projected > 100 {
		if opts.ASCII {
			b.WriteString("\n  ! Limit likely before reset at current pace")
		} else {
			b.WriteString("\n  ▲ Limit likely before reset at current pace")
		}
	}
}

func providerUsageProjection(window *ProviderUsageWindow, now time.Time) (elapsedFraction, projected float64, ok bool) {
	if window == nil {
		return 0, 0, false
	}
	usedPercent := min(100, max(0, window.UsedPercent))
	if window.DurationMinutes < 7*24*60 || window.ResetsAt.IsZero() || usedPercent < 2 {
		return 0, 0, false
	}
	duration := time.Duration(window.DurationMinutes) * time.Minute
	remaining := window.ResetsAt.Sub(now)
	if remaining <= 0 || remaining > duration {
		return 0, 0, false
	}
	elapsed := duration - remaining
	minimumElapsed := max(15*time.Minute, duration*15/100)
	if elapsed < minimumElapsed {
		return 0, 0, false
	}
	elapsedFraction = float64(elapsed) / float64(duration)
	if elapsedFraction <= 0 || elapsedFraction > 1 {
		return 0, 0, false
	}
	projected = usedPercent / elapsedFraction
	return elapsedFraction, projected, true
}

func providerUsageBarWidth(opts ProviderUsageFormatOptions) int {
	const defaultWidth = 22
	if opts.Width <= 0 {
		return defaultWidth
	}
	overhead := 13 // indentation, spacing, and "100% used"
	if opts.ASCII {
		overhead += 2 // surrounding brackets
	}
	available := opts.Width - overhead
	if available < 8 {
		return 0
	}
	return min(defaultWidth, available)
}

func formatProviderUsageMeter(usedPercent float64, width int, ascii bool, elapsedFraction *float64) string {
	usedPercent = min(100, max(0, usedPercent))
	filled := 0
	if usedPercent > 0 {
		filled = int(math.Ceil(usedPercent * float64(width) / 100))
	}
	filled = min(width, max(0, filled))
	empty, full, marker := "░", "█", "│"
	if ascii {
		empty, full, marker = "-", "#", "|"
	}
	cells := make([]string, width)
	for i := range cells {
		cells[i] = empty
		if i < filled {
			cells[i] = full
		}
	}
	if elapsedFraction != nil && width > 0 {
		position := int(math.Round(min(1, max(0, *elapsedFraction)) * float64(width-1)))
		cells[position] = marker
	}
	meter := strings.Join(cells, "")
	if ascii {
		return "[" + meter + "]"
	}
	return meter
}

func formatUsageWindowDuration(minutes int) string {
	switch {
	case minutes <= 0:
		return "Window"
	case minutes%10080 == 0:
		return fmt.Sprintf("%d week window", minutes/10080)
	case minutes%1440 == 0:
		return fmt.Sprintf("%d day window", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%d hour window", minutes/60)
	default:
		return fmt.Sprintf("%d minute window", minutes)
	}
}

func formatUsageReset(reset, now time.Time) string {
	remaining := reset.Sub(now)
	if remaining > 0 {
		totalMinutes := int64(remaining.Round(time.Minute) / time.Minute)
		if totalMinutes < 1 {
			return "in less than a minute"
		}
		days := totalMinutes / (24 * 60)
		hours := totalMinutes % (24 * 60) / 60
		minutes := totalMinutes % 60
		parts := make([]string, 0, 3)
		if days > 0 {
			parts = append(parts, fmt.Sprintf("%dd", days))
		}
		if hours > 0 {
			parts = append(parts, fmt.Sprintf("%dh", hours))
		}
		if minutes > 0 {
			parts = append(parts, fmt.Sprintf("%dm", minutes))
		}
		return "in " + strings.Join(parts, " ")
	}
	return reset.Local().Format("Jan 2, 3:04 PM")
}

func providerUsageDisplayName(provider string) string {
	switch {
	case strings.EqualFold(provider, "chatgpt"):
		return "ChatGPT Codex"
	case strings.EqualFold(provider, "opencode-go"):
		return "OpenCode Go"
	}
	return formatUsageLabel(provider)
}

func formatUsageLabel(value string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSpace(value)))
	for i := range words {
		words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
	}
	return strings.Join(words, " ")
}
