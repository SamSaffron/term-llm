// Package sessions implements the `term-llm sessions` command family. The cmd
// package owns Cobra definitions and flag binding; each command here is a
// method on App, which carries the parsed flag values (Options) and the
// CLI-level helpers (Deps) it needs.
package sessions

import (
	"fmt"
	"io"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
)

// Deps are CLI-level helpers owned by the cmd package.
type Deps struct {
	LoadConfig         func() (*config.Config, error)
	SessionStoreConfig func(cfg *config.Config) session.Config
	// StartInteractiveDiagnostics routes diagnostics away from the terminal UI
	// and returns the function that restores them.
	StartInteractiveDiagnostics func(stderr io.Writer) (func(), error)
	// ResumeChat hands the terminal over to `chat --resume <id>`.
	ResumeChat func(sessionID string) error
	// NewSharePublisher defaults to share.NewPublisher; tests inject fakes.
	NewSharePublisher func(config.ShareConfig) (sharepkg.Publisher, error)
}

// Options holds the flag values for the sessions subcommands.
type Options struct {
	// list / autotitle (shared --limit)
	Provider  string
	Limit     int
	Status    string
	Mode      string
	TagFilter string
	// show
	JSON bool
	// export
	ExportIncludeSystem       bool
	ExportIncludeAudio        bool
	ExportIncludeReasoning    bool
	ExportIncludeRawReasoning bool
	// export gist
	GistPublic              bool
	GistIncludeSystem       bool
	GistIncludeReasoning    bool
	GistIncludeRawReasoning bool
	// share
	ShareVisibility          string
	ShareNew                 bool
	ShareJSON                bool
	ShareIncludeRawReasoning bool
	ShareIncludeAudio        bool
	ShareNoImages            bool
	ShareNoTools             bool
	// reset
	ResetYes bool
	// autotitle
	AutotitleDryRun  bool
	AutotitleForce   bool
	AutotitleMinAge  time.Duration
	AutotitleVerbose bool
}

// App runs the sessions subcommands.
type App struct {
	Options
	Deps
}

func (a App) openStore() (session.Store, error) {
	cfg, err := a.LoadConfig()
	if err != nil {
		return nil, err
	}

	storeCfg := a.SessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return nil, fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}

	return session.NewStore(storeCfg)
}

func (a App) newSharePublisher() func(config.ShareConfig) (sharepkg.Publisher, error) {
	if a.NewSharePublisher != nil {
		return a.NewSharePublisher
	}
	return sharepkg.NewPublisher
}
