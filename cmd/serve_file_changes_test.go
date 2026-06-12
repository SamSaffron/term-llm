package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/session"
)

func newFileChangesTestServer(t *testing.T) (*serveServer, *filetrack.Store) {
	t.Helper()
	store, err := filetrack.Open(filepath.Join(t.TempDir(), "file_history.db"), filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := &serveServer{fileTrackStoreFn: func() *filetrack.Store { return store }}
	return srv, store
}

func getSessionPath(t *testing.T, srv *serveServer, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	var body map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
	}
	return rr.Code, body
}

func TestSessionFileChangesEndpoints(t *testing.T) {
	srv, store := newFileChangesTestServer(t)
	ctx := context.Background()

	mustStoreRecord := func(rec filetrack.ChangeRecord) {
		t.Helper()
		if _, err := store.RecordChange(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	mustStoreRecord(filetrack.ChangeRecord{
		SessionID: "sess-1", Path: "/work/a.go",
		After: []byte("package a\n"), BeforeMissing: true,
	})
	mustStoreRecord(filetrack.ChangeRecord{
		SessionID: "sess-1", Path: "/work/a.go",
		Before: []byte("package a\n"), After: []byte("package a\n\nfunc A() {}\n"),
	})

	t.Run("list", func(t *testing.T) {
		code, body := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", code, body)
		}
		changes, ok := body["file_changes"].([]any)
		if !ok || len(changes) != 1 {
			t.Fatalf("file_changes = %#v, want one entry", body["file_changes"])
		}
		entry := changes[0].(map[string]any)
		if entry["path"] != "/work/a.go" || entry["kind"] != "create" {
			t.Fatalf("entry = %#v, want create of /work/a.go", entry)
		}
		if entry["adds"].(float64) != 3 {
			t.Fatalf("adds = %v, want 3 (cumulative baseline → current)", entry["adds"])
		}
		if entry["seq"].(float64) != 2 {
			t.Fatalf("seq = %v, want latest path sequence 2", entry["seq"])
		}
	})

	t.Run("diff", func(t *testing.T) {
		code, body := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes/diff?path=/work/a.go")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", code, body)
		}
		if body["kind"] != "create" || body["lang"] != "go" || body["truncated"] != false {
			t.Fatalf("diff meta = %#v", body)
		}
		hunks, ok := body["hunks"].([]any)
		if !ok || len(hunks) == 0 {
			t.Fatalf("hunks = %#v, want at least one", body["hunks"])
		}
		lines := hunks[0].(map[string]any)["lines"].([]any)
		if len(lines) != 3 {
			t.Fatalf("lines = %#v, want 3 added lines", lines)
		}
	})

	t.Run("diff for unknown path", func(t *testing.T) {
		code, _ := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes/diff?path=/nope")
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("diff without path param", func(t *testing.T) {
		code, _ := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes/diff")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("empty session lists nothing", func(t *testing.T) {
		code, body := getSessionPath(t, srv, "/v1/sessions/other/file-changes")
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if changes := body["file_changes"].([]any); len(changes) != 0 {
			t.Fatalf("file_changes = %#v, want empty", changes)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/file-changes", nil)
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
}

func TestSessionFileChangesDisabled(t *testing.T) {
	srv := &serveServer{fileTrackStoreFn: func() *filetrack.Store { return nil }}
	code, _ := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when tracking disabled", code)
	}
}

func TestSessionFileChangesRequireLiveSession(t *testing.T) {
	srv, store := newFileChangesTestServer(t)
	ctx := context.Background()

	sessions, err := session.NewStore(session.Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.Close() })
	live := &session.Session{ID: "live-session", Mode: "chat"}
	if err := sessions.Create(ctx, live); err != nil {
		t.Fatal(err)
	}
	srv.store = sessions

	for _, id := range []string{"live-session", "deleted-session"} {
		if _, err := store.RecordChange(ctx, filetrack.ChangeRecord{
			SessionID: id, Path: "/work/f.txt",
			After: []byte("x\n"), BeforeMissing: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	code, _ := getSessionPath(t, srv, "/v1/sessions/live-session/file-changes")
	if code != http.StatusOK {
		t.Fatalf("live session status = %d, want 200", code)
	}

	// History for sessions that no longer exist must not be retrievable by URL.
	code, _ = getSessionPath(t, srv, "/v1/sessions/deleted-session/file-changes")
	if code != http.StatusNotFound {
		t.Fatalf("deleted session status = %d, want 404", code)
	}
	code, _ = getSessionPath(t, srv, "/v1/sessions/deleted-session/file-changes/diff?path=/work/f.txt")
	if code != http.StatusNotFound {
		t.Fatalf("deleted session diff status = %d, want 404", code)
	}
}
