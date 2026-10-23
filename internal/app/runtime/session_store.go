package runtime

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
)

// SessionStoreOverrides captures the command-level persistence flags.
type SessionStoreOverrides struct {
	NoSession bool
	DBPath    string
}

// SessionStoreConfigWith resolves persistence settings with explicit command overrides.
func SessionStoreConfigWith(cfg *config.Config, overrides SessionStoreOverrides) session.Config {
	path := strings.TrimSpace(cfg.Sessions.Path)
	if cliPath := strings.TrimSpace(overrides.DBPath); cliPath != "" {
		path = cliPath
	}
	return session.Config{
		Enabled:          cfg.Sessions.Enabled && !overrides.NoSession,
		MaxAgeDays:       cfg.Sessions.MaxAgeDays,
		MaxCount:         cfg.Sessions.MaxCount,
		Path:             path,
		StripImageBase64: cfg.Sessions.StripImageBase64,
	}
}

// InitSessionStoreWith creates a session store if enabled in config.
// Returns the store (may be nil if disabled) and a cleanup function.
// The cleanup function is always safe to call (handles nil store).
// Warnings are written to errWriter.
func InitSessionStoreWith(cfg *config.Config, errWriter io.Writer, overrides SessionStoreOverrides) (session.Store, func()) {
	storeCfg := SessionStoreConfigWith(cfg, overrides)
	if !storeCfg.Enabled {
		return nil, func() {}
	}

	store, err := session.NewStore(storeCfg)
	if err != nil {
		if isNewerSessionSchemaError(err) {
			fmt.Fprintf(errWriter, "warning: session store unavailable: %v\nUpgrade this term-llm binary to a version that supports this database schema. Session persistence is disabled for this run; the database has been left intact.\n", err)
			return nil, func() {}
		}
		// Check if this is a schema error that can be recovered by reset
		if isSchemaError(err) {
			dbPath, _ := session.ResolveDBPath(storeCfg.Path)
			fmt.Fprintf(errWriter, "Session database has schema errors: %v\n\n", err)
			fmt.Fprintf(errWriter, "Would you like to reset the sessions database? [y/N]: ")

			if PromptForReset() {
				if resetErr := resetSessionDatabase(storeCfg.Path); resetErr != nil {
					fmt.Fprintf(errWriter, "warning: failed to reset database: %v\n", resetErr)
					return nil, func() {}
				}
				fmt.Fprintln(errWriter, "Session database reset. Creating new store...")
				// Retry store creation
				store, err = session.NewStore(storeCfg)
				if err != nil {
					fmt.Fprintf(errWriter, "warning: session store still unavailable after reset: %v\n", err)
					return nil, func() {}
				}
			} else {
				fmt.Fprintf(errWriter, "\nTo fix manually, run: term-llm sessions reset\n")
				fmt.Fprintf(errWriter, "Or delete: %s\n", dbPath)
				return nil, func() {}
			}
		} else {
			fmt.Fprintf(errWriter, "warning: session store unavailable: %v\n", err)
			return nil, func() {}
		}
	}

	// Wrap store with logging to surface persistence errors
	store = session.NewLoggingStore(store, func(format string, args ...any) {
		fmt.Fprintf(errWriter, "warning: "+format+"\n", args...)
	})

	cleanup := func() {
		if store != nil {
			store.Close()
		}
	}

	return store, cleanup
}

// isNewerSessionSchemaError recognizes session's untyped future-version rejection,
// including when it is wrapped by schema initialization. This is not corruption.
func isNewerSessionSchemaError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "session database schema version ") &&
		strings.Contains(message, " is newer than supported version ")
}

// isSchemaError checks if an error indicates a database schema problem
// that could be fixed by resetting the database.
func isSchemaError(err error) bool {
	if err == nil || isNewerSessionSchemaError(err) {
		return false
	}
	errStr := err.Error()
	schemaPatterns := []string{
		"no such column",
		"no such table",
		"SQL logic error",
		"database disk image is malformed",
		"file is not a database",
		"create base schema",
		"initialize schema",
	}
	for _, pattern := range schemaPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// PromptForReset asks the user if they want to reset the session database.
// Returns true if user confirms with 'y' or 'yes'.
func PromptForReset() bool {
	if !terminalpolicy.OutputInteractive(os.Stderr) {
		return false
	}

	// Use /dev/tty directly; never consume redirected stdin for an implicit,
	// destructive recovery prompt.
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()

	reader := bufio.NewReader(tty)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "yes" || response == "y"
}

// resetSessionDatabase deletes the session database files.
func resetSessionDatabase(pathOverride string) error {
	dbPath, err := session.ResolveDBPath(pathOverride)
	if err != nil {
		return fmt.Errorf("get db path: %w", err)
	}
	if dbPath == ":memory:" {
		return nil
	}

	filesToDelete := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}

	for _, f := range filesToDelete {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete %s: %w", f, err)
		}
	}

	return nil
}
