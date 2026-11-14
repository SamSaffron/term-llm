package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/session"
)

// A sidebar shared with a busy terminal can hold hundreds of ask runs between
// two chats. filter=sidebar must return full pages of rows the sidebar renders
// instead of pages the client filters down to nothing.
func TestHandleSessions_SidebarFilterFillsPages(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Now()
	rows := []struct {
		id       string
		mode     session.SessionMode
		origin   session.SessionOrigin
		archived bool
	}{
		{"ask-1", session.ModeAsk, session.OriginTUI, false},
		{"chat-1", session.ModeChat, session.OriginWeb, false},
		{"ask-2", session.ModeAsk, session.OriginTUI, false},
		{"ask-3", session.ModeAsk, session.OriginTUI, false},
		{"chat-2", session.ModeChat, session.OriginTUI, false},
		{"ask-4", session.ModeAsk, session.OriginTUI, false},
		{"chat-3", session.ModeChat, session.OriginTelegram, false},
		{"archived-chat", session.ModeChat, session.OriginWeb, true},
		{"archived-ask", session.ModeAsk, session.OriginTUI, true},
	}
	for i, row := range rows {
		at := base.Add(-time.Duration(i) * time.Minute)
		if err := store.Create(ctx, &session.Session{ID: row.id, Provider: "mock", Model: "mock-model", Mode: row.mode, Origin: row.origin, CreatedAt: at, UpdatedAt: at, Status: session.StatusActive, Archived: row.archived}); err != nil {
			t.Fatalf("Create(%s): %v", row.id, err)
		}
	}
	srv := &serveServer{store: store}
	get := func(query string) (int, []string, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleSessions(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions?"+query, nil))
		if rr.Code != http.StatusOK {
			return rr.Code, nil, ""
		}
		var body struct {
			Sessions []struct {
				ID string `json:"id"`
			} `json:"sessions"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		ids := make([]string, 0, len(body.Sessions))
		for _, entry := range body.Sessions {
			ids = append(ids, entry.ID)
		}
		return rr.Code, ids, body.NextCursor
	}

	_, ids, cursor := get("filter=sidebar&limit=2")
	if want := []string{"chat-1", "chat-2"}; !reflect.DeepEqual(ids, want) || cursor == "" {
		t.Fatalf("first page = %v cursor=%q, want %v with a cursor", ids, cursor, want)
	}
	_, ids, cursor = get("filter=sidebar&no_project=1&limit=2&cursor=" + url.QueryEscape(cursor))
	if want := []string{"chat-3"}; !reflect.DeepEqual(ids, want) || cursor != "" {
		t.Fatalf("second page = %v cursor=%q, want %v and no cursor", ids, cursor, want)
	}

	// "Show archived sessions" composes with the filter: archived chats return,
	// archived terminal runs stay hidden exactly as the client rule hides them.
	if _, ids, _ = get("filter=sidebar&include_archived=1&limit=10"); !reflect.DeepEqual(ids, []string{"chat-1", "chat-2", "chat-3", "archived-chat"}) {
		t.Fatalf("archived sidebar page = %v", ids)
	}

	// Without the filter the listing is unchanged.
	if _, ids, _ = get("limit=2"); !reflect.DeepEqual(ids, []string{"ask-1", "chat-1"}) {
		t.Fatalf("unfiltered page = %v", ids)
	}
	// Unknown filters are ignored rather than rejected, so a newer client keeps
	// working against an older node.
	if code, ids, _ := get("filter=bogus&limit=2"); code != http.StatusOK || !reflect.DeepEqual(ids, []string{"ask-1", "chat-1"}) {
		t.Fatalf("unknown filter = %d %v, want unfiltered 200", code, ids)
	}
}
