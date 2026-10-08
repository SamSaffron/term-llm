package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/hub"
)

// Use compact protocol JSON so the count tests remain below the per-page byte
// cap, even when a node ignores the requested 200-item limit.
func writeAttentionBudgetPage(w http.ResponseWriter, count, sequence int, more bool, title string) {
	items := make([]map[string]any, count)
	for i := range items {
		items[i] = map[string]any{"session_id": fmt.Sprintf("page-%d-item-%d", sequence, i)}
		if title != "" {
			items[i]["long_title"] = title
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"protocol_version": 2, "store_instance_id": "store-new", "snapshot_version": 10,
		"items": items, "has_more": more, "next_cursor": strconv.Itoa(sequence),
	})
}

func TestHubAttentionCollectorResourceLimitsAreNodeScoped(t *testing.T) {
	tests := []struct {
		name         string
		wantRequests int64
		page         func(http.ResponseWriter, *http.Request, int)
	}{
		{
			name: "endless advancing cursor", wantRequests: hubAttentionMaxPages,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				writeAttentionBudgetPage(w, 1, n, true, "")
			},
		},
		{
			name: "oversized single page count", wantRequests: 1,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				writeAttentionBudgetPage(w, hubAttentionMaxItems+1, n, false, "")
			},
		},
		{
			name: "cumulative count across kinds", wantRequests: 51,
			page: func(w http.ResponseWriter, r *http.Request, n int) {
				if r.URL.Query().Get("kind") == "running" {
					writeAttentionBudgetPage(w, 1, n, false, "")
					return
				}
				writeAttentionBudgetPage(w, hubAttentionPageSize, n, n%25 != 0, "")
			},
		},
		{
			name: "cumulative payload across kinds", wantRequests: 16,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				// Each page is below 2MiB, but 16 pages (including JSON
				// overhead) exceed the 16MiB cumulative payload budget.
				writeAttentionBudgetPage(w, 1, n, n%8 != 0, strings.Repeat("x", 1<<20))
			},
		},
		{
			name: "payload budget survives snapshot retry", wantRequests: 17,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 9 {
					w.WriteHeader(http.StatusConflict)
					return
				}
				writeAttentionBudgetPage(w, 1, n, n != 8, strings.Repeat("x", 1<<20))
			},
		},
		{
			name: "oversized single page payload", wantRequests: 1,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				writeAttentionBudgetPage(w, 1, n, false, strings.Repeat("x", hubAttentionMaxBytes))
			},
		},
		{
			name: "page budget survives snapshot retry", wantRequests: hubAttentionMaxPages,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 61 {
					w.WriteHeader(http.StatusConflict)
					return
				}
				writeAttentionBudgetPage(w, 1, n, n != 60, "")
			},
		},
		{
			name: "item budget survives snapshot retry", wantRequests: 52,
			page: func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 31 {
					w.WriteHeader(http.StatusConflict)
					return
				}
				writeAttentionBudgetPage(w, hubAttentionPageSize, n, n != 30, "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				// A deterministic failsafe makes the old unbounded collector
				// fail this test without relying on its eight-second timeout.
				if n > tt.wantRequests {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if r.URL.Query().Get("limit") != strconv.Itoa(hubAttentionPageSize) {
					t.Errorf("requested limit = %q", r.URL.Query().Get("limit"))
				}
				tt.page(w, r, int(n))
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count := 0
				if r.URL.Query().Get("kind") == "unseen" {
					count = 1
				}
				writeAttentionBudgetPage(w, count, 1, false, "")
			}))
			defer good.Close()
			srv := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{
				{ID: "bad", Name: "Bad", Source: hub.SourceConfig, URL: bad.URL},
				{ID: "good", Name: "Good", Source: hub.SourceConfig, URL: good.URL},
			}}), nil)
			store := openProjection(t)
			srv.attentionStore = store
			ctx := context.Background()
			if err := store.ReplaceNode(ctx, "bad", "store-old", "old-etag", []hub.SessionActivity{
				{SessionID: "cached", Kind: "running", ShortTitle: "previous snapshot"},
			}); err != nil {
				t.Fatal(err)
			}
			previous, err := store.GetSync(ctx, "bad")
			if err != nil {
				t.Fatal(err)
			}
			if failures := srv.collectAttention(ctx); failures != 1 {
				t.Fatalf("failures = %d, want 1", failures)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("bad requests = %d, want %d", got, tt.wantRequests)
			}
			state, err := store.GetSync(ctx, "bad")
			if err != nil {
				t.Fatal(err)
			}
			if state.LastError != "node attention collection exceeded resource limits" || state.LastErrorAt.IsZero() {
				t.Fatalf("bad node sync = %+v", state)
			}
			if state.StoreInstanceID != previous.StoreInstanceID || state.ETag != previous.ETag || !state.LastSuccessAt.Equal(previous.LastSuccessAt) {
				t.Fatalf("failed sync changed previous snapshot: before=%+v after=%+v", previous, state)
			}
			goodState, err := store.GetSync(ctx, "good")
			if err != nil || !hubAttentionSyncHealthy(goodState) || goodState.LastError != "" {
				t.Fatalf("healthy node sync = %+v, %v", goodState, err)
			}
			activities, _, err := store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(activities) != 2 {
				t.Fatalf("partial snapshot installed: %d activities", len(activities))
			}
			for _, activity := range activities {
				if activity.NodeID == "bad" && (activity.SessionID != "cached" || activity.ShortTitle != "previous snapshot") {
					t.Fatalf("cached activity replaced: %+v", activity)
				}
			}
			assertAttentionBudgetHealthyAPI(t, srv)
		})
	}
}

func assertAttentionBudgetHealthyAPI(t *testing.T, srv *hubServer) {
	t.Helper()
	recorder := httptest.NewRecorder()
	srv.handleHubAttention(recorder, httptest.NewRequest(http.MethodGet, "/api/attention", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("attention API status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Nodes []hubAttentionNodeView  `json:"nodes"`
		Inbox []hubAttentionInboxItem `json:"inbox"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Nodes) != 2 || len(response.Inbox) != 1 || response.Inbox[0].NodeID != "good" {
		t.Fatalf("healthy node unavailable in API: %+v", response)
	}
	for _, node := range response.Nodes {
		if node.Stale != (node.NodeID == "bad") {
			t.Fatalf("node stale state = %+v", node)
		}
	}
}

func TestHubAttentionCollectorAllowsPaginationAtResourceBounds(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1))
		if n > hubAttentionMaxPages {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 98 unseen pages, one input-required page, and one running page:
		// exactly 100 requests and 10,000 activities, all legitimately paginated.
		wantCursor := ""
		if n > 1 && n < hubAttentionMaxPages-1 {
			wantCursor = strconv.Itoa(n - 1)
		}
		if r.URL.Query().Get("cursor") != wantCursor {
			t.Errorf("page %d cursor = %q, want %q", n, r.URL.Query().Get("cursor"), wantCursor)
		}
		writeAttentionBudgetPage(w, hubAttentionMaxItems/hubAttentionMaxPages, n, n < hubAttentionMaxPages-2, "")
	}))
	defer server.Close()
	node := hub.Node{ID: "alpha", Name: "Alpha", URL: server.URL + "/chat"}
	if err := node.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := newHubServer(nil, nil)
	srv.attentionStore = openProjection(t)
	if err := srv.collectNodeAttention(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	activities, _, err := srv.attentionStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != hubAttentionMaxPages || len(activities) != hubAttentionMaxItems {
		t.Fatalf("requests=%d activities=%d", requests.Load(), len(activities))
	}
}
