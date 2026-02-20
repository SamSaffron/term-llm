package serve

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// SessionRuntime is a per-conversation runtime for non-web platforms.
type SessionRuntime struct {
	Engine       *llm.Engine
	ProviderName string
	ModelName    string
	Cleanup      func()
}

// Settings holds per-platform runtime settings derived from CLI flags and config.
type Settings struct {
	SystemPrompt string
	IdleTimeout  time.Duration
	MaxTurns     int
	Search       bool
	Tools        string
	MCP          string
	Agent        string
	Store        session.Store
	// NewSession creates a fresh runtime instance for a new conversation.
	// Called once per platform session (for example, per Telegram chat session).
	NewSession func(context.Context) (*SessionRuntime, error)
}

// Platform is the interface implemented by each messaging platform adapter.
type Platform interface {
	// Name returns the platform identifier (e.g. "telegram").
	Name() string
	// NeedsSetup returns true when required configuration is missing.
	NeedsSetup() bool
	// RunSetup runs an interactive wizard to collect and persist configuration.
	RunSetup() error
	// Run starts the platform's message loop, blocking until ctx is cancelled.
	Run(ctx context.Context, cfg *config.Config, settings Settings) error
}
