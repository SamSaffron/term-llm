package cmd

import (
	usageapp "github.com/samsaffron/term-llm/internal/app/usage"
	"github.com/spf13/cobra"
)

var usageOptions = usageapp.NewOptions()

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show usage, costs, and subscription limits",
	Long: `Show token usage and costs from Claude Code, Codex CLI, and term-llm.

By default, this command reads local usage data stored by these CLI tools and
shows aggregated token counts and estimated costs. The agy-bin, chatgpt, grok,
claude-bin, cursor-bin, and opencode-go providers instead fetch live subscription
limits without invoking a model or consuming tokens.

For GitHub Copilot, it fetches AI Credit usage from GitHub's latest
billing usage API. Set GITHUB_TOKEN or GH_TOKEN with billing permissions.

Examples:
  term-llm usage                              # show last 7 days
  term-llm usage --provider claude-code       # filter local Claude Code history
  term-llm usage --provider claude-bin        # show live Claude subscription limits
  term-llm usage --provider claude-bin --json # output the structured Claude response
  term-llm usage --provider agy-bin           # show live Antigravity model quotas
  term-llm usage --provider agy-bin --json    # output structured Antigravity quotas
  term-llm usage --provider cursor-bin        # show live Cursor plan usage
  term-llm usage --provider cursor-bin --json # output the structured Cursor response
  term-llm usage --provider chatgpt           # show live ChatGPT Codex plan usage
  term-llm usage --provider grok              # show live Grok coding-credit usage
  term-llm usage --provider opencode-go       # show live OpenCode Go plan usage
  term-llm usage --provider openrouter        # account usage, last 30 UTC days
  term-llm usage -p openrouter --days 7 --daily # daily account usage
  term-llm usage --provider copilot           # show personal GitHub Copilot AI Credit usage
  term-llm usage --provider copilot --copilot-scope org --copilot-entity my-org
  term-llm usage --provider term-llm          # show term-llm direct API usage
  term-llm usage --since 20250101             # from Jan 1, 2025
  term-llm usage --json                       # output as JSON
  term-llm usage --breakdown                  # show per-model breakdown`,
	RunE: usageOptions.Run,
}

func init() {
	rootCmd.AddCommand(usageCmd)
	usageCmd.Flags().StringVarP(&usageOptions.Provider, "provider", "p", "", "Filter by provider (agy-bin, chatgpt, grok, claude-bin, cursor-bin, opencode-go, openrouter, claude-code, copilot, term-llm, or all)")
	usageCmd.Flags().StringVar(&usageOptions.Since, "since", "", "Start date (YYYYMMDD)")
	usageCmd.Flags().StringVar(&usageOptions.Until, "until", "", "End date (YYYYMMDD)")
	usageCmd.Flags().IntVar(&usageOptions.Days, "days", 30, "OpenRouter calendar UTC days including today (1-3660)")
	usageCmd.Flags().BoolVar(&usageOptions.Daily, "daily", false, "Show OpenRouter usage by UTC day instead of model")
	usageCmd.Flags().BoolVar(&usageOptions.JSON, "json", false, "Output as JSON")
	usageCmd.Flags().BoolVar(&usageOptions.Breakdown, "breakdown", false, "Show per-model breakdown")
	usageCmd.Flags().BoolVar(&usageOptions.IncludeExternal, "include-external", false, "Include externally-tracked term-llm usage (claude-bin and codex calls)")
	usageCmd.Flags().StringVar(&usageOptions.CopilotScope, "copilot-scope", "user", "Copilot billing scope (user, org, enterprise)")
	usageCmd.Flags().StringVar(&usageOptions.CopilotEntity, "copilot-entity", "", "Copilot billing entity (username, organization, or enterprise slug; defaults to authenticated user for user scope)")
	usageCmd.Flags().IntVar(&usageOptions.CopilotYear, "year", 0, "Copilot usage year (YYYY; defaults to GitHub API current year)")
	usageCmd.Flags().IntVar(&usageOptions.CopilotMonth, "month", 0, "Copilot usage month (1-12; defaults to GitHub API current month)")
	usageCmd.Flags().IntVar(&usageOptions.CopilotDay, "day", 0, "Copilot usage day (1-31)")
	usageCmd.Flags().StringVar(&usageOptions.CopilotModel, "copilot-model", "", "Filter Copilot AI Credit usage by model")
	usageCmd.Flags().StringVar(&usageOptions.CopilotProduct, "copilot-product", "", "Filter Copilot AI Credit usage by product")
	usageCmd.Flags().StringVar(&usageOptions.CopilotUser, "copilot-user", "", "GitHub username for user scope, or user filter for organization/enterprise scope")
	usageCmd.Flags().StringVar(&usageOptions.CopilotOrg, "copilot-org", "", "Organization for org scope, or organization filter for enterprise scope")
	usageCmd.Flags().StringVar(&usageOptions.CopilotEnterprise, "copilot-enterprise", "", "Enterprise slug for enterprise scope")
	usageCmd.Flags().StringVar(&usageOptions.CopilotCostCenter, "copilot-cost-center", "", "Filter enterprise Copilot usage by cost center ID")
	if err := usageCmd.RegisterFlagCompletionFunc("provider", UsageProviderFlagCompletion); err != nil {
		panic("failed to register provider completion: " + err.Error())
	}
	if err := usageCmd.RegisterFlagCompletionFunc("copilot-scope", CopilotScopeFlagCompletion); err != nil {
		panic("failed to register copilot scope completion: " + err.Error())
	}
}
