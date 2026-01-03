package cmd

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

// ProviderFlagCompletion handles --provider flag completion for LLM commands
func ProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	completions := llm.GetProviderCompletions(toComplete, false)

	// If completing provider name (no colon), don't add space so user can type ":"
	if !strings.Contains(toComplete, ":") {
		return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// ImageProviderFlagCompletion handles --provider flag completion for image commands
func ImageProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	completions := llm.GetProviderCompletions(toComplete, true)

	// If completing provider name (no colon), don't add space so user can type ":"
	if !strings.Contains(toComplete, ":") {
		return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}
