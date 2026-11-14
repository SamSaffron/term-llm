package serve

import (
	"context"
	"net/http"

	"github.com/term-llm/term-llm/internal/app/serve/sessionsearch"
	"github.com/term-llm/term-llm/internal/session"
)

type sessionSearchEntry struct {
	webSessionEntry
	Snippet   string `json:"snippet,omitempty"`
	MessageID int64  `json:"message_id,omitempty"`
}

func (s *serveServer) handleSessionsSearch(w http.ResponseWriter, r *http.Request) {
	sessionsearch.SearchHandler(w, r, s.store, parseSidebarSessionCategories, func(summary session.SessionSummary, attention session.AttentionState, match session.SearchResult) sessionSearchEntry {
		entry := s.webSessionEntryFromSummary(summary)
		applyWebSessionAttention(&entry, attention)
		return sessionSearchEntry{webSessionEntry: entry, Snippet: match.Snippet, MessageID: match.MessageID}
	}, func(ctx context.Context, rows []sessionSearchEntry) {
		entries := make([]webSessionEntry, len(rows))
		for i := range rows {
			entries[i] = rows[i].webSessionEntry
		}
		s.enrichSessionAuthors(ctx, entries)
		for i := range rows {
			rows[i].webSessionEntry = entries[i]
		}
	}, writeJSONConditional, writeOpenAIError)
}
