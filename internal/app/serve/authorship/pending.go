package authorship

import (
	"context"
	"fmt"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

// RestorePendingAuthors preserves the original sender of durable steering when
// a reconnecting browser submits it as a follow-up. Neither anonymous resumers
// nor a different viewer may replace the author already accepted for that ID.
func RestorePendingAuthors(ctx context.Context, store session.Store, sessionID string, messages []core.Message) error {
	pendingStore, ok := session.AsPendingSteeringStore(store)
	if !ok {
		return nil
	}
	entries, err := pendingStore.ListPendingSteering(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load pending steering authors: %w", err)
	}
	authors := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.OwnerKind == "" {
			authors[entry.ID] = entry.Message.AuthorID
		}
	}
	for i := range messages {
		if messages[i].Role != core.RoleUser {
			continue
		}
		if author, exists := authors[messages[i].ClientMessageID]; exists {
			messages[i].AuthorID = author
		}
	}
	return nil
}
