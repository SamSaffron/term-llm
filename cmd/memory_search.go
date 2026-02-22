package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	memorySearchLimit int
	memorySearchJSON  bool
)

var memorySearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search memory fragments (BM25)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMemorySearch,
}

func init() {
	memorySearchCmd.Flags().IntVar(&memorySearchLimit, "limit", 6, "Maximum number of results")
	memorySearchCmd.Flags().BoolVar(&memorySearchJSON, "json", false, "Output as JSON")
}

func runMemorySearch(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		return fmt.Errorf("query cannot be empty")
	}

	results, err := store.SearchFragments(context.Background(), query, memorySearchLimit, strings.TrimSpace(memoryAgent))
	if err != nil {
		return err
	}

	if memorySearchJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	if len(results) == 0 {
		fmt.Println("No memory fragments found.")
		return nil
	}

	if strings.TrimSpace(memoryAgent) == "" {
		fmt.Printf("%-14s %-36s %s\n", "AGENT", "PATH", "SNIPPET")
		fmt.Println(strings.Repeat("-", 108))
		for _, r := range results {
			fmt.Printf("%-14s %-36s %s\n", r.Agent, truncateString(r.Path, 36), oneLine(truncateString(r.Snippet, 64)))
		}
		return nil
	}

	fmt.Printf("%-36s %s\n", "PATH", "SNIPPET")
	fmt.Println(strings.Repeat("-", 96))
	for _, r := range results {
		fmt.Printf("%-36s %s\n", truncateString(r.Path, 36), oneLine(truncateString(r.Snippet, 64)))
	}

	return nil
}

func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}
