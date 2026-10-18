package llm

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/providerusage"
)

// Deprecated: use providerusage.Report.
type ProviderUsage = providerusage.Report

// Deprecated: use providerusage.Limit.
type ProviderUsageLimit = providerusage.Limit

// Deprecated: use providerusage.Window.
type ProviderUsageWindow = providerusage.Window

// Deprecated: use providerusage.Credits.
type ProviderUsageCredits = providerusage.Credits

// Deprecated: use providerusage.FormatOptions.
type ProviderUsageFormatOptions = providerusage.FormatOptions

// Deprecated: use providerusage.Fetch.
func FetchProviderUsage(ctx context.Context, provider string) (*ProviderUsage, error) {
	return providerusage.Fetch(ctx, provider)
}

// Deprecated: use providerusage.FetchWithAPIKey.
func FetchProviderUsageWithAPIKey(ctx context.Context, provider, apiKey string) (*ProviderUsage, error) {
	return providerusage.FetchWithAPIKey(ctx, provider, apiKey)
}

// Deprecated: use providerusage.Format.
func FormatProviderUsage(report *ProviderUsage, now time.Time) string {
	return providerusage.Format(report, now)
}

// Deprecated: use providerusage.FormatWithOptions.
func FormatProviderUsageWithOptions(report *ProviderUsage, now time.Time, opts ProviderUsageFormatOptions) string {
	return providerusage.FormatWithOptions(report, now, opts)
}
