// Package memory implements the `term-llm memory` command family. The cmd
// package owns Cobra definitions and flag binding; each command here takes an
// explicit Options struct and reaches CLI-level helpers through Deps.
package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Deps are CLI-level helpers owned by the cmd package.
type Deps struct {
	LoadConfig func() (*config.Config, error)
	// ApplyAskProviderOverrides applies the configured ask provider/model to cfg.
	ApplyAskProviderOverrides func(cfg *config.Config) error
	NewEngine                 func(provider llm.Provider, cfg *config.Config) *llm.Engine
	SessionStoreConfig        func(cfg *config.Config) session.Config
	ParseTags                 func(tags string) []string
	FormatRelativeTime        func(t time.Time) string
	FormatBytes               func(b int64) string
	ActiveModel               func(cfg *config.Config) string
	LoadAgent                 func(name string, cfg *config.Config) (*agents.Agent, error)
	// DebugRaw reports whether raw debug logging is enabled.
	DebugRaw func() bool
}

func (d Deps) debugRaw() bool { return d.DebugRaw != nil && d.DebugRaw() }

// Env carries the persistent `memory` flags plus injected dependencies.
type Env struct {
	Agent  string
	DBPath string
	// DefaultDBPath is the default captured when Cobra bound --db.
	DefaultDBPath string
	DryRun        bool
	Deps
}

// DefaultDBPath returns the default memory database path shown in flag help.
func DefaultDBPath() string {
	if dbPath, err := memorydb.GetDBPath(); err == nil {
		return dbPath
	}
	return "~/.local/share/term-llm/memory.db"
}

// ResolveDBPath returns the database path for a --db flag value, honoring
// TERM_LLM_MEMORY_DB when the flag is still at its default.
func ResolveDBPath(flagValue, defaultFlagValue string) string {
	if defaultFlagValue == "" {
		defaultFlagValue = DefaultDBPath()
	}
	if dbEnv := os.Getenv("TERM_LLM_MEMORY_DB"); dbEnv != "" && flagValue == defaultFlagValue {
		return dbEnv
	}
	return flagValue
}

// AgentFilter returns the trimmed --agent value.
func (e Env) AgentFilter() string { return strings.TrimSpace(e.Agent) }

// OpenStore opens the memory store selected by the environment.
func (e Env) OpenStore() (*memorydb.Store, error) {
	return OpenStoreAtPath(ResolveDBPath(e.DBPath, e.DefaultDBPath))
}

// OpenStoreAtPath opens the memory store at path.
func OpenStoreAtPath(path string) (*memorydb.Store, error) {
	store, err := memorydb.NewStore(memorydb.Config{Path: path})
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	return store, nil
}

type imageRecordStore interface {
	tools.ImageRecorder
	Close() error
}

type imageRecordStoreOpener func(path string) (imageRecordStore, error)

type lazyImageRecorder struct {
	path string
	open imageRecordStoreOpener
}

func (r *lazyImageRecorder) RecordImage(ctx context.Context, record *memorydb.ImageRecord) error {
	store, err := r.open(r.path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecordImage(ctx, record)
}

func openImageRecordStore(path string) (imageRecordStore, error) {
	return OpenStoreAtPath(path)
}

// WireImageRecorder attaches a recorder that opens the memory store only when
// an image is generated, then closes it after recording. Recording remains
// non-fatal if the store cannot be opened. dbPath is the --db flag value.
func WireImageRecorder(registry *tools.LocalToolRegistry, dbPath, defaultDBPath, agent, sessionID string) {
	wireImageRecorderWithOpener(registry, dbPath, defaultDBPath, agent, sessionID, openImageRecordStore)
}

func wireImageRecorderWithOpener(registry *tools.LocalToolRegistry, dbPath, defaultDBPath, agent, sessionID string, open imageRecordStoreOpener) {
	if registry == nil {
		return
	}
	recorder := &lazyImageRecorder{
		// Runtime setup has already applied flags and environment overrides. Keep
		// that selected path stable for the lifetime of this registry.
		path: ResolveDBPath(dbPath, defaultDBPath),
		open: open,
	}
	registry.SetImageRecorder(recorder, agent, sessionID)
}

// RecordImageDirect records a generated image in the memory store, ignoring
// failures.
func RecordImageDirect(dbPath, defaultDBPath string, cfg *config.Config, prompt, outputPath string, result *image.ImageResult, providerName string) {
	if cfg == nil || outputPath == "" || result == nil {
		return
	}
	store, err := OpenStoreAtPath(ResolveDBPath(dbPath, defaultDBPath))
	if err != nil {
		return
	}
	defer store.Close()

	rec := &memorydb.ImageRecord{
		Prompt:     prompt,
		OutputPath: outputPath,
		MimeType:   result.MimeType,
		Provider:   providerName,
		FileSize:   len(result.Data),
	}
	_ = store.RecordImage(context.Background(), rec)
}
