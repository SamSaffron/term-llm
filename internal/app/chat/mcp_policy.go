package chat

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/app/runtime/mcpstart"
	"github.com/samsaffron/term-llm/internal/session"
)

// prepareChatMCP persists only a successfully loaded selection (or an explicit
// empty override). The manager and selection were prepared before any startup.
func prepareChatMCP(ctx context.Context, prepared *mcpstart.PreparedSelection, store session.Store, sess *session.Session) (string, error) {
	if prepared.CanPersist() && sess != nil && sess.MCP != prepared.Active {
		sess.MCP = prepared.Active
		if err := store.Update(ctx, sess); err != nil {
			return "", fmt.Errorf("save cleaned MCP selection: %w", err)
		}
	}
	return prepared.Active, nil
}
