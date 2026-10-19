package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/formatutil"
	usagedata "github.com/samsaffron/term-llm/internal/usage"
	"github.com/spf13/cobra"
)

var fetchOpenRouterUsage = usagedata.FetchOpenRouterUsage
var openRouterUsageNow = time.Now

func usageFlagChanged(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Changed(name)
}

func (o *Options) openRouterUsagePeriod(cmd *cobra.Command, now time.Time) (time.Time, time.Time, error) {
	if o.Days < 1 || o.Days > 3660 {
		return time.Time{}, time.Time{}, fmt.Errorf("--days must be between 1 and 3660")
	}
	if usageFlagChanged(cmd, "days") && (o.Since != "" || o.Until != "" || usageFlagChanged(cmd, "since") || usageFlagChanged(cmd, "until")) {
		return time.Time{}, time.Time{}, fmt.Errorf("--days cannot be combined with --since or --until")
	}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	last := today
	var err error
	if o.Until != "" || usageFlagChanged(cmd, "until") {
		last, err = time.Parse("20060102", o.Until)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --until date (expected YYYYMMDD)")
		}
	}
	since := last.AddDate(0, 0, 1-o.Days)
	if o.Since != "" || usageFlagChanged(cmd, "since") {
		since, err = time.Parse("20060102", o.Since)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --since date (expected YYYYMMDD)")
		}
	}
	if last.After(today) || since.After(today) {
		return time.Time{}, time.Time{}, fmt.Errorf("OpenRouter usage dates cannot be in the future (UTC)")
	}
	if since.After(last) {
		return time.Time{}, time.Time{}, fmt.Errorf("--since must not be after --until")
	}
	until := last.AddDate(0, 0, 1)
	if until.After(now) {
		until = now
	}
	if !since.Before(until) {
		return time.Time{}, time.Time{}, fmt.Errorf("OpenRouter usage period is empty at UTC midnight; use --days 2 or an earlier --since date")
	}
	return since, until, nil
}

func (o *Options) runOpenRouterUsage(cmd *cobra.Command) error {
	if o.IncludeExternal || usageFlagChanged(cmd, "include-external") {
		return fmt.Errorf("OpenRouter usage does not support --include-external")
	}
	if copilotUsageFlagsChanged(cmd) {
		return fmt.Errorf("Copilot usage flags require --provider copilot")
	}
	now := openRouterUsageNow().UTC()
	since, until, err := o.openRouterUsagePeriod(cmd, now)
	if err != nil {
		return err
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	opts, err := openRouterUsageCredentials()
	if err != nil {
		return err
	}
	opts.Since, opts.Until, opts.Now = since, until, now
	report, err := fetchOpenRouterUsage(cmd.Context(), opts)
	if err != nil {
		return err
	} // Backend errors are credential-safe and preserve cancellation.
	if report == nil {
		return fmt.Errorf("OpenRouter usage returned no report")
	}
	if o.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return writeOpenRouterUsageText(cmd.OutOrStdout(), report, o.Daily)
}

func openRouterUsageCredentials() (usagedata.OpenRouterOptions, error) {
	opts := usagedata.OpenRouterOptions{}
	cfg, err := loadProviderUsageConfig()
	if err != nil {
		return opts, fmt.Errorf("cannot load configuration for OpenRouter usage; check your configuration")
	}
	p := config.ProviderConfig{}
	if cfg != nil {
		if raw := cfg.GetProviderConfig("openrouter"); raw != nil {
			p = *raw
		}
	}
	management := p.ManagementKey
	if strings.TrimSpace(management) == "" {
		management = os.Getenv("OPENROUTER_MANAGEMENT_KEY")
	}
	opts.ManagementKey, err = resolveOpenRouterUsageCredential(management, "management key")
	if err != nil {
		opts.CredentialWarnings = append(opts.CredentialWarnings, err.Error())
	}
	opts.APIKey = func() (string, error) {
		raw := p.APIKey
		if strings.TrimSpace(raw) == "" {
			if strings.TrimSpace(p.ResolvedAPIKey) != "" {
				return strings.TrimSpace(p.ResolvedAPIKey), nil
			}
			raw = os.Getenv("OPENROUTER_API_KEY")
		}
		return resolveOpenRouterUsageCredential(raw, "API key")
	}
	return opts, nil
}

func resolveOpenRouterUsageCredential(raw, label string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	value, err := config.ResolveValue(raw)
	if err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("cannot resolve OpenRouter %s; check its configured reference", label)
	}
	return strings.TrimSpace(value), nil
}

// Remove all control/format characters from remote labels, including ANSI ESC,
// newlines and bidi overrides. JSON encoding separately escapes control bytes.
func openRouterUsageLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

func writeOpenRouterUsageText(w io.Writer, r *usagedata.OpenRouterReport, daily bool) error {
	if r == nil {
		return fmt.Errorf("nil OpenRouter usage report")
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, "OpenRouter Usage (USD)")
	fmt.Fprintf(&b, "Period: %s inclusive to %s exclusive (UTC)\n", r.Period.Since.UTC().Format(time.RFC3339), r.Period.Until.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Source: %s | Scope: %s | Status: %s\n", openRouterUsageLabel(r.Source), openRouterUsageLabel(r.Scope), openRouterUsageLabel(r.Status))
	if r.Balance != nil {
		fmt.Fprintf(&b, "Account balance: $%.6f remaining (credits $%.6f; usage $%.6f)\n", r.Balance.Remaining, r.Balance.TotalCredits, r.Balance.TotalUsage)
	}
	if r.KeyUsage != nil {
		k := r.KeyUsage
		fmt.Fprintf(&b, "This key only (not requested-period totals): lifetime $%.6f; daily $%.6f; weekly $%.6f; monthly $%.6f\n", k.Usage, k.UsageDaily, k.UsageWeekly, k.UsageMonthly)
		if k.LimitRemaining != nil {
			fmt.Fprintf(&b, "This key remaining limit: $%.6f\n", *k.LimitRemaining)
		}
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", openRouterUsageLabel(warning))
	}
	if r.Totals == nil {
		fmt.Fprintln(&b, "\nRequested-period model/daily breakdown unavailable. Configure providers.openrouter.management_key or OPENROUTER_MANAGEMENT_KEY for account analytics.")
	} else {
		writeOpenRouterAmountsTable(&b, r, daily)
	}
	n, err := w.Write(b.Bytes())
	if err == nil && n != b.Len() {
		return io.ErrShortWrite
	}
	return err
}

func writeOpenRouterAmountsTable(w io.Writer, r *usagedata.OpenRouterReport, daily bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	label := "MODEL"
	if daily {
		label = "UTC DAY"
	}
	fmt.Fprintf(tw, "\n%s\tREQUESTS\tINPUT\tOUTPUT\tCACHE %%\tCOST (USD)\n", label)
	row := func(name string, a usagedata.OpenRouterAmounts) {
		cache := "—"
		if a.CacheHitPercent != nil {
			cache = fmt.Sprintf("%.1f%%", *a.CacheHitPercent)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t$%.6f\n", openRouterUsageLabel(name), formatutil.FormatTokens(a.Requests), formatutil.FormatTokens(a.InputTokens), formatutil.FormatTokens(a.OutputTokens), cache, a.CostUSD)
	}
	if daily {
		for _, d := range r.Daily {
			row(d.Date, d.OpenRouterAmounts)
		}
	} else {
		for _, m := range r.Models {
			row(m.Model, m.OpenRouterAmounts)
		}
	}
	row("TOTAL", *r.Totals)
	_ = tw.Flush() // Only called with the in-memory buffer above.
	if len(r.Models) == 0 && len(r.Daily) == 0 {
		fmt.Fprintln(w, "No activity returned for this period.")
	}
}
