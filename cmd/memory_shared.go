package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func openReadOnlySessionStore(cfg *config.Config) (session.Store, error) {
	storeCfg := sessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return nil, fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}
	storeCfg.ReadOnly = true
	return session.NewStore(storeCfg)
}

func listCompleteSessions(ctx context.Context, store session.Store) ([]session.SessionSummary, error) {
	const pageSize = 200
	offset := 0
	all := make([]session.SessionSummary, 0, pageSize)

	for {
		page, err := store.List(ctx, session.ListOptions{
			Status: session.StatusComplete,
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}

	return all, nil
}

func resolveMemoryAgent(sessionAgent string) string {
	if strings.TrimSpace(memoryAgent) != "" {
		return strings.TrimSpace(memoryAgent)
	}
	sessionAgent = strings.TrimSpace(sessionAgent)
	if sessionAgent != "" {
		return sessionAgent
	}
	return "default"
}

func hasMemoryMiningTag(tags string) bool {
	for _, tag := range parseTags(tags) {
		if tag == "memory-mining" {
			return true
		}
	}
	return false
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "UNIQUE constraint failed: memory_fragments.agent, memory_fragments.path")
}
