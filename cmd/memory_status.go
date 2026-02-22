package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var memoryStatusJSON bool

var memoryStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show memory mining status",
	RunE:  runMemoryStatus,
}

type memoryStatusRow struct {
	Agent           string     `json:"agent"`
	FragmentCount   int        `json:"fragment_count"`
	LastMinedAt     *time.Time `json:"last_mined_at,omitempty"`
	SessionsPending int        `json:"sessions_pending"`
}

func init() {
	memoryStatusCmd.Flags().BoolVar(&memoryStatusJSON, "json", false, "Output as JSON")
}

func runMemoryStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	memStore, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer memStore.Close()

	counts, err := memStore.FragmentCountsByAgent(ctx)
	if err != nil {
		return err
	}
	lastMinedByAgent, err := memStore.LastMinedByAgent(ctx)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	sessStore, err := openReadOnlySessionStore(cfg)
	if err != nil {
		return err
	}
	defer sessStore.Close()

	rowsByAgent := map[string]*memoryStatusRow{}
	filterAgent := strings.TrimSpace(memoryAgent)

	for agent, count := range counts {
		if filterAgent != "" && agent != filterAgent {
			continue
		}
		row := rowsByAgent[agent]
		if row == nil {
			row = &memoryStatusRow{Agent: agent}
			rowsByAgent[agent] = row
		}
		row.FragmentCount = count
	}

	for agent, minedAt := range lastMinedByAgent {
		if filterAgent != "" && agent != filterAgent {
			continue
		}
		row := rowsByAgent[agent]
		if row == nil {
			row = &memoryStatusRow{Agent: agent}
			rowsByAgent[agent] = row
		}
		minedAtCopy := minedAt
		row.LastMinedAt = &minedAtCopy
	}

	complete, err := listCompleteSessions(ctx, sessStore)
	if err != nil {
		return fmt.Errorf("list complete sessions: %w", err)
	}

	for _, summary := range complete {
		sess, err := sessStore.Get(ctx, summary.ID)
		if err != nil {
			return fmt.Errorf("get session %s: %w", summary.ID, err)
		}
		if sess == nil {
			continue
		}

		agent := resolveMemoryAgent(sess.Agent)
		if filterAgent != "" && agent != filterAgent {
			continue
		}

		row := rowsByAgent[agent]
		if row == nil {
			row = &memoryStatusRow{Agent: agent}
			rowsByAgent[agent] = row
		}

		state, err := memStore.GetState(ctx, sess.ID)
		if err != nil {
			return fmt.Errorf("get mining state for session %s: %w", sess.ID, err)
		}
		if state == nil || state.LastMinedOffset < summary.MessageCount {
			row.SessionsPending++
		}
	}

	rows := make([]memoryStatusRow, 0, len(rowsByAgent))
	for _, row := range rowsByAgent {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Agent < rows[j].Agent
	})

	if memoryStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Println("No memory status data available.")
		return nil
	}

	fmt.Printf("%-14s %-10s %-12s %-8s\n", "AGENT", "FRAGMENTS", "LAST MINED", "PENDING")
	fmt.Println(strings.Repeat("-", 52))
	for _, row := range rows {
		lastMined := "-"
		if row.LastMinedAt != nil {
			lastMined = formatRelativeTime(*row.LastMinedAt)
		}
		fmt.Printf("%-14s %-10d %-12s %-8d\n", row.Agent, row.FragmentCount, lastMined, row.SessionsPending)
	}

	return nil
}
