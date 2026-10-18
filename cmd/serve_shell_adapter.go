package cmd

import (
	"context"
	"time"

	serveshell "github.com/samsaffron/term-llm/internal/app/serve/shell"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

// The shared shell manager, PTY handling, and collaborative controller live in
// internal/app/serve/shell; these aliases keep serve startup and HTTP handlers
// wired without depending on the package layout.
const serveShellReplayBytes = serveshell.ReplayBytes

type (
	serveShellManager               = serveshell.Manager
	serveShell                      = serveshell.Shell
	serveShellCollaborationSnapshot = serveshell.CollaborationSnapshot
)

var errServeShellClosed = serveshell.ErrClosed

func newServeShellManager(ttl time.Duration, exists func(string) bool) *serveShellManager {
	return serveshell.NewManager(ttl, exists)
}

func platformServeShellSupported() bool { return serveshell.Supported() }

func collectLLMText(message llm.Message) string { return serveshell.CollectLLMText(message) }

// serveCollaborativeShellController adapts the extracted controller to the
// serve runtime, which supplies the manager lazily.
type serveCollaborativeShellController struct {
	manager func() (*serveShellManager, error)
}

func (c *serveCollaborativeShellController) inner() *serveshell.Controller {
	if c == nil {
		return nil
	}
	return &serveshell.Controller{Manager: c.manager}
}

func (c *serveCollaborativeShellController) Mode(ctx context.Context, sessionID string) tools.CollaborativeShellMode {
	return c.inner().Mode(ctx, sessionID)
}

func (c *serveCollaborativeShellController) PrepareRequestContext(ctx context.Context, sessionID string, messages []llm.Message) ([]llm.Message, error) {
	return c.inner().PrepareRequestContext(ctx, sessionID, messages)
}

func (c *serveCollaborativeShellController) PrepareCompactionContext(ctx context.Context, sessionID string, result *llm.CompactionResult) error {
	return c.inner().PrepareCompactionContext(ctx, sessionID, result)
}

func (c *serveCollaborativeShellController) Execute(ctx context.Context, sessionID string, args tools.SharedShellArgs) (tools.ShellResult, error) {
	return c.inner().Execute(ctx, sessionID, args)
}

func (c *serveCollaborativeShellController) ReserveActivity(ctx context.Context, sessionID, expectedShellID string) (*tools.SharedShellActivity, error) {
	return c.inner().ReserveActivity(ctx, sessionID, expectedShellID)
}

func (c *serveCollaborativeShellController) CommitDurableActivity(ctx context.Context, sessionID string, activity tools.SharedShellActivity) error {
	return c.inner().CommitDurableActivity(ctx, sessionID, activity)
}

func (c *serveCollaborativeShellController) CommitActivity(ctx context.Context, sessionID, reservationID string) error {
	return c.inner().CommitActivity(ctx, sessionID, reservationID)
}

func (c *serveCollaborativeShellController) ReleaseActivity(ctx context.Context, sessionID, reservationID string) {
	c.inner().ReleaseActivity(ctx, sessionID, reservationID)
}

func (c *serveCollaborativeShellController) String() string { return c.inner().String() }

var _ tools.CollaborativeShellController = (*serveCollaborativeShellController)(nil)
var _ tools.CollaborativeShellActivityController = (*serveCollaborativeShellController)(nil)
