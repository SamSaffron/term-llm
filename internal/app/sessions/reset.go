package sessions

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/spf13/cobra"
)

func (a App) Reset(cmd *cobra.Command, args []string) error {
	if !a.ResetYes && !terminalpolicy.Interactive(os.Stdin, os.Stdout) {
		return fmt.Errorf("sessions reset requires an interactive terminal; use --yes to confirm non-interactively")
	}

	cfg, err := a.LoadConfig()
	if err != nil {
		return err
	}

	storeCfg := a.SessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}

	dbPath, err := session.ResolveDBPath(storeCfg.Path)
	if err != nil {
		return fmt.Errorf("failed to get database path: %w", err)
	}
	if dbPath == ":memory:" {
		fmt.Println("Session database is configured as :memory: (ephemeral), nothing to reset.")
		return nil
	}

	// Check if database exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Println("No sessions database found.")
		return nil
	}

	// Require confirmation unless explicitly supplied.
	if !a.ResetYes {
		fmt.Printf("This will delete ALL sessions at:\n  %s\n\n", dbPath)
		fmt.Print("Type 'yes' to confirm: ")

		var response string
		if _, err := fmt.Scanln(&response); err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}

		if response != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Delete the database file and WAL files
	filesToDelete := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}

	for _, f := range filesToDelete {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete %s: %w", f, err)
		}
	}

	fmt.Println("Sessions database deleted.")
	return nil
}
