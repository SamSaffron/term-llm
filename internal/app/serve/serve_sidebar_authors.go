package serve

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSidebar(w http.ResponseWriter, r *http.Request) {
	createdBy, valid := requestCreatedBy(w, r)
	if !valid {
		return
	}
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	perProject, _ := strconv.Atoi(r.URL.Query().Get("per_project"))
	if r.URL.Query().Get("refresh_status") == "1" {
		s.clearProjectStatusCache()
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	groups, err := projects.Sidebar(r.Context(), session.SidebarOptions{
		CreatedBy:               createdBy,
		PerProject:              perProject,
		IncludeArchivedProjects: r.URL.Query().Get("include_archived_projects") == "1",
		IncludeArchivedSessions: r.URL.Query().Get("include_archived_sessions") == "1",
	})
	if err != nil {
		writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not load projects and conversations")
		return
	}
	const recentPageSize = 30
	recent, err := s.store.List(r.Context(), session.ListOptions{
		CreatedBy:        createdBy,
		Limit:            recentPageSize + 1,
		Archived:         r.URL.Query().Get("include_archived_sessions") == "1",
		SortByActivity:   true,
		ExcludeSubagents: true,
	})
	if err != nil {
		writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not load recent conversations")
		return
	}
	recentNextCursor := ""
	if len(recent) > recentPageSize {
		recentNextCursor = session.EncodeRecentSessionCursor(recent[recentPageSize-1])
		recent = recent[:recentPageSize]
	}
	attentionBySession := make(map[string]session.AttentionState)
	if batch, ok := session.AsAttentionBatchStore(s.store); ok {
		ids := make([]string, 0, len(recent))
		for _, group := range groups {
			for _, summary := range group.Sessions {
				ids = append(ids, summary.ID)
			}
		}
		for _, summary := range recent {
			ids = append(ids, summary.ID)
		}
		if states, batchErr := batch.GetAttentionBatch(r.Context(), ids); batchErr == nil {
			attentionBySession = states
		} else {
			log.Printf("[serve] project sidebar attention projection unavailable: %v", batchErr)
		}
	}
	// Availability is intentionally derived rather than persisted.
	statusCtx, cancelStatus := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancelStatus()
	for i := range groups {
		if groups[i].Project != nil {
			api := s.cachedProjectStatus(statusCtx, *groups[i].Project, i < maxEagerProjectStatusChecks)
			groups[i].Project = &api.Project
		}
		for j := range groups[i].Sessions {
			applySessionSummaryAttention(&groups[i].Sessions[j], attentionBySession[groups[i].Sessions[j].ID])
			// Legacy sessions predate provider_key and retain Provider.Name() as a
			// display label (for example, "ChatGPT (gpt-5.6-sol, effort=low)").
			// The runtime picker must receive the canonical key, never that label.
			groups[i].Sessions[j].ProviderKey = sessionSummaryProviderKey(s.cfgRef, groups[i].Sessions[j])
		}
	}
	recentEntries := make([]webSessionEntry, 0, len(recent))
	for _, summary := range recent {
		entry := s.webSessionEntryFromSummary(summary)
		applyWebSessionAttention(&entry, attentionBySession[summary.ID])
		recentEntries = append(recentEntries, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"groups":             s.sidebarAuthorGroups(r.Context(), groups, recentEntries),
		"recent_sessions":    recentEntries,
		"recent_next_cursor": recentNextCursor,
	})
}
