package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

type searchSortStore struct {
	session.Store
	options []session.SearchOptions
}

func (s *searchSortStore) Search(_ context.Context, opts session.SearchOptions) ([]session.SearchResult, error) {
	s.options = append(s.options, opts)
	return nil, nil
}

func TestHandleSessionsSearchSort(t *testing.T) {
	for _, tc := range []struct {
		query  string
		want   session.SearchSort
		status int
	}{
		{"", "", http.StatusOK},
		{"&sort=relevance", session.SearchSortRelevance, http.StatusOK},
		{"&sort=newest", session.SearchSortNewest, http.StatusOK},
		{"&sort=oldest", session.SearchSortOldest, http.StatusOK},
		{"&sort=invalid", "", http.StatusBadRequest},
		{"&sort=updated_at%20DESC", "", http.StatusBadRequest},
	} {
		t.Run(tc.query, func(t *testing.T) {
			store := &searchSortStore{}
			srv := &serveServer{store: store}
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/search?q=needle&limit=99&categories=web&include_archived=1&project_id=p1"+tc.query, nil)
			rr := httptest.NewRecorder()
			srv.handleSessionsSearch(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
			}
			if tc.status != http.StatusOK {
				if len(store.options) != 0 {
					t.Fatal("invalid sort reached store")
				}
				return
			}
			if len(store.options) != 1 {
				t.Fatalf("search calls = %d", len(store.options))
			}
			opts := store.options[0]
			if opts.Sort != tc.want || opts.Limit != 50 || opts.Query != "needle" || !opts.Archived || !opts.ExcludeSubagents || opts.ProjectID != "p1" || len(opts.Categories) != 1 || opts.Categories[0] != "web" {
				t.Fatalf("unexpected options: %#v", opts)
			}
		})
	}
}
