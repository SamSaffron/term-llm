package cmd

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/spf13/cobra"
)

var memoryStatusJSON bool

var memoryStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show memory mining status",
	RunE:  runMemoryStatus,
}

func init() {
	memoryStatusCmd.Flags().BoolVar(&memoryStatusJSON, "json", false, "Output as JSON")
}

func runMemoryStatus(cmd *cobra.Command, args []string) error {
	return appmemory.Status(appmemory.StatusOptions{Env: memoryEnv(), JSON: memoryStatusJSON})
}
