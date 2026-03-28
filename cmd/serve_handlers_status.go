package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSessionsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session store not available")
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")

	sessions, err := s.store.List(r.Context(), session.ListOptions{
		Limit:      100,
		Archived:   includeArchived,
		Categories: categories,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}

	// Collect active session IDs from in-memory state without touching runtimes.
	activeIDs := s.activeSessionIDs()

	type statusEntry struct {
		ID         string `json:"id"`
		ShortTitle string `json:"short_title"`
		LongTitle  string `json:"long_title"`
		ActiveRun  bool   `json:"active_run,omitempty"`
		MsgCount   int    `json:"message_count"`
	}

	result := make([]statusEntry, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, statusEntry{
			ID:         sess.ID,
			ShortTitle: sess.PreferredShortTitle(),
			LongTitle:  sess.PreferredLongTitle(),
			ActiveRun:  activeIDs[sess.ID],
			MsgCount:   sess.MessageCount,
		})
	}

	body, _ := json.Marshal(map[string]any{"sessions": result})

	// ETag for conditional requests — avoids re-transmitting unchanged data.
	hash := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(hash[:8]) + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// activeSessionIDs returns the set of session IDs that have an active run,
// either via the session manager (in-memory runtimes) or via detached
// response runs. It does NOT touch runtimes (no TTL refresh).
func (s *serveServer) activeSessionIDs() map[string]bool {
	result := make(map[string]bool)
	if s.sessionMgr != nil {
		for id, active := range s.sessionMgr.ActiveSessionIDs() {
			if active {
				result[id] = true
			}
		}
	}
	if s.responseRuns != nil {
		for id := range s.responseRuns.ActiveSessionIDs() {
			result[id] = true
		}
	}
	return result
}
