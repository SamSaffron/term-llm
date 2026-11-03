// Package sessionsearch handles session-search HTTP validation, querying and projection.
package sessionsearch

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/serve/authorship"
	"github.com/samsaffron/term-llm/internal/session"
)

// SearchHandler validates and projects a filtered, attention-enriched session search page.
func SearchHandler[T any](w http.ResponseWriter, r *http.Request, store session.Store,
	parseCategories func(string, bool) ([]string, error),
	project func(session.SessionSummary, session.AttentionState, session.SearchResult) T,
	enrich func(context.Context, []T), write func(http.ResponseWriter, *http.Request, int, any), fail func(http.ResponseWriter, int, string, string)) {
	createdBy, err := authorship.CreatedByFilter(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if createdBy != "" {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	order := session.SearchSort(strings.TrimSpace(r.URL.Query().Get("sort")))
	switch order {
	case "", session.SearchSortRelevance, session.SearchSortNewest, session.SearchSortOldest:
	default:
		fail(w, http.StatusBadRequest, "invalid_request_error", "sort must be relevance, newest, or oldest")
		return
	}
	if store == nil {
		write(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}

	categories, err := parseCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if query == "" {
		write(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = min(parsed, 50)
		}
	}

	matches, err := store.Search(r.Context(), session.SearchOptions{
		CreatedBy:        createdBy,
		Query:            query,
		Sort:             order,
		Categories:       categories,
		Limit:            limit,
		Archived:         includeArchived,
		ProjectID:        strings.TrimSpace(r.URL.Query().Get("project_id")),
		ExcludeSubagents: true,
	})
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request_error", "invalid search query")
		return
	}

	attentionBySession := make(map[string]session.AttentionState)
	if batch, ok := session.AsAttentionBatchStore(store); ok {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.SessionID)
		}
		if states, batchErr := batch.GetAttentionBatch(r.Context(), ids); batchErr == nil {
			attentionBySession = states
		} else {
			log.Printf("[serve] search attention projection unavailable: %v", batchErr)
		}
	}

	result := make([]T, 0, len(matches))
	for _, match := range matches {
		summary := session.SessionSummary{
			ID:                  match.SessionID,
			CreatedBy:           match.CreatedBy,
			Number:              match.SessionNumber,
			Name:                match.SessionName,
			Summary:             match.Summary,
			GeneratedShortTitle: match.GeneratedShortTitle,
			GeneratedLongTitle:  match.GeneratedLongTitle,
			TitleSource:         match.TitleSource,
			Provider:            match.Provider,
			ProviderKey:         match.ProviderKey,
			Model:               match.Model,
			Mode:                match.Mode,
			Origin:              match.Origin,
			Archived:            match.Archived,
			Pinned:              match.Pinned,
			PinOrder:            match.PinOrder,
			MessageCount:        match.MessageCount,
			Status:              match.Status,
			ProjectID:           match.ProjectID,
			ProjectName:         match.ProjectName,
			CreatedAt:           match.SessionCreatedAt,
			UpdatedAt:           match.UpdatedAt,
			LastMessageAt:       match.LastMessageAt,
		}
		result = append(result, project(summary, attentionBySession[summary.ID], match))
	}

	enrich(r.Context(), result)
	write(w, r, http.StatusOK, map[string]any{"sessions": result})
}
