package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
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

func TestHandleSessionsSelectedSessionSideloadsStartupMetadata(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := session.NewStore(session.Config{
		Enabled: true,
		Path:    filepath.Join(t.TempDir(), "sessions.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessionStore.Close() })

	fileStore, err := filetrack.Open(filepath.Join(t.TempDir(), "file_history.db"), filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fileStore.Close() })

	now := time.Now()
	clean := &session.Session{ID: "clean-session", Summary: "Clean", CreatedAt: now, UpdatedAt: now}
	changed := &session.Session{ID: "changed-session", Summary: "Changed", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
	for _, sess := range []*session.Session{clean, changed} {
		if err := sessionStore.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.ID, err)
		}
	}
	if _, err := fileStore.RecordChange(ctx, filetrack.ChangeRecord{
		SessionID: changed.ID,
		Path:      "/work/changed.go",
		Before:    []byte("old\nkeep\n"),
		After:     []byte("new\nkeep\nextra\n"),
	}); err != nil {
		t.Fatal(err)
	}
	planStore := sessionStore.(session.PlanSnapshotStore)
	if _, err := planStore.SavePlanSnapshot(ctx, changed.ID, planpkg.Snapshot{Plan: []planpkg.Step{
		{Step: "Finished", Status: planpkg.StatusCompleted},
		{Step: "Working", Status: planpkg.StatusInProgress},
		{Step: "Later", Status: planpkg.StatusPending},
	}}); err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{
		store:            sessionStore,
		fileTrackStoreFn: func() *filetrack.Store { return fileStore },
	}

	type fileChangeSummary struct {
		FileCount int `json:"file_count"`
		Adds      int `json:"adds"`
		Dels      int `json:"dels"`
	}
	type planSummary struct {
		Version        int64  `json:"version"`
		StepCount      int    `json:"step_count"`
		CompletedSteps int    `json:"completed_steps"`
		Position       int    `json:"position"`
		State          string `json:"state"`
	}
	type selectedSession struct {
		ID                string            `json:"id"`
		Number            int64             `json:"number"`
		FileChangeSummary fileChangeSummary `json:"file_change_summary"`
		PlanSummary       *planSummary      `json:"plan_summary"`
	}
	request := func(selector string) selectedSession {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions?selected_session="+selector, nil)
		rr := httptest.NewRecorder()
		srv.handleSessions(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET selected_session=%s: status=%d body=%s", selector, rr.Code, rr.Body.String())
		}
		var body struct {
			Sessions []json.RawMessage `json:"sessions"`
			Selected selectedSession   `json:"selected_session"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(selector, "selected_only=1") && len(body.Sessions) != 0 {
			t.Fatalf("selected_only returned %d sidebar sessions, want none", len(body.Sessions))
		}
		return body.Selected
	}

	cleanSelected := request(clean.ID)
	if cleanSelected.ID != clean.ID || cleanSelected.FileChangeSummary != (fileChangeSummary{}) {
		t.Fatalf("clean selected session = %#v", cleanSelected)
	}
	if cleanSelected.PlanSummary != nil {
		t.Fatalf("clean plan summary = %#v, want nil", cleanSelected.PlanSummary)
	}

	// Numeric route selectors are resolved in this bulk request, before the
	// browser is allowed to issue any session-scoped requests.
	changedSelected := request(strconv.FormatInt(changed.Number, 10) + "&selected_only=1")
	if changedSelected.ID != changed.ID || changedSelected.Number != changed.Number {
		t.Fatalf("numeric selected session = %#v", changedSelected)
	}
	if got := changedSelected.FileChangeSummary; got.FileCount != 1 || got.Adds != 2 || got.Dels != 1 {
		t.Fatalf("file change summary = %#v, want count=1 adds=2 dels=1", got)
	}
	if got := changedSelected.PlanSummary; got == nil || got.Version <= 0 || got.StepCount != 3 || got.CompletedSteps != 1 || got.Position != 2 || got.State != "in_progress" {
		t.Fatalf("plan summary = %#v", got)
	}
}

func TestHandleSessionsWithoutSelectionDoesNotLoadFileChanges(t *testing.T) {
	// A plain sidebar listing has no selected_session sideload. This guards the
	// many-row endpoint against per-session file-history or plan lookups.
	ctx := context.Background()
	store, err := session.NewStore(session.Config{
		Enabled: true,
		Path:    filepath.Join(t.TempDir(), "sessions.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, id := range []string{"one", "two"} {
		if err := store.Create(ctx, &session.Session{ID: id, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	srv := &serveServer{store: store, fileTrackStoreFn: func() *filetrack.Store {
		t.Fatal("plain session listing must not open file tracking")
		return nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
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
	pngBefore := []byte("\x89PNG\r\n\x1a\nbefore")
	pngAfter := []byte("\x89PNG\r\n\x1a\nafter")
	mustStoreRecord(filetrack.ChangeRecord{
		SessionID: "sess-image", Path: "/work/preview.png",
		Before: pngBefore, After: pngAfter,
	})
	mustStoreRecord(filetrack.ChangeRecord{
		SessionID: "sess-image", Path: "/work/created.gif",
		BeforeMissing: true, After: []byte("GIF89acreated"),
	})
	mustStoreRecord(filetrack.ChangeRecord{
		SessionID: "sess-image", Path: "/work/deleted.gif",
		Before: []byte("GIF89adeleted"), AfterMissing: true,
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

	t.Run("image diff and content", func(t *testing.T) {
		code, body := getSessionPath(t, srv, "/v1/sessions/sess-image/file-changes/diff?path=/work/preview.png")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", code, body)
		}
		if body["image"] != true || body["truncated"] != false {
			t.Fatalf("image diff meta = %#v", body)
		}
		if hunks, ok := body["hunks"].([]any); !ok || len(hunks) != 0 {
			t.Fatalf("image hunks = %#v, want empty", body["hunks"])
		}

		for _, tc := range []struct {
			side string
			want []byte
		}{{"before", pngBefore}, {"after", pngAfter}} {
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-image/file-changes/content?path=/work/preview.png&side="+tc.side, nil)
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, req)
			if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("%s content status = %d, type = %q", tc.side, rr.Code, rr.Header().Get("Content-Type"))
			}
			if rr.Header().Get("Cache-Control") != "private, no-store" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s content security headers = %#v", tc.side, rr.Header())
			}
			if string(rr.Body.Bytes()) != string(tc.want) {
				t.Fatalf("%s content = %q, want %q", tc.side, rr.Body.Bytes(), tc.want)
			}
		}
	})

	t.Run("created and deleted GIF content", func(t *testing.T) {
		for _, tc := range []struct {
			path string
			side string
			want string
		}{
			{path: "/work/created.gif", side: "after", want: "GIF89acreated"},
			{path: "/work/deleted.gif", side: "before", want: "GIF89adeleted"},
		} {
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-image/file-changes/content?path="+tc.path+"&side="+tc.side, nil)
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, req)
			if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/gif" || rr.Body.String() != tc.want {
				t.Fatalf("%s %s: status=%d type=%q body=%q", tc.path, tc.side, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
			}
		}
	})

	t.Run("image content rejects invalid or unavailable side", func(t *testing.T) {
		for _, path := range []string{
			"/v1/sessions/sess-image/file-changes/content?path=/work/preview.png&side=middle",
			"/v1/sessions/sess-image/file-changes/content?path=/work/created.gif&side=before",
			"/v1/sessions/sess-image/file-changes/content?path=/work/deleted.gif&side=after",
		} {
			code, _ := getSessionPath(t, srv, path)
			if code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400", path, code)
			}
		}
	})

	t.Run("image content rejects non-image file", func(t *testing.T) {
		code, _ := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes/content?path=/work/a.go&side=after")
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
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
	code, _ = getSessionPath(t, srv, "/v1/sessions/deleted-session/file-changes/content?path=/work/f.txt&side=after")
	if code != http.StatusNotFound {
		t.Fatalf("deleted session content status = %d, want 404", code)
	}
}
