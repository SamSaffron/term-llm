package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

var (
	fileTrackMu    sync.Mutex
	fileTrackStore *filetrack.Store
	fileTrackKey   string
)

func applyServeFileTrackingOverride(cfg *config.Config, enabled bool) {
	if cfg != nil && enabled {
		cfg.FileTracking.Enabled = true
	}
}

// resolvedFileTrackConfig returns the effective store path/options and a stable
// key for comparing config changes. Empty path means tracking is disabled.
func resolvedFileTrackConfig(cfg *config.Config) (path, key string, opts filetrack.Options) {
	if cfg == nil || !cfg.FileTracking.Enabled {
		return "", "", filetrack.Options{}
	}

	path = cfg.FileTracking.Path
	if path == "" {
		dataDir, err := appdata.GetDataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: file tracking disabled: %v\n", err)
			return "", "", filetrack.Options{}
		}
		path = filepath.Join(dataDir, "file_history.db")
	}
	opts = filetrack.Options{
		MaxFileBytes: cfg.FileTracking.MaxFileBytes, MaxSessionBytes: cfg.FileTracking.MaxSessionBytes,
		MaxTotalBytes: cfg.FileTracking.MaxTotalBytes, MaxObservationRows: cfg.FileTracking.MaxObservationRows,
		MaxObservationSessionRows: cfg.FileTracking.MaxObservationSessionRows,
		MaxObservationBytes:       cfg.FileTracking.MaxObservationBytes, MaxObservationSessionBytes: cfg.FileTracking.MaxObservationSessionBytes,
		MaxObservationAgeDays: cfg.FileTracking.MaxObservationAgeDays,
	}
	key = fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d", path, opts.MaxFileBytes, opts.MaxSessionBytes, opts.MaxTotalBytes, opts.MaxObservationRows, opts.MaxObservationSessionRows, opts.MaxObservationBytes, opts.MaxObservationSessionBytes, opts.MaxObservationAgeDays)
	return path, key, opts
}

// fileTrackingStore returns the process-wide file-change history store,
// opening it on first use. Returns nil when tracking is disabled or the store
// cannot be opened — tracking must never break editing.
//
// The store is keyed by the effective file_tracking config. If the config
// changes in a long-lived process, the old store is closed and a new one is
// opened so path/limit/enabled changes take effect instead of being pinned by
// the first caller.
func fileTrackingStore(cfg *config.Config) *filetrack.Store {
	path, key, opts := resolvedFileTrackConfig(cfg)

	fileTrackMu.Lock()
	defer fileTrackMu.Unlock()

	if path == "" {
		if fileTrackStore != nil {
			_ = fileTrackStore.Close()
			fileTrackStore = nil
			fileTrackKey = ""
		}
		return nil
	}

	if fileTrackStore != nil && fileTrackKey == key {
		return fileTrackStore
	}
	if fileTrackStore != nil {
		_ = fileTrackStore.Close()
		fileTrackStore = nil
		fileTrackKey = ""
	}

	store, err := filetrack.Open(path, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: file tracking disabled: %v\n", err)
		return nil
	}
	fileTrackStore = store
	fileTrackKey = key

	// Best-effort sweep of history for deleted sessions; async so opening the
	// store never delays startup. Capture config values now because cfg may be
	// reloaded or mutated later in a long-lived process.
	maxAgeDays := cfg.Sessions.MaxAgeDays
	sessionsCfgPath := cfg.Sessions.Path
	go func() {
		sessionsPath, err := session.ResolveDBPath(sessionsCfgPath)
		if err != nil {
			sessionsPath = ""
		}
		_ = store.GC(context.Background(), sessionsPath, maxAgeDays)
	}()

	return fileTrackStore
}

func closeFileTrackingStore() {
	fileTrackMu.Lock()
	defer fileTrackMu.Unlock()
	if fileTrackStore != nil {
		_ = fileTrackStore.Close()
		fileTrackStore = nil
		fileTrackKey = ""
	}
}

// wireFileRecorder attaches the file-change recorder to a tool registry when
// file tracking is enabled. Non-fatal on failure, mirroring wireImageRecorder.
func wireFileRecorder(registry *tools.LocalToolRegistry, cfg *config.Config) {
	if registry == nil {
		return
	}
	store := fileTrackingStore(cfg)
	if store == nil {
		registry.SetFileChangeRecorder(nil)
		return
	}
	registry.SetFileChangeRecorder(filetrack.NewRecorder(store))
}
