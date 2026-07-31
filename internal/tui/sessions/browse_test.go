package sessions

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
)

type ftsRefreshStore struct {
	session.NoopStore
	searchResults    []session.SearchResult
	getCalls         int
	getMessagesCalls int
}

func (s *ftsRefreshStore) Search(ctx context.Context, opts session.SearchOptions) ([]session.SearchResult, error) {
	return s.searchResults, nil
}

func (s *ftsRefreshStore) Get(ctx context.Context, id string) (*session.Session, error) {
	s.getCalls++
	return nil, session.ErrNotFound
}

func (s *ftsRefreshStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]session.Message, error) {
	s.getMessagesCalls++
	return nil, nil
}

func TestUpdate_PasteMsgUpdatesSearchInput(t *testing.T) {
	m := New(nil, 80, 24, nil)
	m.searching = true
	m.searchInput.Focus()

	updated, _ := m.Update(tea.PasteMsg{Content: "release notes"})
	m = updated.(*Model)

	if got := m.searchInput.Value(); got != "release notes" {
		t.Fatalf("expected pasted search query, got %q", got)
	}
}

func TestDoRefresh_FTSSearchUsesSearchMetadataOnly(t *testing.T) {
	updatedAt := time.Now().Add(-5 * time.Minute).Round(time.Second)
	store := &ftsRefreshStore{searchResults: []session.SearchResult{
		{
			SessionID:           "sess-1",
			SessionNumber:       42,
			SessionName:         "Demo",
			Summary:             "search match",
			GeneratedShortTitle: "Message search",
			GeneratedLongTitle:  "Message search result metadata",
			TitleSource:         session.TitleSourceGenerated,
			Snippet:             "discussed **search**\nresult context",
			Provider:            "openai",
			Model:               "gpt-5",
			Mode:                session.ModeChat,
			Status:              session.StatusComplete,
			MessageCount:        7,
			SessionCreatedAt:    updatedAt.Add(-time.Hour),
			UpdatedAt:           updatedAt,
		},
		{
			SessionID:        "sess-1",
			SessionNumber:    42,
			SessionName:      "Demo",
			Summary:          "search match",
			Provider:         "openai",
			Model:            "gpt-5",
			Mode:             session.ModeChat,
			Status:           session.StatusComplete,
			MessageCount:     7,
			SessionCreatedAt: updatedAt.Add(-time.Hour),
			UpdatedAt:        updatedAt,
		},
	}}
	m := New(store, 80, 24, nil)
	m.ftsEnabled = true
	m.searchQuery = "search"
	m.statusFilter = StatusComplete

	refreshed, _ := m.doRefresh()
	m = refreshed.(*Model)

	if store.getCalls != 0 {
		t.Fatalf("Get called %d times, want 0", store.getCalls)
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("GetMessages called %d times, want 0", store.getMessagesCalls)
	}
	if len(m.sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(m.sessions))
	}
	if got := m.sessions[0].MessageCount; got != 7 {
		t.Fatalf("message count = %d, want 7", got)
	}
	if got := m.sessions[0].UpdatedAt; !got.Equal(updatedAt) {
		t.Fatalf("updated_at = %v, want %v", got, updatedAt)
	}
	if got := m.sessions[0].GeneratedLongTitle; got != "Message search result metadata" {
		t.Fatalf("generated long title = %q, want search result metadata", got)
	}
	if got := m.sessions[0].TitleSource; got != session.TitleSourceGenerated {
		t.Fatalf("title source = %q, want %q", got, session.TitleSourceGenerated)
	}

	out := m.View().Content
	if !strings.Contains(out, "[Search: messages]") {
		t.Fatalf("expected message search label, got %q", out)
	}
	if !strings.Contains(out, "Match: discussed search result context") {
		t.Fatalf("expected selected result snippet without match markers, got %q", out)
	}
}

func TestView_FTSSearchShowsNoResultsMessage(t *testing.T) {
	m := New(&ftsRefreshStore{}, 80, 24, nil)
	m.ftsEnabled = true
	m.searchQuery = "agy"

	refreshed, _ := m.doRefresh()
	m = refreshed.(*Model)

	if out := m.View().Content; !strings.Contains(out, `No sessions match "agy"`) {
		t.Fatalf("expected explicit no-results message, got %q", out)
	}
}
