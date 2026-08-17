package cmd

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
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/usage"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	usageProvider          string
	usageSince             string
	usageUntil             string
	usageJSON              bool
	usageBreakdown         bool
	usageIncludeExternal   bool
	usageCopilotScope      string
	usageCopilotEntity     string
	usageCopilotYear       int
	usageCopilotMonth      int
	usageCopilotDay        int
	usageCopilotModel      string
	usageCopilotProduct    string
	usageCopilotUser       string
	usageCopilotOrg        string
	usageCopilotEnterprise string
	usageCopilotCostCenter string
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show usage, costs, and subscription limits",
	Long: `Show token usage and costs from Claude Code, Codex CLI, Gemini CLI, and term-llm.

By default, this command reads local usage data stored by these CLI tools and
shows aggregated token counts and estimated costs. The chatgpt, claude-bin,
cursor-bin, and opencode-go providers instead fetch live subscription limits
without invoking a model or consuming tokens.

For GitHub Copilot, it fetches AI Credit usage from GitHub's latest
billing usage API. Set GITHUB_TOKEN or GH_TOKEN with billing permissions.

Examples:
  term-llm usage                              # show last 7 days
  term-llm usage --provider claude-code       # filter local Claude Code history
  term-llm usage --provider claude-bin        # show live Claude subscription limits
  term-llm usage --provider claude-bin --json # output the structured Claude response
  term-llm usage --provider cursor-bin        # show live Cursor plan usage
  term-llm usage --provider cursor-bin --json # output the structured Cursor response
  term-llm usage --provider chatgpt           # show live ChatGPT Codex plan usage
  term-llm usage --provider opencode-go       # show live OpenCode Go plan usage
  term-llm usage --provider copilot           # show personal GitHub Copilot AI Credit usage
  term-llm usage --provider copilot --copilot-scope org --copilot-entity my-org
  term-llm usage --provider term-llm          # show term-llm direct API usage
  term-llm usage --since 20250101             # from Jan 1, 2025
  term-llm usage --json                       # output as JSON
  term-llm usage --breakdown                  # show per-model breakdown`,
	RunE: runUsage,
}

func init() {
	rootCmd.AddCommand(usageCmd)
	usageCmd.Flags().StringVarP(&usageProvider, "provider", "p", "", "Filter by provider (chatgpt, claude-bin, cursor-bin, opencode-go, claude-code, copilot, gemini-cli, term-llm, or all)")
	usageCmd.Flags().StringVar(&usageSince, "since", "", "Start date (YYYYMMDD)")
	usageCmd.Flags().StringVar(&usageUntil, "until", "", "End date (YYYYMMDD)")
	usageCmd.Flags().BoolVar(&usageJSON, "json", false, "Output as JSON")
	usageCmd.Flags().BoolVar(&usageBreakdown, "breakdown", false, "Show per-model breakdown")
	usageCmd.Flags().BoolVar(&usageIncludeExternal, "include-external", false, "Include externally-tracked term-llm usage (claude-bin, codex, gemini-cli calls)")
	usageCmd.Flags().StringVar(&usageCopilotScope, "copilot-scope", "user", "Copilot billing scope (user, org, enterprise)")
	usageCmd.Flags().StringVar(&usageCopilotEntity, "copilot-entity", "", "Copilot billing entity (username, organization, or enterprise slug; defaults to authenticated user for user scope)")
	usageCmd.Flags().IntVar(&usageCopilotYear, "year", 0, "Copilot usage year (YYYY; defaults to GitHub API current year)")
	usageCmd.Flags().IntVar(&usageCopilotMonth, "month", 0, "Copilot usage month (1-12; defaults to GitHub API current month)")
	usageCmd.Flags().IntVar(&usageCopilotDay, "day", 0, "Copilot usage day (1-31)")
	usageCmd.Flags().StringVar(&usageCopilotModel, "copilot-model", "", "Filter Copilot AI Credit usage by model")
	usageCmd.Flags().StringVar(&usageCopilotProduct, "copilot-product", "", "Filter Copilot AI Credit usage by product")
	usageCmd.Flags().StringVar(&usageCopilotUser, "copilot-user", "", "GitHub username for user scope, or user filter for organization/enterprise scope")
	usageCmd.Flags().StringVar(&usageCopilotOrg, "copilot-org", "", "Organization for org scope, or organization filter for enterprise scope")
	usageCmd.Flags().StringVar(&usageCopilotEnterprise, "copilot-enterprise", "", "Enterprise slug for enterprise scope")
	usageCmd.Flags().StringVar(&usageCopilotCostCenter, "copilot-cost-center", "", "Filter enterprise Copilot usage by cost center ID")
	if err := usageCmd.RegisterFlagCompletionFunc("provider", UsageProviderFlagCompletion); err != nil {
		panic("failed to register provider completion: " + err.Error())
	}
	if err := usageCmd.RegisterFlagCompletionFunc("copilot-scope", CopilotScopeFlagCompletion); err != nil {
		panic("failed to register copilot scope completion: " + err.Error())
	}
}

func runUsage(cmd *cobra.Command, args []string) error {
	// Subscription providers report live limits without consuming model tokens.
	if usageProvider == "claude-bin" {
		if err := validateClaudeBinUsageFlags(cmd); err != nil {
			return err
		}
		return runClaudeBinUsage(cmd.Context(), cmd.OutOrStdout(), usageJSON)
	}
	if usageProvider == "cursor-bin" {
		if err := validateCursorBinUsageFlags(cmd); err != nil {
			return err
		}
		return runCursorBinUsage(cmd.Context(), cmd.OutOrStdout(), usageJSON)
	}
	if usageProvider == "chatgpt" || usageProvider == "opencode-go" {
		return runDirectProviderUsage(cmd, usageProvider)
	}
	// Special handling for copilot - fetch latest AI Credit usage from GitHub's billing API
	if usageProvider == "copilot" {
		return runCopilotUsage()
	}
	if copilotUsageFlagsChanged(cmd) {
		return fmt.Errorf("Copilot usage flags require --provider copilot")
	}

	// Load all usage data
	result := usage.LoadAllUsage()

	// Report missing directories (only if verbose or no data)
	if len(result.Entries) == 0 && len(result.MissingDirectories) > 0 {
		fmt.Fprintln(os.Stderr, "No usage data found. Checked directories:")
		for _, dir := range result.MissingDirectories {
			fmt.Fprintf(os.Stderr, "  - %s (not found)\n", dir)
		}
		return nil
	}

	// Parse date filters
	since, until := usage.DefaultDateRange()

	if usageSince != "" {
		t, err := usage.ParseDateYYYYMMDD(usageSince)
		if err != nil {
			return fmt.Errorf("invalid --since date (expected YYYYMMDD): %w", err)
		}
		since = t
	}

	if usageUntil != "" {
		t, err := usage.ParseDateYYYYMMDD(usageUntil)
		if err != nil {
			return fmt.Errorf("invalid --until date (expected YYYYMMDD): %w", err)
		}
		// Set to end of day
		until = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
	}

	// Map provider flag to constant
	providerFilter := ""
	switch usageProvider {
	case "claude-code", "claude":
		providerFilter = usage.ProviderClaudeCode
	case "gemini-cli", "gemini":
		providerFilter = usage.ProviderGeminiCLI
	case "term-llm":
		providerFilter = usage.ProviderTermLLM
	case "", "all":
		providerFilter = ""
	default:
		return fmt.Errorf("unknown provider: %s (use chatgpt, claude-bin, cursor-bin, opencode-go, claude-code, copilot, gemini-cli, or term-llm)", usageProvider)
	}

	// Filter entries
	filtered := result.Filter(usage.FilterOptions{
		Since:           since,
		Until:           until,
		Provider:        providerFilter,
		IncludeExternal: usageIncludeExternal,
	})

	if len(filtered) == 0 {
		if usageJSON {
			fmt.Println("{\"daily\": [], \"totals\": {}}")
		} else {
			fmt.Println("No usage data found for the specified date range.")
		}
		return nil
	}

	// Calculate costs for entries without pre-calculated costs
	fetcher := usage.NewPricingFetcher()
	filtered = usage.CalculateCosts(filtered, fetcher)

	// Aggregate by day
	daily := usage.AggregateDaily(filtered)
	totals := usage.CalculateTotals(daily)

	// Auto-enable breakdown when filtering to term-llm (model info is more useful than "term-llm")
	if providerFilter == usage.ProviderTermLLM && !usageBreakdown {
		usageBreakdown = true
	}

	if usageJSON {
		return outputJSON(daily, totals)
	}

	return outputTable(daily, totals, since, until)
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
	fetchDirectProviderUsage = llm.FetchProviderUsageWithAPIKey
	loadProviderUsageConfig  = config.Load
)

func runDirectProviderUsage(cmd *cobra.Command, provider string) error {
	if usageSince != "" || usageUntil != "" || usageBreakdown || usageIncludeExternal {
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
	if usageJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	out := cmd.OutOrStdout()
	opts := providerUsageFormatOptions(out)
	fmt.Fprintln(out, llm.FormatProviderUsageWithOptions(report, time.Now(), opts))
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

func providerUsageFormatOptions(w io.Writer) llm.ProviderUsageFormatOptions {
	opts := llm.ProviderUsageFormatOptions{ASCII: strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")}
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

func outputJSON(daily []usage.DailyUsage, totals usage.DailyUsage) error {
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

		if usageBreakdown {
			breakdown := usage.GetModelBreakdown(d.Entries)
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

func outputTable(daily []usage.DailyUsage, totals usage.DailyUsage, since, until time.Time) error {
	fmt.Printf("Usage from %s to %s\n\n", since.Format("2006-01-02"), until.Format("2006-01-02"))

	// Create tabwriter for aligned output
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)

	// Header
	fmt.Fprintf(w, "Date\t Input\t Output\t Cache Write\t Cache Read\t Cost\t\n")
	fmt.Fprintf(w, "────\t ─────\t ──────\t ───────────\t ──────────\t ────\t\n")

	// Daily rows
	for _, d := range daily {
		providerBreakdown := usage.GetProviderBreakdown(d.Entries)

		if usageBreakdown {
			// Model breakdown mode: show date row, then models
			fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
				d.Date,
				formatTokens(d.InputTokens),
				formatTokens(d.OutputTokens),
				formatTokens(d.CacheWriteTokens),
				formatTokens(d.CacheReadTokens),
				formatCost(d.TotalCost))
			breakdown := usage.GetModelBreakdown(d.Entries)
			for _, mb := range breakdown {
				fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
					shortenModelName(mb.Model),
					formatTokens(mb.InputTokens),
					formatTokens(mb.OutputTokens),
					formatTokens(mb.CacheWriteTokens),
					formatTokens(mb.CacheReadTokens),
					formatCost(mb.Cost))
			}
		} else if len(providerBreakdown) == 1 {
			// Single provider: date row, then provider row with numbers
			pb := providerBreakdown[0]
			fmt.Fprintf(w, "%s\t\t\t\t\t\t\n", d.Date)
			fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
				formatProviderName(pb.Provider),
				formatTokens(pb.InputTokens),
				formatTokens(pb.OutputTokens),
				formatTokens(pb.CacheWriteTokens),
				formatTokens(pb.CacheReadTokens),
				formatCost(pb.Cost))
		} else {
			// Multiple providers: show date totals, then provider breakdown
			fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
				d.Date,
				formatTokens(d.InputTokens),
				formatTokens(d.OutputTokens),
				formatTokens(d.CacheWriteTokens),
				formatTokens(d.CacheReadTokens),
				formatCost(d.TotalCost))
			for _, pb := range providerBreakdown {
				fmt.Fprintf(w, "  %s\t %s\t %s\t %s\t %s\t %s\t\n",
					formatProviderName(pb.Provider),
					formatTokens(pb.InputTokens),
					formatTokens(pb.OutputTokens),
					formatTokens(pb.CacheWriteTokens),
					formatTokens(pb.CacheReadTokens),
					formatCost(pb.Cost))
			}
		}
	}

	// Separator before totals
	fmt.Fprintf(w, "────\t ─────\t ──────\t ───────────\t ──────────\t ────\t\n")

	// Provider totals first
	providerTotals := usage.GetProviderBreakdown(totals.Entries)
	for _, pb := range providerTotals {
		fmt.Fprintf(w, "%s\t %s\t %s\t %s\t %s\t %s\t\n",
			formatProviderName(pb.Provider),
			formatTokens(pb.InputTokens),
			formatTokens(pb.OutputTokens),
			formatTokens(pb.CacheWriteTokens),
			formatTokens(pb.CacheReadTokens),
			formatCost(pb.Cost))
	}

	// Grand total last
	fmt.Fprintf(w, "Total\t %s\t %s\t %s\t %s\t %s\t\n",
		formatTokens(totals.InputTokens),
		formatTokens(totals.OutputTokens),
		formatTokens(totals.CacheWriteTokens),
		formatTokens(totals.CacheReadTokens),
		formatCost(totals.TotalCost))

	return w.Flush()
}

// formatTokens formats a token count in human-readable form (e.g., 1.5M, 384k)
func formatTokens(n int) string {
	if n == 0 {
		return "0"
	}
	if n >= 1_000_000 {
		val := float64(n) / 1_000_000
		if val >= 100 {
			return fmt.Sprintf("%.0fM", val)
		} else if val >= 10 {
			return fmt.Sprintf("%.1fM", val)
		}
		return fmt.Sprintf("%.2fM", val)
	}
	if n >= 1_000 {
		val := float64(n) / 1_000
		if val >= 100 {
			return fmt.Sprintf("%.0fk", val)
		} else if val >= 10 {
			return fmt.Sprintf("%.1fk", val)
		}
		return fmt.Sprintf("%.2fk", val)
	}
	return fmt.Sprintf("%d", n)
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
	case usage.ProviderClaudeCode:
		return "Claude Code"
	case usage.ProviderGeminiCLI:
		return "Gemini CLI"
	case usage.ProviderTermLLM:
		return "term-llm"
	default:
		return provider
	}
}

// runCopilotUsage fetches and displays latest GitHub Copilot AI Credit usage.
func runCopilotUsage() error {
	if usageSince != "" || usageUntil != "" {
		return fmt.Errorf("Copilot AI Credit usage uses --year, --month, and --day filters instead of --since/--until")
	}

	scope, err := parseCopilotUsageScope(usageCopilotScope)
	if err != nil {
		return err
	}
	filters, err := copilotUsageFiltersFromFlags(scope)
	if err != nil {
		return err
	}
	entity := copilotUsageEntityFromFlags(scope)

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

	if usageJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	return writeCopilotUsageText(os.Stdout, report)
}

func parseCopilotUsageScope(raw string) (githubcopilot.Scope, error) {
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

func copilotUsageEntityFromFlags(scope githubcopilot.Scope) string {
	if strings.TrimSpace(usageCopilotEntity) != "" {
		return usageCopilotEntity
	}
	switch scope {
	case githubcopilot.ScopeUser:
		return usageCopilotUser
	case githubcopilot.ScopeOrganization:
		return usageCopilotOrg
	case githubcopilot.ScopeEnterprise:
		return usageCopilotEnterprise
	default:
		return ""
	}
}

func copilotUsageFiltersFromFlags(scope githubcopilot.Scope) (githubcopilot.UsageFilters, error) {
	if usageCopilotYear < 0 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --year %d", usageCopilotYear)
	}
	if usageCopilotMonth < 0 || usageCopilotMonth > 12 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --month %d (expected 1-12)", usageCopilotMonth)
	}
	if usageCopilotDay < 0 || usageCopilotDay > 31 {
		return githubcopilot.UsageFilters{}, fmt.Errorf("invalid --day %d (expected 1-31)", usageCopilotDay)
	}
	filterUser := usageCopilotUser
	filterOrg := usageCopilotOrg
	if scope == githubcopilot.ScopeUser {
		filterUser = ""
	}
	if scope == githubcopilot.ScopeOrganization {
		filterOrg = ""
	}
	return githubcopilot.UsageFilters{
		Year:         usageCopilotYear,
		Month:        usageCopilotMonth,
		Day:          usageCopilotDay,
		User:         filterUser,
		Organization: filterOrg,
		Model:        usageCopilotModel,
		Product:      usageCopilotProduct,
		CostCenterID: usageCopilotCostCenter,
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
