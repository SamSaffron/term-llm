package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
)

// ErrNotFound is returned when a lookup or update targets a row that does not
// exist (e.g., UpdateMessage against a deleted/never-persisted message ID).
var ErrNotFound = errors.New("session: not found")

// Store is the interface for session persistence.
type Store interface {
	// Session CRUD
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	GetByNumber(ctx context.Context, number int64) (*Session, error)
	GetByPrefix(ctx context.Context, prefix string) (*Session, error)
	Update(ctx context.Context, s *Session) error
	MarkTitleSkipped(ctx context.Context, id string, t time.Time) error
	Delete(ctx context.Context, id string) error

	// Listing and search
	List(ctx context.Context, opts ListOptions) ([]SessionSummary, error)
	Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error)

	// Message operations - stores full llm.Message with Parts
	AddMessage(ctx context.Context, sessionID string, msg *Message) error
	// UpdateMessage replaces the content of an existing message (by msg.ID) with
	// the supplied msg (role, parts, text, duration, sequence are updated in
	// place). Used for "persist as we go" upserts of an in-progress assistant
	// message during streaming. Returns ErrNotFound if the row does not exist.
	UpdateMessage(ctx context.Context, sessionID string, msg *Message) error
	GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// GetMessagesFrom returns rows at/after fromSeq in sequence order. When limit
	// <= 0, all remaining rows are returned.
	GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]Message, error)
	// GetMessageByID retrieves a single message by its global message id.
	GetMessageByID(ctx context.Context, msgID int64) (*Message, error)
	ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error
	CompactMessages(ctx context.Context, sessionID string, messages []Message) error

	// Metrics operations (for incremental session saving)
	UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error
	UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error
	UpdateStatus(ctx context.Context, id string, status SessionStatus) error
	IncrementUserTurns(ctx context.Context, id string) error

	// Current session tracking (for auto-resume)
	SetCurrent(ctx context.Context, sessionID string) error
	GetCurrent(ctx context.Context) (*Session, error)
	ClearCurrent(ctx context.Context) error

	// Push subscription management (for web push notifications)
	SavePushSubscription(ctx context.Context, sub *PushSubscription) error
	DeletePushSubscription(ctx context.Context, endpoint string) error
	ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error)

	// Lifecycle
	Close() error
}

// StreamingMessageUpdater is an optional Store capability for the hot streaming
// assistant upsert path. Implementations may update role/parts/duration without
// rewriting the FTS-backed text_content column until finalizeText is true.
type StreamingMessageUpdater interface {
	UpdateStreamingMessage(ctx context.Context, sessionID string, msg *Message, finalizeText bool) error
}

// UpdateStreamingMessage updates an in-progress assistant message using the
// store's streaming-aware fast path when available, otherwise it falls back to
// Store.UpdateMessage.
func UpdateStreamingMessage(ctx context.Context, store Store, sessionID string, msg *Message, finalizeText bool) error {
	if updater, ok := store.(StreamingMessageUpdater); ok {
		return updater.UpdateStreamingMessage(ctx, sessionID, msg, finalizeText)
	}
	return store.UpdateMessage(ctx, sessionID, msg)
}

// ProviderStateStore is an optional Store capability for provider-specific
// resume state. It stores opaque JSON/blob payloads keyed by term-llm session
// and provider key, allowing providers such as claude-bin to survive runtime
// eviction without leaking that state into the user-visible transcript.
type ProviderStateStore interface {
	SaveProviderState(ctx context.Context, sessionID, providerKey string, state []byte) error
	LoadProviderState(ctx context.Context, sessionID, providerKey string) ([]byte, error)
	DeleteProviderState(ctx context.Context, sessionID, providerKey string) error
}

// MessagesDescendingPager is an optional Store capability for efficient reverse
// pagination over session messages. Implementations return messages ordered by
// descending sequence and, when beforeSeq > 0, only rows with sequence < beforeSeq.
type MessagesDescendingPager interface {
	GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]Message, error)
}

// PromptHistoryEntry is a user prompt recalled from composer history.
type PromptHistoryEntry struct {
	ID        int64
	CreatedAt time.Time
	Text      string
}

// PromptHistoryStore is an optional Store capability for shell-style composer
// history recall. Implementations traverse persisted user prompts globally so
// multiple TUI processes share the same prompt history.
type PromptHistoryStore interface {
	PreviousUserPrompt(ctx context.Context, agent string, beforeID int64) (*PromptHistoryEntry, error)
	NextUserPrompt(ctx context.Context, agent string, afterID int64) (*PromptHistoryEntry, error)
}

// PromptHistoryOutsideSessionStore is an optional Store capability for the TUI
// composer history sequence after the current session's in-memory prompts have
// been exhausted. It traverses persisted user prompts from all agents while
// excluding the current session to avoid duplicate recalls.
type PromptHistoryOutsideSessionStore interface {
	PreviousUserPromptOutsideSession(ctx context.Context, excludeSessionID string, beforeID int64, beforeCreatedAt time.Time) (*PromptHistoryEntry, error)
	NextUserPromptOutsideSession(ctx context.Context, excludeSessionID string, afterID int64, afterCreatedAt time.Time) (*PromptHistoryEntry, error)
}

// PushSubscription represents a Web Push subscription stored in the database.
type PushSubscription struct {
	ID        string
	Endpoint  string
	KeyP256DH string
	KeyAuth   string
}

// Config holds session storage configuration.
type Config struct {
	Enabled          bool   `mapstructure:"enabled"`            // Master switch
	MaxAgeDays       int    `mapstructure:"max_age_days"`       // Auto-delete after N days (0=never)
	MaxCount         int    `mapstructure:"max_count"`          // Keep at most N sessions (0=unlimited)
	Path             string `mapstructure:"path"`               // Optional DB path override (supports :memory:)
	StripImageBase64 bool   `mapstructure:"strip_image_base64"` // Store path/metadata only for images with ImagePath (smaller DB, less portable)
	ReadOnly         bool   `mapstructure:"-"`                  // Open DB in read-only mode (skip schema init/cleanup)
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
	return appdata.GetDataDir()
}

// GetDBPath returns the path to the sessions database.
func GetDBPath() (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "sessions.db"), nil
}

// GetHandoverDir returns the handover directory for the given working directory.
// The path is XDG_DATA_HOME/term-llm/handover/<basename>-<sha256[:6]>/
// where the hash is computed from the absolute cwd to avoid collisions.
func GetHandoverDir(cwd string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}
	h := sha256.Sum256([]byte(abs))
	projectID := filepath.Base(abs) + "-" + hex.EncodeToString(h[:3])
	return filepath.Join(dataDir, "handover", projectID), nil
}

// GetHandoverPath returns a full handover file path with a random name like
// "2026-04-03-amber-creek-bloom.md". A fresh slug is generated per call so
// concurrent sessions in the same project get distinct plan files. The
// expanded system prompt is the durable per-session record of the path; use
// ExtractHandoverPath to recover it.
func GetHandoverPath(cwd, date string) (string, error) {
	dir, err := GetHandoverDir(cwd)
	if err != nil {
		return "", err
	}
	slug := RandomHandoverSlug()
	return filepath.Join(dir, date+"-"+slug+".md"), nil
}

// ExtractHandoverPath recovers the handover file path embedded in a system
// prompt via {{handover_path}}. It matches the first path under dir with the
// "<date>-<slug>.md" shape. Returns "" when the prompt names no such file.
func ExtractHandoverPath(prompt, dir string) string {
	if prompt == "" || dir == "" {
		return ""
	}
	re := regexp.MustCompile(regexp.QuoteMeta(dir) + `[\\/]\d{4}-\d{2}-\d{2}-[a-zA-Z0-9-]+\.md`)
	return re.FindString(prompt)
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
