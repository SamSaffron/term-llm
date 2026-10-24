package session

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// AgentRunResumable deliberately does not equate Started with saved history: runners
// enter execution before provider initialization and transcript persistence.
// Failed runs need durable assistant/tool history, not just the initial prompt.
func AgentRunResumable(ctx context.Context, runStore AgentRunStore, r AgentRun) bool {
	switch r.Status {
	case "completed", "turn_limit", "cancelled", "interrupted":
		return true
	case "failed":
		if !r.Started {
			return false
		}
		store, ok := runStore.(Store)
		if !ok {
			return false
		}
		sess, err := store.Get(ctx, r.ID)
		if err != nil || sess == nil {
			return false
		}
		messages, err := LoadActiveMessages(ctx, store, sess)
		if err != nil {
			return false
		}
		for _, msg := range messages {
			if msg.Role == core.RoleAssistant || msg.Role == core.RoleTool {
				return true
			}
		}
	}
	return false
}
