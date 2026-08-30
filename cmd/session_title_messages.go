package cmd

import (
	"context"
	"sort"

	"github.com/samsaffron/term-llm/internal/session"
)

const sessionTitleMessagePageSize = 80

// loadSessionTitleMessages keeps title generation bounded while preserving both
// the original request and the latest outcome. GetMessages returns the oldest
// rows, so a prefix alone can omit the only substantive assistant response in a
// tool-heavy session.
func loadSessionTitleMessages(ctx context.Context, store session.Store, sessionID string) ([]session.Message, error) {
	head, err := store.GetMessages(ctx, sessionID, sessionTitleMessagePageSize, 0)
	if err != nil || len(head) < sessionTitleMessagePageSize {
		return head, err
	}

	var tail []session.Message
	if pager, ok := store.(session.MessagesDescendingPager); ok {
		tail, err = pager.GetMessagesPageDescending(ctx, sessionID, 0, sessionTitleMessagePageSize)
		if err != nil {
			return nil, err
		}
	} else {
		// Compatibility fallback for custom stores without reverse pagination.
		all, allErr := store.GetMessages(ctx, sessionID, 0, 0)
		if allErr != nil {
			return nil, allErr
		}
		if len(all) <= sessionTitleMessagePageSize {
			return all, nil
		}
		tail = all[len(all)-sessionTitleMessagePageSize:]
	}

	messages := make([]session.Message, 0, len(head)+len(tail))
	messages = append(messages, head...)
	messages = append(messages, tail...)
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Sequence < messages[j].Sequence
	})

	merged := messages[:0]
	seen := make(map[int]struct{}, len(messages))
	for _, message := range messages {
		if _, ok := seen[message.Sequence]; ok {
			continue
		}
		seen[message.Sequence] = struct{}{}
		merged = append(merged, message)
	}
	return merged, nil
}
