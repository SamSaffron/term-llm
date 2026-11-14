package serve

import (
	"context"
	"net/http"

	"github.com/term-llm/term-llm/internal/app/serve/authorship"
	"github.com/term-llm/term-llm/internal/session"
)

func identityID(ctx context.Context) string { return authorship.ViewerID(ctx) }

func requestCreatedBy(w http.ResponseWriter, r *http.Request) (string, bool) {
	return authorship.RequireCreatedBy(w, r, writeOpenAIError)
}

func (s *serveServer) enrichSessionAuthors(ctx context.Context, entries []webSessionEntry) {
	starters := map[string]string{}
	for _, entry := range entries {
		starters[entry.ID] = entry.createdByID
	}
	fields := s.authors.Sessions(ctx, s.store, starters)
	for i := range entries {
		entries[i].SessionFields = fields[entries[i].ID]
	}
}

func (s *serveServer) sidebarAuthorGroups(ctx context.Context, groups []session.SidebarGroup, recent []webSessionEntry) []authorship.SidebarGroup {
	starters := map[string]string{}
	for _, row := range recent {
		starters[row.ID] = row.createdByID
	}
	out, fields := s.authors.Sidebar(ctx, s.store, groups, starters)
	for i := range recent {
		recent[i].SessionFields = fields[recent[i].ID]
	}
	return out
}
