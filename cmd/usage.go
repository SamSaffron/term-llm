package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samsaffron/term-llm/internal/usage"
	"github.com/spf13/cobra"
)

var (
	usageProvider        string
	usageSince           string
	usageUntil           string
	usageJSON            bool
	usageBreakdown       bool
	usageIncludeExternal bool
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show token usage and costs from local CLI tools",
	Long: `Show token usage and costs from Claude Code, Codex CLI, Gemini CLI, and term-llm.

This command reads local usage data stored by these CLI tools and displays
aggregated statistics including token counts and estimated costs.

Examples:
  term-llm usage                              # show last 7 days
  term-llm usage --provider claude-code       # filter to Claude Code only
  term-llm usage --provider term-llm          # show term-llm direct API usage
  term-llm usage --since 20250101             # from Jan 1, 2025
  term-llm usage --json                       # output as JSON
  term-llm usage --breakdown                  # show per-model breakdown`,
	RunE: runUsage,
}

func init() {
	rootCmd.AddCommand(usageCmd)
	usageCmd.Flags().StringVarP(&usageProvider, "provider", "p", "", "Filter by provider (claude-code, codex, gemini-cli, term-llm, or all)")
	usageCmd.Flags().StringVar(&usageSince, "since", "", "Start date (YYYYMMDD)")
	usageCmd.Flags().StringVar(&usageUntil, "until", "", "End date (YYYYMMDD)")
	usageCmd.Flags().BoolVar(&usageJSON, "json", false, "Output as JSON")
	usageCmd.Flags().BoolVar(&usageBreakdown, "breakdown", false, "Show per-model breakdown")
	usageCmd.Flags().BoolVar(&usageIncludeExternal, "include-external", false, "Include externally-tracked term-llm usage (claude-bin, codex, gemini-cli calls)")
}

func runUsage(cmd *cobra.Command, args []string) error {
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
	case "codex":
		providerFilter = usage.ProviderCodex
	case "gemini-cli", "gemini":
		providerFilter = usage.ProviderGeminiCLI
	case "term-llm":
		providerFilter = usage.ProviderTermLLM
	case "", "all":
		providerFilter = ""
	default:
		return fmt.Errorf("unknown provider: %s (use claude-code, codex, gemini-cli, or term-llm)", usageProvider)
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

// truncateModels returns the first n models with shortened names
func truncateModels(models []string, n int) []string {
	result := make([]string, 0, min(len(models), n))
	for i, m := range models {
		if i >= n {
			break
		}
		result = append(result, shortenModelName(m))
	}
	return result
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

// truncateString truncates a string to n characters
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// formatProviderName returns a human-friendly provider name
func formatProviderName(provider string) string {
	switch provider {
	case usage.ProviderClaudeCode:
		return "Claude Code"
	case usage.ProviderCodex:
		return "Codex"
	case usage.ProviderGeminiCLI:
		return "Gemini CLI"
	case usage.ProviderTermLLM:
		return "term-llm"
	default:
		return provider
	}
}
