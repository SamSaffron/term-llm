package mcpstart

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
)

// LoadSessionManager creates a non-started manager for config-backed session
// listings. Saved agent names are re-resolved. This read path filters the view
// only; actual session restore owns persistence of a cleaned selection.
func LoadSessionManager(ctx context.Context, cfg *config.Config, store session.Store, sessionID, defaultAgent string) (*mcp.Manager, string, error) {
	connections, err := mcp.LoadConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load MCP connections: %w", err)
	}
	setting, agentName := "", defaultAgent
	var sess *session.Session
	if store != nil && strings.TrimSpace(sessionID) != "" {
		sess, err = store.Get(ctx, sessionID)
		if err != nil {
			return nil, "", err
		}
		if sess != nil {
			setting, agentName = sess.MCP, sess.Agent
		}
	}
	identity := ""
	if cfg != nil {
		resolved, err := appruntime.LoadPersistedAgent(agentName, cfg)
		if err != nil {
			return nil, "", err
		}
		identity = agents.PolicyIdentity(resolved)
	}
	manager := mcp.NewManagerWithConfig(connections, mcp.WithPolicyIdentity(identity))
	if err := manager.RefreshPolicy(); err != nil {
		return nil, "", err
	}
	cleaned := strings.Join(manager.FilterSelection(ParseServerList(setting)), ",")

	return manager, cleaned, nil
}
