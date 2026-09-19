package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	liveclassify "github.com/samsaffron/term-llm/internal/live/classify"
	"github.com/spf13/cobra"
)

var liveDecisionsSince string

var liveCmd = &cobra.Command{
	Use:   "live",
	Short: "Inspect live voice diagnostics",
}

var liveDecisionsCmd = &cobra.Command{
	Use:   "decisions",
	Short: "List classifier route decisions",
	RunE:  runLiveDecisions,
}

func init() {
	rootCmd.AddCommand(liveCmd)
	liveCmd.AddCommand(liveDecisionsCmd)
	liveDecisionsCmd.Flags().StringVar(&liveDecisionsSince, "since", "", "Only show decisions since a duration ago or RFC3339 timestamp")
}

func runLiveDecisions(cmd *cobra.Command, _ []string) error {
	since, err := parseLiveDecisionsSince(liveDecisionsSince, time.Now())
	if err != nil {
		return err
	}
	diagnosticsDir := config.GetDiagnosticsDir()
	path := filepath.Join(diagnosticsDir, "live.db")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(cmd.OutOrStdout(), "No decisions recorded.")
			return nil
		}
		return fmt.Errorf("inspect live decision database: %w", err)
	}
	store, err := liveclassify.OpenDecisionStoreReadOnly(path)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := store.List(context.WithoutCancel(cmd.Context()), since, 200)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "TIME\tLIVE\tSESSION\tGATED\tACTED\tRESOLVER\tLATENCY\tPROBABILITIES\tERROR\tSTATE")
	for _, row := range rows {
		probabilities, _ := json.Marshal(row.Probabilities)
		state := strings.ReplaceAll(strings.TrimSpace(string(row.StateJSON)), "\t", " ")
		state = strings.ReplaceAll(state, "\n", " ")
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.CreatedAt.Local().Format(time.RFC3339), row.LiveID, row.BoundSession, row.GatedLabel,
			row.ActedLabel, row.ResolverOutcome, row.Latency, probabilities,
			strings.ReplaceAll(row.Error, "\t", " "), state)
	}
	return writer.Flush()
}

func parseLiveDecisionsSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return time.Time{}, fmt.Errorf("--since duration must not be negative")
		}
		return now.Add(-duration), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q: expected a duration such as 24h or an RFC3339 timestamp", value)
	}
	return parsed, nil
}
