package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is the interface for session persistence.
type Store interface {
	// Session CRUD
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	GetByNumber(ctx context.Context, number int64) (*Session, error)
	GetByPrefix(ctx context.Context, prefix string) (*Session, error)
	Update(ctx context.Context, s *Session) error
	Delete(ctx context.Context, id string) error

	// Listing and search
	List(ctx context.Context, opts ListOptions) ([]SessionSummary, error)
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)

	// Message operations - stores full llm.Message with Parts
	AddMessage(ctx context.Context, sessionID string, msg *Message) error
	GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error

	// Metrics operations (for incremental session saving)
	UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens int) error
	UpdateStatus(ctx context.Context, id string, status SessionStatus) error
	IncrementUserTurns(ctx context.Context, id string) error

	// Current session tracking (for auto-resume)
	SetCurrent(ctx context.Context, sessionID string) error
	GetCurrent(ctx context.Context) (*Session, error)
	ClearCurrent(ctx context.Context) error

	// Lifecycle
	Close() error
}

// Config holds session storage configuration.
type Config struct {
	Enabled    bool   `mapstructure:"enabled"`      // Master switch
	MaxAgeDays int    `mapstructure:"max_age_days"` // Auto-delete after N days (0=never)
	MaxCount   int    `mapstructure:"max_count"`    // Keep at most N sessions (0=unlimited)
	Path       string `mapstructure:"path"`         // Optional DB path override (supports :memory:)
	ReadOnly   bool   `mapstructure:"-"`            // Open DB in read-only mode (skip schema init/cleanup)
}

// DefaultConfig returns the default session configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:    true,
		MaxAgeDays: 0, // Never auto-delete
		MaxCount:   0, // Unlimited
		Path:       "",
	}
}

// GetDataDir returns the XDG data directory for term-llm.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDataDir() (string, error) {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm"), nil
}

// GetDBPath returns the path to the sessions database.
func GetDBPath() (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "sessions.db"), nil
}

// ResolveDBPath resolves an optional DB path override.
// Empty path uses the default XDG location.
// Supports :memory: for ephemeral in-memory storage.
func ResolveDBPath(pathOverride string) (string, error) {
	pathOverride = strings.TrimSpace(pathOverride)
	if pathOverride == "" {
		return GetDBPath()
	}
	if pathOverride == ":memory:" {
		return pathOverride, nil
	}

	// Expand env vars and leading "~/".
	pathOverride = os.ExpandEnv(pathOverride)
	if strings.HasPrefix(pathOverride, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		pathOverride = filepath.Join(homeDir, pathOverride[2:])
	}

	abs, err := filepath.Abs(pathOverride)
	if err != nil {
		return "", fmt.Errorf("resolve db path %q: %w", pathOverride, err)
	}
	return abs, nil
}

// NewStore creates a new Store based on the configuration.
// If sessions are disabled, returns a no-op store.
func NewStore(cfg Config) (Store, error) {
	if !cfg.Enabled {
		return &NoopStore{}, nil
	}
	return NewSQLiteStore(cfg)
}
