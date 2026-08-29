package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/spf13/cobra"
)

var lifecycleStatusJSON bool

var lifecycleCmd = &cobra.Command{
	Use:   "lifecycle",
	Short: "Inspect terminal-host lifecycle integration",
}

var lifecycleStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Explain detected and enabled lifecycle adapters",
	Long: `Report terminal-host lifecycle discovery without publishing live state.

The command does not invoke configured sinks or mutate host status.`,
	Args: cobra.NoArgs,
	RunE: runLifecycleStatus,
}

func init() {
	rootCmd.AddCommand(lifecycleCmd)
	lifecycleCmd.AddCommand(lifecycleStatusCmd)
	lifecycleStatusCmd.Flags().BoolVar(&lifecycleStatusJSON, "json", false, "Output as JSON")
}

func runLifecycleStatus(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	report, err := termhost.Inspect(cfg.Lifecycle, legacyTerminalProgressEnabled(cfg))
	if err != nil {
		return fmt.Errorf("inspect terminal-host lifecycle: %w", err)
	}
	if lifecycleStatusJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Lifecycle: %v (%s)\n", report.Enabled, report.Reason)
	for _, adapter := range report.Adapters {
		fmt.Fprintf(out, "%-20s detected=%-5v enabled=%-5v %s\n", adapter.Name+" ["+adapter.Type+"]", adapter.Detected, adapter.Enabled, adapter.Reason)
	}
	fmt.Fprintf(out, "%-20s detected=%-5v enabled=%-5v %s\n", report.OSC.Name+" ["+report.OSC.Type+"]", report.OSC.Detected, report.OSC.Enabled, report.OSC.Reason)
	return nil
}
