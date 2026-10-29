package serve

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSessionsSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	order := session.SearchSort(strings.TrimSpace(r.URL.Query().Get("sort")))
	switch order {
	case "", session.SearchSortRelevance, session.SearchSortNewest, session.SearchSortOldest:
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "sort must be relevance, newest, or oldest")
		return
	}
	if s.store == nil {
		writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if query == "" {
		writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = min(parsed, 50)
		}
	}

	matches, err := s.store.Search(r.Context(), session.SearchOptions{
		Query:            query,
		Sort:             order,
		Categories:       categories,
		Limit:            limit,
		Archived:         includeArchived,
		ProjectID:        strings.TrimSpace(r.URL.Query().Get("project_id")),
		ExcludeSubagents: true,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid search query")
		return
	}

	type sessionSearchEntry struct {
		webSessionEntry
		Snippet   string `json:"snippet,omitempty"`
		MessageID int64  `json:"message_id,omitempty"`
	}

	attentionBySession := make(map[string]session.AttentionState)
	if batch, ok := session.AsAttentionBatchStore(s.store); ok {
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

	result := make([]sessionSearchEntry, 0, len(matches))
	for _, match := range matches {
		summary := session.SessionSummary{
			ID:                  match.SessionID,
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
		lastMessageAt := sessionSummaryLastMessageAt(summary)
		attention := attentionBySession[summary.ID]
		attentionTerminalAt := int64(0)
		if !attention.TerminalAt.IsZero() {
			attentionTerminalAt = attention.TerminalAt.UnixMilli()
		}
		result = append(result, sessionSearchEntry{
			webSessionEntry: webSessionEntry{
				ID:                       summary.ID,
				Number:                   summary.Number,
				Name:                     summary.Name,
				ShortTitle:               summary.PreferredShortTitle(),
				LongTitle:                summary.PreferredLongTitle(),
				Mode:                     summary.Mode,
				Origin:                   summary.Origin,
				Provider:                 sessionSummaryProviderKey(s.cfgRef, summary),
				ProjectID:                summary.ProjectID,
				ProjectName:              summary.ProjectName,
				Archived:                 summary.Archived,
				Pinned:                   summary.Pinned,
				PinOrder:                 summary.PinOrder,
				CreatedAt:                summary.CreatedAt.UnixMilli(),
				LastMessageAt:            lastMessageAt.UnixMilli(),
				MsgCount:                 summary.MessageCount,
				AttentionStoreInstanceID: attention.StoreInstanceID,
				AttentionSeq:             attention.LatestAttentionSeq,
				AttentionResponseID:      attention.ResponseID,
				AttentionFinalRev:        attention.FinalRev,
				SeenThroughSeq:           attention.SeenThroughSeq,
				SeenRev:                  attention.SeenRev,
				SeenSource:               attention.SeenSource,
				AttentionUnseen:          attention.Unseen,
				AttentionOutcome:         attention.Outcome,
				AttentionTerminalAt:      attentionTerminalAt,
			},
			Snippet:   match.Snippet,
			MessageID: match.MessageID,
		})
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": result})
}
