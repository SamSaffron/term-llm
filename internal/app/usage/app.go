package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	githubcopilot "github.com/samsaffron/term-llm/internal/copilot"
	"github.com/samsaffron/term-llm/internal/llm/providerusage"
	"github.com/samsaffron/term-llm/internal/ui"
	usagedata "github.com/samsaffron/term-llm/internal/usage"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Options holds the per-command flag state for the usage command.
type Options struct {
	Provider          string
	Since             string
	Until             string
	Days              int
	Daily             bool
	JSON              bool
	Breakdown         bool
	IncludeExternal   bool
	CopilotScope      string
	CopilotEntity     string
	CopilotYear       int
	CopilotMonth      int
	CopilotDay        int
	CopilotModel      string
	CopilotProduct    string
	CopilotUser       string
	CopilotOrg        string
	CopilotEnterprise string
	CopilotCostCenter string
}

// NewOptions returns Options populated with the command's flag defaults.
func NewOptions() *Options {
	return &Options{Days: 30, CopilotScope: "user"}
}

// Run executes the usage command.
func (o *Options) Run(cmd *cobra.Command, args []string) error {
	if err := o.validateUsageProvider(); err != nil {
		return err
	}
	if o.Provider != "openrouter" && (usageFlagChanged(cmd, "days") || usageFlagChanged(cmd, "daily") || o.Daily || o.Days != 30) {
		return fmt.Errorf("--days and --daily require --provider openrouter")
	}
	if o.Provider == "openrouter" {
		return o.runOpenRouterUsage(cmd)
	}
	// Subscription providers report live limits without consuming model tokens.
	if o.Provider == "agy-bin" {
		if err := validateAgyBinUsageFlags(cmd); err != nil {
			return err
		}
		return runAgyBinUsage(cmd.Context(), cmd.OutOrStdout(), o.JSON)
	}
	if o.Provider == "claude-bin" {
		if err := validateClaudeBinUsageFlags(cmd); err != nil {
			return err
		}
		return runClaudeBinUsage(cmd.Context(), cmd.OutOrStdout(), o.JSON)
	}
	if o.Provider == "cursor-bin" {
		if err := validateCursorBinUsageFlags(cmd); err != nil {
			return err
		}
		return runCursorBinUsage(cmd.Context(), cmd.OutOrStdout(), o.JSON)
	}
	if o.Provider == "chatgpt" || o.Provider == "grok" || o.Provider == "opencode-go" {
		return o.runDirectProviderUsage(cmd, o.Provider)
	}
	// Special handling for copilot - fetch latest AI Credit usage from GitHub's billing API
	if o.Provider == "copilot" {
		return o.runCopilotUsage()
	}
	if copilotUsageFlagsChanged(cmd) {
		return fmt.Errorf("Copilot usage flags require --provider copilot")
	}

	// Load all usage data
	result := usagedata.LoadAllUsage()

	// Report missing directories (only if verbose or no data)
	if len(result.Entries) == 0 && len(result.MissingDirectories) > 0 {
		fmt.Fprintln(os.Stderr, "No usage data found. Checked directories:")
		for _, dir := range result.MissingDirectories {
			fmt.Fprintf(os.Stderr, "  - %s (not found)\n", dir)
		}
		return nil
	}

	// Parse date filters
	since, until := usagedata.DefaultDateRange()

	if o.Since != "" {
		t, err := usagedata.ParseDateYYYYMMDD(o.Since)
		if err != nil {
			return fmt.Errorf("invalid --since date (expected YYYYMMDD): %w", err)
		}
		since = t
	}

	if o.Until != "" {
		t, err := usagedata.ParseDateYYYYMMDD(o.Until)
		if err != nil {
			return fmt.Errorf("invalid --until date (expected YYYYMMDD): %w", err)
		}
		// Set to end of day
		until = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
	}

	// Map provider flag to constant
	providerFilter := ""
	switch o.Provider {
	case "claude-code", "claude":
		providerFilter = usagedata.ProviderClaudeCode
	case "term-llm":
		providerFilter = usagedata.ProviderTermLLM
	case "", "all":
		providerFilter = ""
	}

	// Filter entries
	filtered := result.Filter(usagedata.FilterOptions{
		Since:           since,
		Until:           until,
		Provider:        providerFilter,
		IncludeExternal: o.IncludeExternal,
	})

	if len(filtered) == 0 {
		if o.JSON {
			fmt.Println("{\"daily\": [], \"totals\": {}}")
		} else {
			fmt.Println("No usage data found for the specified date range.")
		}
		return nil
	}

	// Calculate costs for entries without pre-calculated costs
	fetcher := usagedata.NewPricingFetcher()
	filtered = usagedata.CalculateCosts(filtered, fetcher)

	// Aggregate by day
	daily := usagedata.AggregateDaily(filtered)
	totals := usagedata.CalculateTotals(daily)

	// Auto-enable breakdown when filtering to term-llm (model info is more useful than "term-llm")
	if providerFilter == usagedata.ProviderTermLLM && !o.Breakdown {
		o.Breakdown = true
	}

	if o.JSON {
		return o.outputJSON(daily, totals)
	}

	return o.outputTable(daily, totals, since, until)
}

func (o *Options) validateUsageProvider() error {
	switch o.Provider {
	case "", "all", "claude", "claude-code", "term-llm", "agy-bin", "claude-bin", "cursor-bin", "chatgpt", "grok", "opencode-go", "copilot", "openrouter":
		return nil
	default:
		return fmt.Errorf("unknown usage provider (use agy-bin, chatgpt, grok, claude-bin, cursor-bin, opencode-go, openrouter, claude-code, copilot, term-llm, or all)")
	}
}

func copilotUsageFlagsChanged(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	for _, name := range []string{
		"copilot-scope",
		"copilot-entity",
		"year",
		"month",
		"day",
		"copilot-model",
		"copilot-product",
		"copilot-user",
		"copilot-org",
		"copilot-enterprise",
		"copilot-cost-center",
	} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			return true
		}
	}
	return false
}

var (
	fetchDirectProviderUsage = providerusage.FetchWithAPIKey
	loadProviderUsageConfig  = config.Load
)

func (o *Options) runDirectProviderUsage(cmd *cobra.Command, provider string) error {
	if o.Since != "" || o.Until != "" || o.Breakdown || o.IncludeExternal {
		return fmt.Errorf("live %s usage does not support date, breakdown, or external-usage filters", provider)
	}
	if copilotUsageFlagsChanged(cmd) {
		return fmt.Errorf("Copilot usage flags require --provider copilot")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	apiKey, err := resolvedProviderUsageAPIKey(provider)
	if err != nil {
		return err
	}
	report, err := fetchDirectProviderUsage(ctx, provider, apiKey)
	if err != nil {
		return fmt.Errorf("failed to fetch %s usage: %w", provider, err)
	}
	if o.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	out := cmd.OutOrStdout()
	opts := providerUsageFormatOptions(out)
	fmt.Fprintln(out, providerusage.FormatWithOptions(report, time.Now(), opts))
	return nil
}

func resolvedProviderUsageAPIKey(provider string) (string, error) {
	if provider != "opencode-go" {
		return "", nil
	}
	cfg, err := loadProviderUsageConfig()
	if err != nil {
		return "", fmt.Errorf("load config for %s usage: %w", provider, err)
	}
	if cfg == nil {
		return "", nil
	}
	providerCfg, err := cfg.GetResolvedProviderConfig(provider)
	if err != nil {
		return "", fmt.Errorf("resolve %s credentials: %w", provider, err)
	}
	if providerCfg == nil {
		return "", nil
	}
	if err := providerCfg.ResolveForInference(); err != nil {
		return "", fmt.Errorf("resolve %s API key: %w", provider, err)
	}
	return strings.TrimSpace(providerCfg.ResolvedAPIKey), nil
}

func providerUsageFormatOptions(w io.Writer) providerusage.FormatOptions {
	opts := providerusage.FormatOptions{ASCII: strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")}
	fdWriter, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(fdWriter.Fd())) {
		return opts
	}
	if width, _, err := term.GetSize(int(fdWriter.Fd())); err == nil && width > 0 {
		opts.Width = width
	}
	return opts
}

// jsonOutput represents the JSON output format
type jsonOutput struct {
	Daily  []jsonDailyUsage `json:"daily"`
	Totals jsonTotals       `json:"totals"`
}

type jsonDailyUsage struct {
	Date             string               `json:"date"`
	InputTokens      int                  `json:"inputTokens"`
	OutputTokens     int                  `json:"outputTokens"`
	CacheWriteTokens int                  `json:"cacheWriteTokens"`
	CacheReadTokens  int                  `json:"cacheReadTokens"`
	ReasoningTokens  int                  `json:"reasoningTokens"`
	TotalTokens      int                  `json:"totalTokens"`
	TotalCost        float64              `json:"totalCost"`
	ModelsUsed       []string             `json:"modelsUsed"`
	Breakdown        []jsonModelBreakdown `json:"breakdown,omitempty"`
}

type jsonModelBreakdown struct {
	Model            string  `json:"model"`
	InputTokens      int     `json:"inputTokens"`
	OutputTokens     int     `json:"outputTokens"`
	CacheWriteTokens int     `json:"cacheWriteTokens"`
	CacheReadTokens  int     `json:"cacheReadTokens"`
	Cost             float64 `json:"cost"`
}

type jsonTotals struct {
	InputTokens      int      `json:"inputTokens"`
	OutputTokens     int      `json:"outputTokens"`
	CacheWriteTokens int      `json:"cacheWriteTokens"`
	CacheReadTokens  int      `json:"cacheReadTokens"`
	ReasoningTokens  int      `json:"reasoningTokens"`
	TotalTokens      int      `json:"totalTokens"`
	TotalCost        float64  `json:"totalCost"`
	ModelsUsed       []string `json:"modelsUsed"`
}

func (o *Options) outputJSON(daily []usagedata.DailyUsage, totals usagedata.DailyUsage) error {
	output := jsonOutput{
		Daily: make([]jsonDailyUsage, len(daily)),
		Totals: jsonTotals{
			InputTokens:      totals.InputTokens,
			OutputTokens:     totals.OutputTokens,
			CacheWriteTokens: totals.CacheWriteTokens,
			CacheReadTokens:  totals.CacheReadTokens,
			ReasoningTokens:  totals.ReasoningTokens,
			TotalTokens:      totals.TotalTokens(),
			TotalCost:        totals.TotalCost,
			ModelsUsed:       totals.ModelsUsed,
		},
	}

	for i, d := range daily {
		jd := jsonDailyUsage{
			Date:             d.Date,
			InputTokens:      d.InputTokens,
			OutputTokens:     d.OutputTokens,
			CacheWriteTokens: d.CacheWriteTokens,
			CacheReadTokens:  d.CacheReadTokens,
			ReasoningTokens:  d.ReasoningTokens,
			TotalTokens:      d.TotalTokens(),
			TotalCost:        d.TotalCost,
			ModelsUsed:       d.ModelsUsed,
		}

		if o.Breakdown {
			breakdown := usagedata.GetModelBreakdown(d.Entries)
			jd.Breakdown = make([]jsonModelBreakdown, len(breakdown))
			for j, mb := range breakdown {
				jd.Breakdown[j] = jsonModelBreakdown{
					Model:            mb.Model,
					InputTokens:      mb.InputTokens,
					OutputTokens:     mb.OutputTokens,
					CacheWriteTokens: mb.CacheWriteTokens,
					CacheReadTokens:  mb.CacheReadTokens,
					Cost:             mb.Cost,
				}
			}
		}

		output.Daily[i] = jd
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func (o *Options) outputTable(daily []usagedata.DailyUsage, totals usagedata.DailyUsage, since, until time.Time) error {
	fmt.Printf("Usage from %s to %s\n\n", since.Format("2006-01-02"), until.Format("2006-01-02"))

	// Create tabwriter for aligned output
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)

	// Header
	fmt.Fprintf(w, "Date\t Input\t Output\t Cache Write\t Cache Read\t Cost\t\n")
	fmt.Fprintf(w, "────\t ─────\t ──────\t ───────────\t ──────────\t ────\t\n")

	// Daily rows
	for _, d := range daily {
		providerBreakdown := usagedata.GetProviderBreakdown(d.Entries)

		if o.Breakdown {
			// Model breakdown mode: show date row, then models
			fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
				d.Date,
				ui.FormatTokenCount(d.InputTokens),
				ui.FormatTokenCount(d.OutputTokens),
				ui.FormatTokenCount(d.CacheWriteTokens),
				ui.FormatTokenCount(d.CacheReadTokens),
				formatCost(d.TotalCost))
			breakdown := usagedata.GetModelBreakdown(d.Entries)
			for _, mb := range breakdown {
				fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
					shortenModelName(mb.Model),
					ui.FormatTokenCount(mb.InputTokens),
					ui.FormatTokenCount(mb.OutputTokens),
					ui.FormatTokenCount(mb.CacheWriteTokens),
					ui.FormatTokenCount(mb.CacheReadTokens),
					formatCost(mb.Cost))
			}
		} else if len(providerBreakdown) == 1 {
			// Single provider: date row, then provider row with numbers
			pb := providerBreakdown[0]
			fmt.Fprintf(w, "%s\t\t\t\t\t\t\n", d.Date)
			fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
				formatProviderName(pb.Provider),
				ui.FormatTokenCount(pb.InputTokens),
				ui.FormatTokenCount(pb.OutputTokens),
				ui.FormatTokenCount(pb.CacheWriteTokens),
				ui.FormatTokenCount(pb.CacheReadTokens),
				formatCost(pb.Cost))
		} else {
			// Multiple providers: show date totals, then provider breakdown
			fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
				d.Date,
				ui.FormatTokenCount(d.InputTokens),
				ui.FormatTokenCount(d.OutputTokens),
				ui.FormatTokenCount(d.CacheWriteTokens),
				ui.FormatTokenCount(d.CacheReadTokens),
				formatCost(d.TotalCost))
			for _, pb := range providerBreakdown {
				fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
					formatProviderName(pb.Provider),
					ui.FormatTokenCount(pb.InputTokens),
					ui.FormatTokenCount(pb.OutputTokens),
					ui.FormatTokenCount(pb.CacheWriteTokens),
					ui.FormatTokenCount(pb.CacheReadTokens),
					formatCost(pb.Cost))
			}
		}
	}

	// Separator before totals
	fmt.Fprintf(w, "────\t ─────\t ──────\t ───────────\t ──────────\t ────\t\n")

	// Provider totals first
	providerTotals := usagedata.GetProviderBreakdown(totals.Entries)
	for _, pb := range providerTotals {
		fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
			formatProviderName(pb.Provider),
			ui.FormatTokenCount(pb.InputTokens),
			ui.FormatTokenCount(pb.OutputTokens),
			ui.FormatTokenCount(pb.CacheWriteTokens),
			ui.FormatTokenCount(pb.CacheReadTokens),
			formatCost(pb.Cost))
	}

	// Grand total last
	fmt.Fprintf(w, "Total\t %s\t %s\t %s\t %s\t %s\t\n",
		ui.FormatTokenCount(totals.InputTokens),
		ui.FormatTokenCount(totals.OutputTokens),
		ui.FormatTokenCount(totals.CacheWriteTokens),
		ui.FormatTokenCount(totals.CacheReadTokens),
		formatCost(totals.TotalCost))

	return w.Flush()
}

// formatCost formats a cost in USD
func formatCost(cost float64) string {
	if cost == 0 {
		return "$0.00"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// shortenModelName shortens common model names
func shortenModelName(name string) string {
	// Remove common prefixes
	name = strings.TrimPrefix(name, "anthropic/")
	name = strings.TrimPrefix(name, "openai/")
	name = strings.TrimPrefix(name, "google/")

	// Shorten common model names
	if strings.HasPrefix(name, "claude-") {
		// claude-4-sonnet-20250514 -> claude-4-sonnet
		parts := strings.Split(name, "-")
		if len(parts) >= 3 {
			// Check if last part looks like a date
			if len(parts[len(parts)-1]) == 8 {
				name = strings.Join(parts[:len(parts)-1], "-")
			}
		}
	}
	if strings.HasPrefix(name, "gpt-") {
		// Keep as-is for GPT models
	}
	if strings.HasPrefix(name, "gemini-") {
		// gemini-3-flash-preview -> gemini-3-flash
		name = strings.TrimSuffix(name, "-preview")
	}

	return name
}

// formatProviderName returns a human-friendly provider name
func formatProviderName(provider string) string {
	switch provider {
	case usagedata.ProviderClaudeCode:
		return "Claude Code"
	case usagedata.ProviderTermLLM:
		return "term-llm"
	default:
		return provider
	}
}

// runCopilotUsage fetches and displays latest GitHub Copilot AI Credit usage.
func (o *Options) runCopilotUsage() error {
	if o.Since != "" || o.Until != "" {
		return fmt.Errorf("Copilot AI Credit usage uses --year, --month, and --day filters instead of --since/--until")
	}

	scope, err := o.parseCopilotUsageScope(o.CopilotScope)
	if err != nil {
		return err
	}
	filters, err := o.copilotUsageFiltersFromFlags(scope)
	if err != nil {
		return err
	}
	entity := o.copilotUsageEntityFromFlags(scope)

	client, err := githubcopilot.NewUsageClientFromEnv()
	if err != nil {
		if errors.Is(err, githubcopilot.ErrNoGitHubToken) {
			return fmt.Errorf("GitHub Copilot AI Credit usage requires a GitHub token with billing permissions. " +
				"Set GITHUB_TOKEN or GH_TOKEN and try again. Personal usage requires user Plan: read; " +
				"organization usage requires organization Administration: read; enterprise usage requires enterprise admin or billing manager access")
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := client.GetAICreditUsage(ctx, scope, entity, filters)
	if err != nil {
		return fmt.Errorf("failed to fetch Copilot AI Credit usage: %w", err)
	}

	if o.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	return writeCopilotUsageText(os.Stdout, report)
}

func (o *Options) parseCopilotUsageScope(raw string) (githubcopilot.Scope, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "user", "personal":
		return githubcopilot.ScopeUser, nil
	case "org", "organization":
		return githubcopilot.ScopeOrganization, nil
	case "enterprise":
		return githubcopilot.ScopeEnterprise, nil
	default:
		return "", fmt.Errorf("unknown Copilot usage scope %q (use user, org, or enterprise)", raw)
	}
}

func (o *Options) copilotUsageEntityFromFlags(scope githubcopilot.Scope) string {
	if strings.TrimSpace(o.CopilotEntity) != "" {
		return o.CopilotEntity
	}
	switch scope {
	case githubcopilot.ScopeUser:
		return o.CopilotUser
	case githubcopilot.ScopeOrganization:
		return o.CopilotOrg
	case githubcopilot.ScopeEnterprise:
		return o.CopilotEnterprise
	default:
		return ""
	}
}

func (o *Options) copilotUsageFiltersFromFlags(scope githubcopilot.Scope) (githubcopilot.UsageFilters, error) {
	if o.CopilotYear < 0 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --year %d", o.CopilotYear)
	}
	if o.CopilotMonth < 0 || o.CopilotMonth > 12 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --month %d (expected 1-12)", o.CopilotMonth)
	}
	if o.CopilotDay < 0 || o.CopilotDay > 31 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --day %d (expected 1-31)", o.CopilotDay)
	}
	filterUser := o.CopilotUser
	filterOrg := o.CopilotOrg
	if scope == githubcopilot.ScopeUser {
		filterUser = ""
	}
	if scope == githubcopilot.ScopeOrganization {
		filterOrg = ""
	}
	return githubcopilot.UsageFilters{
		Year:         o.CopilotYear,
		Month:        o.CopilotMonth,
		Day:          o.CopilotDay,
		User:         filterUser,
		Organization: filterOrg,
		Model:        o.CopilotModel,
		Product:      o.CopilotProduct,
		CostCenterID: o.CopilotCostCenter,
	}, nil
}

func writeCopilotUsageText(w io.Writer, report *githubcopilot.UsageReport) error {
	if report == nil {
		return fmt.Errorf("nil Copilot usage report")
	}

	fmt.Fprintln(w, "GitHub Copilot Usage - AI Credits")
	fmt.Fprintf(w, "Scope: %s/%s\n", report.Scope, report.Entity)
	fmt.Fprintf(w, "Period: %s\n", formatCopilotPeriod(report.TimePeriod))
	fmt.Fprintln(w, "Source: GitHub Billing API")

	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}

	if len(report.Items) == 0 {
		fmt.Fprintln(w, "\nNo AI Credit usage found for this period.")
		return nil
	}

	fmt.Fprintln(w, "\nTotal")
	fmt.Fprintf(w, "  Net credits:       %s\n", formatCopilotCredits(report.Totals.NetCredits))
	fmt.Fprintf(w, "  Gross credits:     %s\n", formatCopilotCredits(report.Totals.GrossCredits))
	fmt.Fprintf(w, "  Net cost:          %s\n", formatCost(report.Totals.NetAmountUSD))
	fmt.Fprintf(w, "  Discounts:         %s\n", formatCost(report.Totals.DiscountAmountUSD))

	writeCopilotBreakdown(w, "By model", aggregateCopilotUsage(report.Items, func(item githubcopilot.UsageItem) string {
		return item.Model
	}))
	writeCopilotBreakdown(w, "By product", aggregateCopilotUsage(report.Items, func(item githubcopilot.UsageItem) string {
		return item.Product
	}))

	return nil
}

func formatCopilotPeriod(period githubcopilot.TimePeriod) string {
	if period.Year == 0 {
		return "current billing period"
	}
	if period.Month == 0 {
		return fmt.Sprintf("%04d", period.Year)
	}
	monthName := time.Month(period.Month).String()
	if period.Day == 0 {
		return fmt.Sprintf("%s %04d", monthName, period.Year)
	}
	return fmt.Sprintf("%s %d, %04d", monthName, period.Day, period.Year)
}

func formatCopilotCredits(credits float64) string {
	return fmt.Sprintf("%.2f", credits)
}

type copilotUsageBreakdown struct {
	Name    string
	Credits float64
	Cost    float64
}

func aggregateCopilotUsage(items []githubcopilot.UsageItem, keyFunc func(githubcopilot.UsageItem) string) []copilotUsageBreakdown {
	byName := make(map[string]*copilotUsageBreakdown)
	for _, item := range items {
		name := strings.TrimSpace(keyFunc(item))
		if name == "" {
			name = "(unknown)"
		}
		entry := byName[name]
		if entry == nil {
			entry = &copilotUsageBreakdown{Name: name}
			byName[name] = entry
		}
		entry.Credits += item.NetQuantity
		entry.Cost += item.NetAmountUSD
	}

	breakdown := make([]copilotUsageBreakdown, 0, len(byName))
	for _, entry := range byName {
		breakdown = append(breakdown, *entry)
	}
	sort.Slice(breakdown, func(i, j int) bool {
		if breakdown[i].Credits == breakdown[j].Credits {
			return breakdown[i].Name < breakdown[j].Name
		}
		return breakdown[i].Credits > breakdown[j].Credits
	})
	return breakdown
}

func writeCopilotBreakdown(w io.Writer, title string, breakdown []copilotUsageBreakdown) {
	if len(breakdown) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tCREDITS\tCOST")
	for _, entry := range breakdown {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", entry.Name, formatCopilotCredits(entry.Credits), formatCost(entry.Cost))
	}
	_ = tw.Flush()
}
