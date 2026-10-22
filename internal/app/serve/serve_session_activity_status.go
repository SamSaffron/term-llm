package serve

import (
	"context"

	"github.com/samsaffron/term-llm/internal/session"
)

// Active compaction is polled independently of the sidebar catalog. In
// particular, navigating away from a compacting child must not look like idle.
// Ordinary child response runs/interactions keep their existing exclusion.
func (s *serveServer) compactionStatusSessions(ctx context.Context, ids []string, listed map[string]bool) ([]session.SessionSummary, error) {
	var missing []string
	wanted := make(map[string]bool)
	for _, id := range ids {
		if !listed[id] {
			missing = append(missing, id)
			wanted[id] = true
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	rows, err := s.store.List(ctx, session.ListOptions{IDs: missing, Limit: -1, Archived: true, SortByActivity: true})
	if err != nil {
		return nil, err
	}
	out := make([]session.SessionSummary, 0, len(rows))
	for _, row := range rows {
		if wanted[row.ID] {
			out = append(out, row)
			delete(wanted, row.ID)
		}
	}
	return out, nil
}
