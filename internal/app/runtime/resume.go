package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// RepairChildResumeHistory appends synthetic errors for interrupted tool calls
// before the child transcript is resumed by a provider.
func RepairChildResumeHistory(ctx context.Context, store session.Store, id string) ([]core.Message, error) {
	if store == nil {
		return nil, errors.New("agent transcript persistence is unavailable")
	}
	sess, err := store.Get(ctx, id)
	if err != nil || sess == nil {
		return nil, fmt.Errorf("load child session %q: %w", id, err)
	}
	stored, err := session.LoadActiveMessages(ctx, store, sess)
	if err != nil {
		return nil, fmt.Errorf("load child transcript: %w", err)
	}
	seen := make(map[string]bool)
	for i := range stored {
		if stored[i].Role == core.RoleTool {
			for _, p := range stored[i].Parts {
				if p.Type == core.PartToolResult && p.ToolResult != nil {
					seen[p.ToolResult.ID] = true
				}
			}
		}
	}
	var repaired []core.Message
	history := make([]core.Message, 0, len(stored))
	for i := range stored {
		msg := stored[i].ToLLMMessage()
		history = append(history, msg)
		if msg.Role != core.RoleAssistant {
			continue
		}
		for _, p := range msg.Parts {
			if p.Type != core.PartToolCall || p.ToolCall == nil || p.ToolCall.ID == "" || seen[p.ToolCall.ID] {
				continue
			}
			seen[p.ToolCall.ID] = true
			synthetic := core.ToolErrorMessage(p.ToolCall.ID, p.ToolCall.Name, "interrupted; side effects unknown — verify before retrying", nil)
			repaired = append(repaired, synthetic)
		}
	}
	for _, msg := range repaired {
		if err := store.AddMessage(ctx, id, session.NewMessage(id, msg, -1)); err != nil {
			return nil, fmt.Errorf("persist child tool-call repair: %w", err)
		}
		history = append(history, msg)
	}
	return history, nil
}
