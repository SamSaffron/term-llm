package usage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenRouterOptions selects an inclusive/exclusive UTC period. APIKey is resolved
// lazily; successful account analytics never depends on that optional credential.
type OpenRouterOptions struct {
	ManagementKey      string
	APIKey             func() (string, error)
	CredentialWarnings []string
	Since, Until, Now  time.Time
}

type OpenRouterReport struct {
	SchemaVersion int                    `json:"schema_version"`
	Provider      string                 `json:"provider"`
	Source        string                 `json:"source"`
	Scope         string                 `json:"scope"`
	Status        string                 `json:"status"`
	GeneratedAt   time.Time              `json:"generated_at"`
	Period        OpenRouterPeriod       `json:"period"`
	Currency      string                 `json:"currency"`
	CachedAt      *time.Time             `json:"cached_at"`
	Balance       *OpenRouterBalance     `json:"balance"`
	KeyUsage      *OpenRouterKeyUsage    `json:"key_usage"`
	Totals        *OpenRouterAmounts     `json:"totals"`
	Models        []OpenRouterModelUsage `json:"models"`
	Daily         []OpenRouterDailyUsage `json:"daily"`
	Warnings      []string               `json:"warnings"`
}
type OpenRouterPeriod struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Timezone string    `json:"timezone"`
}
type OpenRouterBalance struct {
	TotalCredits float64 `json:"total_credits_usd"`
	TotalUsage   float64 `json:"total_usage_usd"`
	Remaining    float64 `json:"remaining_usd"`
}
type OpenRouterKeyUsage struct {
	Scope            string   `json:"scope"`
	Usage            float64  `json:"usage_usd"`
	UsageDaily       float64  `json:"usage_daily_usd"`
	UsageWeekly      float64  `json:"usage_weekly_usd"`
	UsageMonthly     float64  `json:"usage_monthly_usd"`
	BYOKUsage        *float64 `json:"byok_usage_usd"`
	BYOKUsageDaily   *float64 `json:"byok_usage_daily_usd"`
	BYOKUsageWeekly  *float64 `json:"byok_usage_weekly_usd"`
	BYOKUsageMonthly *float64 `json:"byok_usage_monthly_usd"`
	Limit            *float64 `json:"limit_usd"`
	LimitRemaining   *float64 `json:"limit_remaining_usd"`
	LimitReset       *string  `json:"limit_reset"`
}
type OpenRouterAmounts struct {
	Requests        int64    `json:"requests"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	CachedTokens    int64    `json:"cached_tokens"`
	ReasoningTokens int64    `json:"reasoning_tokens"`
	CacheHitPercent *float64 `json:"cache_hit_percent"`
	CostUSD         float64  `json:"cost_usd"`
	BYOKUsageUSD    float64  `json:"byok_usage_usd"`
	CreditsUsageUSD float64  `json:"credits_usage_usd"`
	BYOKFeesUSD     float64  `json:"byok_fees_usd"`
}
type OpenRouterModelUsage struct {
	Model string `json:"model"`
	OpenRouterAmounts
}
type OpenRouterDailyUsage struct {
	Date string `json:"date"`
	OpenRouterAmounts
}

// FetchOpenRouterUsage queries only the fixed OpenRouter API origin. It never
// reads configuration or credentials itself and never follows redirects.
func FetchOpenRouterUsage(ctx context.Context, options OpenRouterOptions) (*OpenRouterReport, error) {
	return fetchOpenRouterUsage(ctx, options, &http.Client{Timeout: 20 * time.Second}, "https://openrouter.ai/api/v1")
}

func fetchOpenRouterUsage(ctx context.Context, o OpenRouterOptions, client *http.Client, base string) (*OpenRouterReport, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Since.IsZero() || !o.Since.Before(o.Until) || o.Until.After(o.Now) {
		return nil, fmt.Errorf("OpenRouter usage requires a valid non-future time range")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if safeClient.Timeout <= 0 || safeClient.Timeout > 20*time.Second {
		safeClient.Timeout = 20 * time.Second
	}
	c := openRouterClient{client: &safeClient, base: base}
	r := &OpenRouterReport{SchemaVersion: 1, Provider: "openrouter", Source: "openrouter_summary", Scope: "account", Status: "summary_only", GeneratedAt: o.Now.UTC(), Period: OpenRouterPeriod{o.Since.UTC(), o.Until.UTC(), "UTC"}, Currency: "USD", Models: []OpenRouterModelUsage{}, Daily: []OpenRouterDailyUsage{}, Warnings: append([]string{}, o.CredentialWarnings...)}
	management := strings.TrimSpace(o.ManagementKey)
	if management != "" {
		if err := c.analytics(ctx, management, o, r); err != nil {
			r.Warnings = append(r.Warnings, "Account analytics unavailable: "+err.Error())
		} else {
			r.Source = "openrouter_analytics"
			r.Status = "complete"
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	regular := resolveOpenRouterKey(o, r)
	if regular != "" {
		key, err := c.key(ctx, regular)
		if err != nil {
			r.Warnings = append(r.Warnings, "Key summary unavailable: "+err.Error())
		} else if key == nil {
			r.Warnings = append(r.Warnings, "Management-key counters are not account usage and have been omitted.")
		} else {
			r.KeyUsage = key
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	creditKey := management
	if creditKey == "" {
		creditKey = regular
	}
	if creditKey != "" {
		balance, err := c.credits(ctx, creditKey)
		if err != nil && regular != "" && regular != creditKey && ctx.Err() == nil {
			balance, err = c.credits(ctx, regular)
		}
		if err != nil {
			r.Warnings = append(r.Warnings, "Account balance unavailable: "+err.Error())
		} else {
			r.Balance = balance
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Status != "complete" {
		r.Warnings = append(r.Warnings, "Requested-period model and daily usage is unavailable. Configure an OpenRouter management_key for account analytics; key counters are this key's rolling counters, not requested-period totals.")
	}
	if r.Totals == nil && r.KeyUsage == nil && r.Balance == nil {
		return nil, fmt.Errorf("OpenRouter usage unavailable; verify management_key or api_key credentials and API access (%s)", strings.Join(r.Warnings, "; "))
	}
	return r, nil
}

func resolveOpenRouterKey(o OpenRouterOptions, r *OpenRouterReport) string {
	if o.APIKey == nil {
		return ""
	}
	key, err := o.APIKey()
	if err != nil {
		r.Warnings = append(r.Warnings, "Optional OpenRouter API key could not be resolved.")
		return ""
	}
	return strings.TrimSpace(key)
}
