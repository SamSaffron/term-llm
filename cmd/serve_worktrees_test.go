package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func worktreeRootForTest(repo string) func() (string, error) {
	return func() (string, error) { return repo, nil }
}

func TestServeWorktreeCreateCleanLeavesRootChangesInPlace(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("keep this change\n"), 0o644); err != nil {
		t.Fatalf("write dirty root file: %v", err)
	}
	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}

	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees", bytes.NewBufferString(`{"name":"clean-api-test","clean":true}`))
	rec := httptest.NewRecorder()
	srv.handleWorktrees(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Worktree worktreeAPIResponse `json:"worktree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), response.Worktree.Dir, worktree.RemoveOptions{Force: true})
	})

	rootContents, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil || string(rootContents) != "keep this change\n" {
		t.Fatalf("root file after clean API create = %q, %v; want unchanged dirty contents", rootContents, err)
	}
	worktreeContents, err := os.ReadFile(filepath.Join(response.Worktree.Dir, "file.txt"))
	if err != nil || string(worktreeContents) != "base\n" {
		t.Fatalf("clean API worktree file = %q, %v; want committed base contents", worktreeContents, err)
	}
}

func TestServeWorktreeCreateCleanIsNotBlockedByActiveRootRun(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	releaseRun, err := processRootCheckoutLeases.acquireRun(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRun()
	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}

	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees", bytes.NewBufferString(`{"name":"clean-during-run","clean":true}`))
	rec := httptest.NewRecorder()
	srv.handleWorktrees(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clean create during root run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Worktree worktreeAPIResponse `json:"worktree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), response.Worktree.Dir, worktree.RemoveOptions{Force: true})
	})

	moveReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees", bytes.NewBufferString(`{"name":"move-during-run","clean":false}`))
	moveRec := httptest.NewRecorder()
	srv.handleWorktrees(moveRec, moveReq)
	if moveRec.Code != http.StatusConflict {
		t.Fatalf("move create during root run status = %d body=%s", moveRec.Code, moveRec.Body.String())
	}
}

func TestServeWorktreeHandlersCreateListDiffDelete(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("dirty before API create\n"), 0o644); err != nil {
		t.Fatalf("write dirty root file: %v", err)
	}
	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}

	createReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees", bytes.NewBufferString(`{"name":"api-test"}`))
	createRec := httptest.NewRecorder()
	srv.handleWorktrees(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createRec.Code, createRec.Body.String())
	}
	var createResp struct {
		Worktree worktreeAPIResponse `json:"worktree"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createResp.Worktree.Dir == "" {
		t.Fatalf("create response missing worktree dir: %s", createRec.Body.String())
	}
	rootContents, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil || string(rootContents) != "base\n" {
		t.Fatalf("root file after API create = %q, %v; want clean base contents", rootContents, err)
	}
	moved, err := os.ReadFile(filepath.Join(createResp.Worktree.Dir, "file.txt"))
	if err != nil || string(moved) != "dirty before API create\n" {
		t.Fatalf("moved API worktree file = %q, %v", moved, err)
	}
	if err := os.WriteFile(filepath.Join(createResp.Worktree.Dir, "new.txt"), []byte("hello from api\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/worktrees", nil)
	listRec := httptest.NewRecorder()
	srv.handleWorktrees(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "api-test") {
		t.Fatalf("list body = %s, want created worktree", listRec.Body.String())
	}

	diffReq := httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+createResp.Worktree.Dir, nil)
	diffRec := httptest.NewRecorder()
	srv.handleWorktreeDiff(diffRec, diffReq)
	if diffRec.Code != http.StatusOK {
		t.Fatalf("diff status = %d body=%s", diffRec.Code, diffRec.Body.String())
	}
	if !strings.Contains(diffRec.Body.String(), "hello from api") {
		t.Fatalf("diff body = %s, want untracked file diff", diffRec.Body.String())
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelReq := httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+createResp.Worktree.Dir, nil).WithContext(cancelCtx)
	cancelRec := httptest.NewRecorder()
	srv.handleWorktreeDiff(cancelRec, cancelReq)
	if cancelRec.Body.Len() != 0 {
		t.Fatalf("cancelled diff wrote response: %s", cancelRec.Body.String())
	}

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/worktrees?force=1&dir="+createResp.Worktree.Dir, nil)
		deleteRec := httptest.NewRecorder()
		srv.handleWorktrees(deleteRec, deleteReq)
		if deleteRec.Code != http.StatusOK {
			t.Fatalf("delete status = %d body=%s", deleteRec.Code, deleteRec.Body.String())
		}
	})
}

type worktreeAPIResponse struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type countingWorktreeListStore struct {
	session.NoopStore
	summaries []session.SessionSummary
	listCalls int
	lastOpts  session.ListOptions
}

func (s *countingWorktreeListStore) List(ctx context.Context, opts session.ListOptions) ([]session.SessionSummary, error) {
	s.listCalls++
	s.lastOpts = opts
	return append([]session.SessionSummary(nil), s.summaries...), nil
}

func TestServeWorktreeListBatchesSessionUsage(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt1, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "batched-one"})
	if err != nil {
		t.Fatalf("Create wt1: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt1.Dir, worktree.RemoveOptions{Force: true}) })
	wt2, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "batched-two"})
	if err != nil {
		t.Fatalf("Create wt2: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt2.Dir, worktree.RemoveOptions{Force: true}) })

	updatedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	store := &countingWorktreeListStore{summaries: []session.SessionSummary{
		{ID: "sess-one", Number: 11, GeneratedShortTitle: "Investigate cache invalidation", WorktreeDir: wt1.Dir, Status: session.StatusActive, UpdatedAt: updatedAt},
		{ID: "sess-two", Number: 12, Name: "two", WorktreeDir: wt2.Dir, Status: session.StatusActive},
		{ID: "sess-complete", Number: 13, Name: "complete", WorktreeDir: wt2.Dir, Status: session.StatusComplete},
	}}
	srv := &serveServer{store: store, worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodGet, "/v1/worktrees", nil)
	rec := httptest.NewRecorder()
	srv.handleWorktrees(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listCalls != 1 {
		t.Fatalf("session List calls = %d, want 1", store.listCalls)
	}
	if store.lastOpts.Status != session.StatusActive || store.lastOpts.Archived || store.lastOpts.Limit != 10000 {
		t.Fatalf("List options = %+v, want active non-archived limit 10000", store.lastOpts)
	}

	var resp struct {
		Worktrees []struct {
			Dir   string                  `json:"dir"`
			InUse []worktree.InUseSession `json:"in_use,omitempty"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	assertInUse := func(dir, id, title string) {
		t.Helper()
		for _, row := range resp.Worktrees {
			if !sameServePath(row.Dir, dir) {
				continue
			}
			if len(row.InUse) != 1 || row.InUse[0].ID != id {
				t.Fatalf("worktree %s in_use = %+v, want %s", dir, row.InUse, id)
			}
			if row.InUse[0].Title != title {
				t.Fatalf("worktree %s title = %q, want %q", dir, row.InUse[0].Title, title)
			}
			return
		}
		t.Fatalf("worktree %s missing from response: %+v", dir, resp.Worktrees)
	}
	assertInUse(wt1.Dir, "sess-one", "Investigate cache invalidation")
	assertInUse(wt2.Dir, "sess-two", "two")
}

func TestServeWorktreeMergeCleanupSemantics(t *testing.T) {
	tests := []struct {
		name        string
		bodyKeep    bool
		wantRemoved bool
	}{
		{name: "default removes", wantRemoved: true},
		{name: "keep preserves", bodyKeep: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newGitRepoForBindingTest(t)
			wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "serve-" + strings.ReplaceAll(tt.name, " ", "-")})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(wt.Dir) })
			if err := os.WriteFile(filepath.Join(wt.Dir, "merged.txt"), []byte("serve merge\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(worktreeMergeRequest{Dir: wt.Dir, Keep: tt.bodyKeep})
			srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
			req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			srv.handleWorktreeMerge(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Result  worktree.MergeResult   `json:"result"`
				Cleanup worktree.CleanupResult `json:"cleanup"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Cleanup.Removed != tt.wantRemoved || !resp.Result.Applied {
				t.Fatalf("response = %+v, want removed=%v", resp, tt.wantRemoved)
			}
			_, statErr := os.Stat(wt.Dir)
			if tt.wantRemoved && !os.IsNotExist(statErr) {
				t.Fatalf("worktree stat = %v, want removed", statErr)
			}
			if !tt.wantRemoved && statErr != nil {
				t.Fatalf("kept worktree missing: %v", statErr)
			}
		})
	}
}

func TestServeWorktreeMergeInUseReturnsCleanup(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "serve-in-use"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
	if err := os.WriteFile(filepath.Join(wt.Dir, "merged.txt"), []byte("serve merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(context.Background(), &session.Session{ID: "bound", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CWD: wt.Dir, WorktreeDir: wt.Dir, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{store: store, worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	rec := httptest.NewRecorder()
	srv.handleWorktreeMerge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Cleanup worktree.CleanupResult `json:"cleanup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Cleanup.Removed || len(resp.Cleanup.InUse) != 1 || resp.Cleanup.InUse[0].ID != "bound" {
		t.Fatalf("cleanup = %+v, want bound session", resp.Cleanup)
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("worktree should remain: %v", err)
	}
}

func TestServeWorktreeMergeExcludesCallingSessionFromCleanup(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "serve-caller-cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
	if err := os.WriteFile(filepath.Join(wt.Dir, "merged.txt"), []byte("serve merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := &session.Project{Name: "Caller cleanup", CanonicalDir: repo}
	if err := store.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.Create(context.Background(), &session.Session{ID: "caller", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindSessionWorkspace(context.Background(), "caller", session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: wt.Dir, WorktreeDir: wt.Dir}); err != nil {
		t.Fatal(err)
	}

	runtimeSession := &session.Session{ID: "caller", CWD: wt.Dir, WorktreeDir: wt.Dir}
	rt := &serveRuntime{sessionMeta: runtimeSession}
	mgr := &serveSessionManager{sessions: map[string]*serveRuntime{"caller": rt}}
	srv := &serveServer{store: store, sessionMgr: mgr, worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	req.Header.Set(requestSessionIDHeader, "caller")
	rec := httptest.NewRecorder()
	srv.handleWorktreeMerge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Cleanup worktree.CleanupResult `json:"cleanup"`
		Session *session.Session       `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Session == nil || resp.Session.ID != "caller" || resp.Session.WorktreeDir != "" || !sameServePath(resp.Session.CWD, repo) {
		t.Fatalf("response session = %#v, want root fallback %q", resp.Session, repo)
	}
	if !resp.Cleanup.Removed {
		t.Fatalf("cleanup = %+v, want removed", resp.Cleanup)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree stat = %v, want removed", err)
	}
	if rt.sessionMeta == nil || rt.sessionMeta.WorktreeDir != "" || !sameServePath(rt.sessionMeta.CWD, repo) {
		t.Fatalf("runtime session = %#v, want root fallback %q", rt.sessionMeta, repo)
	}
	persisted, err := store.Get(context.Background(), "caller")
	if err != nil || persisted.WorktreeDir != "" || !sameServePath(persisted.CWD, repo) {
		t.Fatalf("persisted session = %#v, %v; want root fallback %q", persisted, err, repo)
	}
}

func TestServeWorktreeMergeKeepsSourceWhenCallerRuntimeIsBusy(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "serve-busy-caller"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
	if err := os.WriteFile(filepath.Join(wt.Dir, "merged.txt"), []byte("serve merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := &session.Project{Name: "Busy caller", CanonicalDir: repo}
	if err := store.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.Create(context.Background(), &session.Session{ID: "busy-caller", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindSessionWorkspace(context.Background(), "busy-caller", session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: wt.Dir, WorktreeDir: wt.Dir}); err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{sessionMeta: &session.Session{ID: "busy-caller", CWD: wt.Dir, WorktreeDir: wt.Dir}}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	mgr := &serveSessionManager{sessions: map[string]*serveRuntime{"busy-caller": rt}}
	srv := &serveServer{store: store, sessionMgr: mgr, worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	req.Header.Set(requestSessionIDHeader, "busy-caller")
	rec := httptest.NewRecorder()
	srv.handleWorktreeMerge(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "source checkout was kept") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("source worktree was removed after failed caller move: %v", err)
	}
	persisted, err := store.Get(context.Background(), "busy-caller")
	if err != nil || !sameServePath(persisted.WorktreeDir, wt.Dir) {
		t.Fatalf("persisted session = %#v, %v; want source binding", persisted, err)
	}
}

func TestServeWorktreeMergeBlocksActiveRootRun(t *testing.T) {

	repo := newGitRepoForBindingTest(t)

	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-block"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	worktreeDir := wt.Dir
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), worktreeDir, worktree.RemoveOptions{Force: true})
	})

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	if err := store.Create(context.Background(), &session.Session{
		ID:        "root-active",
		Provider:  "mock",
		Model:     "tiny",
		Mode:      session.ModeChat,
		CWD:       repo,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := store.Create(context.Background(), &session.Session{
		ID: "worktree-active", Provider: "mock", Model: "tiny", Mode: session.ModeChat,
		CWD: worktreeDir, WorktreeDir: worktreeDir, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	nonGitDir := t.TempDir()
	if err := store.Create(context.Background(), &session.Session{
		ID: "unrelated-nongit", Provider: "mock", Model: "tiny", Mode: session.ModeChat,
		CWD: nonGitDir, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	projects, ok := session.AsProjectStore(store)
	if !ok {
		t.Fatal("project store unavailable")
	}
	project := &session.Project{Name: "Mutation owner", CanonicalDir: repo}
	if err := projects.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	missingDir := filepath.Join(filepath.Dir(worktreeDir), "missing-owned-worktree")
	missing := &session.Session{ID: "missing-worktree-active", Provider: "mock", Model: "tiny", Mode: session.ModeChat, ProjectID: project.ID, CWD: missingDir, WorktreeDir: missingDir, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
	if err := store.Create(context.Background(), missing); err != nil {
		t.Fatal(err)
	}
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	mgr.mu.Lock()
	for _, id := range []string{"root-active", "worktree-active", "unrelated-nongit", "missing-worktree-active"} {
		mgr.sessions[id] = &serveRuntime{activeInterrupt: &runtimeInterruptState{}}
	}
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		for _, id := range []string{"root-active", "worktree-active", "unrelated-nongit", "missing-worktree-active"} {
			delete(mgr.sessions, id)
		}
		mgr.mu.Unlock()
	}()
	// Leave active runs in the root, a managed checkout, a missing checkout that
	// falls back to this project, and an unrelated non-Git directory.
	srv := &serveServer{store: store, sessionMgr: mgr, worktreeRootFn: worktreeRootForTest(repo)}
	active := srv.activeRootRunsForWorktreeMerge(context.Background(), repo)
	for _, want := range []string{"root-active", "missing-worktree-active"} {
		if !slices.Contains(active, want) {
			t.Fatalf("active runs %v missing %s", active, want)
		}
	}
	for _, unwanted := range []string{"worktree-active", "unrelated-nongit"} {
		if slices.Contains(active, unwanted) {
			t.Fatalf("run outside the root checkout %q blocked repository mutation: %v", unwanted, active)
		}
	}

	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+worktreeDir+`"}`))
	mergeRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(mergeRec, mergeReq)
	if mergeRec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", mergeRec.Code, mergeRec.Body.String())
	}
	if !strings.Contains(mergeRec.Body.String(), "root-active") || !strings.Contains(mergeRec.Body.String(), "root_checkout_active_runs") {
		t.Fatalf("merge body = %s, want overridable active-run conflict", mergeRec.Body.String())
	}

	promoteReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/promote", bytes.NewBufferString(`{"dir":"`+worktreeDir+`","branch":"blocked-promote"}`))
	promoteRec := httptest.NewRecorder()
	srv.handleWorktreePromote(promoteRec, promoteReq)
	if promoteRec.Code != http.StatusConflict {
		t.Fatalf("promote status = %d body=%s", promoteRec.Code, promoteRec.Body.String())
	}
	if !strings.Contains(promoteRec.Body.String(), "root-active") {
		t.Fatalf("promote body = %s, want active root session id", promoteRec.Body.String())
	}

	forceReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+worktreeDir+`","keep":true,"force":true}`))
	forceRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(forceRec, forceReq)
	if forceRec.Code != http.StatusOK {
		t.Fatalf("forced merge status = %d body=%s", forceRec.Code, forceRec.Body.String())
	}
}

func TestServeWorktreeDeleteUsesRepositoryMutationLease(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "delete-lease"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
	release, blocked, err := processRootCheckoutLeases.tryAcquireMutation(repo, false)
	if err != nil || blocked != rootCheckoutMutationAvailable {
		t.Fatalf("acquire test mutation lease: blocked=%v err=%v", blocked, err)
	}
	defer release()
	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	rr := httptest.NewRecorder()
	srv.handleWorktreeDelete(rr, httptest.NewRequest(http.MethodDelete, "/v1/worktrees?dir="+url.QueryEscape(wt.Dir)+"&force=1", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete during mutation status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("blocked delete removed worktree: %v", err)
	}
	mergeRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(mergeRec, httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`","keep":true,"force":true}`)))
	if mergeRec.Code != http.StatusConflict {
		t.Fatalf("forced merge during mutation status=%d body=%s", mergeRec.Code, mergeRec.Body.String())
	}
}

func TestServeWorktreeMutationsBlockNonWebRootRunLease(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "non-web-block"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true})
	})

	releaseRun, err := processRootCheckoutLeases.acquireRun(context.Background(), repo)
	if err != nil {
		t.Fatalf("acquire root run lease: %v", err)
	}
	defer releaseRun()

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`","keep":true}`))
	mergeRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(mergeRec, mergeReq)
	if mergeRec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", mergeRec.Code, mergeRec.Body.String())
	}

	promoteReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/promote", bytes.NewBufferString(`{"dir":"`+wt.Dir+`","branch":"blocked-non-web"}`))
	promoteRec := httptest.NewRecorder()
	srv.handleWorktreePromote(promoteRec, promoteReq)
	if promoteRec.Code != http.StatusConflict {
		t.Fatalf("promote status = %d body=%s", promoteRec.Code, promoteRec.Body.String())
	}

	forceReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`","keep":true,"force":true}`))
	forceRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(forceRec, forceReq)
	if forceRec.Code != http.StatusOK {
		t.Fatalf("forced merge status = %d body=%s", forceRec.Code, forceRec.Body.String())
	}
}

func TestRootCheckoutLeaseIgnoresLinkedWorktreeInsideMainRoot(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	linkedDir := filepath.Join(repo, "nested-worktree")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", linkedDir, "HEAD")
	t.Cleanup(func() {
		runGitForBindingTest(t, repo, "worktree", "remove", "--force", linkedDir)
	})

	var leases rootCheckoutLeaseRegistry
	releaseMutation, blocked, err := leases.tryAcquireMutation(repo, false)
	if err != nil || blocked != rootCheckoutMutationAvailable {
		t.Fatalf("acquire mutation lease: blocked=%v err=%v", blocked, err)
	}
	defer releaseMutation()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseRun, err := leases.acquireRun(ctx, linkedDir)
	if err != nil {
		t.Fatalf("linked-worktree run was blocked by main checkout mutation: %v", err)
	}
	releaseRun()
}

func TestServeWorktreeMergeExclusiveLeaseBlocksNewRootRuns(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-admission"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true})
	})
	if err := os.WriteFile(filepath.Join(wt.Dir, "lease.txt"), []byte("lease test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mutationAdmitted := make(chan struct{})
	continueMutation := make(chan struct{})
	defer func() {
		select {
		case <-continueMutation:
		default:
			close(continueMutation)
		}
	}()
	srv := &serveServer{
		worktreeRootFn: worktreeRootForTest(repo),
		rootMutationAdmitted: func() {
			close(mutationAdmitted)
			<-continueMutation
		},
	}
	requestBody, _ := json.Marshal(worktreeMergeRequest{Dir: wt.Dir, Keep: true})
	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewReader(requestBody))
	mergeRec := httptest.NewRecorder()
	mergeDone := make(chan struct{})
	go func() {
		defer close(mergeDone)
		srv.handleWorktreeMerge(mergeRec, mergeReq)
	}()

	select {
	case <-mutationAdmitted:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not acquire the exclusive root lease")
	}

	platforms := []string{"web", "telegram", "jobs"}
	started := make([]chan struct{}, len(platforms))
	attempted := make([]chan struct{}, len(platforms))
	unexpectedlyStarted := make(chan string, len(platforms))
	runErrs := make(chan error, len(platforms))
	for i, platform := range platforms {
		started[i] = make(chan struct{})
		attempted[i] = make(chan struct{})
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		runtime := &serveRuntime{
			provider:     provider,
			engine:       llm.NewEngine(provider, nil),
			defaultModel: "mock-model",
			platform:     platform,
		}
		go func(i int, rt *serveRuntime) {
			close(attempted[i])
			_, runErr := rt.RunWithEventsAndStart(
				context.Background(),
				false,
				false,
				[]llm.Message{llm.UserText("wait for merge")},
				llm.Request{WorkingDir: repo},
				func() {
					close(started[i])
					unexpectedlyStarted <- platform
				},
				nil,
			)
			runErrs <- runErr
		}(i, runtime)
	}
	for i := range attempted {
		<-attempted[i]
	}
	select {
	case platform := <-unexpectedlyStarted:
		t.Fatalf("%s run became active while merge held the exclusive root lease", platform)
	case <-time.After(100 * time.Millisecond):
	}

	close(continueMutation)
	select {
	case <-mergeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not complete")
	}
	if mergeRec.Code != http.StatusOK {
		t.Fatalf("merge status = %d body=%s", mergeRec.Code, mergeRec.Body.String())
	}
	for i, platform := range platforms {
		select {
		case <-started[i]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s run did not become active after merge released the lease", platform)
		}
	}
	for range platforms {
		if err := <-runErrs; err != nil {
			t.Fatalf("root run failed: %v", err)
		}
	}
}

func TestServeWorktreeMergeConflictReturnsRicherResult(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-conflict-api"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.Dir) })
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root api change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	runGitForBindingTest(t, repo, "add", "file.txt")
	runGitForBindingTest(t, repo, "commit", "-m", "root api change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree api change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	rec := httptest.NewRecorder()
	srv.handleWorktreeMerge(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error    string               `json:"error"`
		Result   worktree.MergeResult `json:"result"`
		Recovery struct {
			Kind              string `json:"kind"`
			Question          string `json:"question"`
			YesLabel          string `json:"yes_label"`
			NoLabel           string `json:"no_label"`
			Available         bool   `json:"available"`
			UnavailableReason string `json:"unavailable_reason"`
		} `json:"recovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode merge response: %v", err)
	}
	if resp.Error != "conflicts" || !resp.Result.ConflictReset || resp.Result.RootDir == "" || resp.Result.WorktreeDir == "" || len(resp.Result.Conflicts) == 0 {
		t.Fatalf("merge conflict response = %+v", resp)
	}
	if resp.Recovery.Kind != "conflict" || resp.Recovery.Question == "" || resp.Recovery.YesLabel == "" || resp.Recovery.NoLabel == "" || resp.Recovery.Available || resp.Recovery.UnavailableReason == "" {
		t.Fatalf("merge recovery offer = %+v", resp.Recovery)
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("root status after API conflict = %q, want clean", status)
	}
}

func TestServeWorktreeAssistedMergeMovesCallerAndReturnsSharedPrompt(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "assisted-merge-api"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() {
		runGitForBindingTest(t, repo, "cherry-pick", "--quit")
		runGitForBindingTest(t, repo, "reset", "--hard", "HEAD")
		_ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true})
	})
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root assisted change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForBindingTest(t, repo, "add", "file.txt")
	runGitForBindingTest(t, repo, "commit", "-m", "root assisted change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree assisted change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(context.Background(), &session.Session{ID: "assisted-caller", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CWD: wt.Dir, WorktreeDir: wt.Dir, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	runtimeSession := &session.Session{ID: "assisted-caller", CWD: wt.Dir, WorktreeDir: wt.Dir}
	rt := &serveRuntime{sessionMeta: runtimeSession}
	mgr := &serveSessionManager{sessions: map[string]*serveRuntime{"assisted-caller": rt}}
	srv := &serveServer{store: store, sessionMgr: mgr, worktreeRootFn: worktreeRootForTest(repo)}
	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	mergeReq.Header.Set(requestSessionIDHeader, "assisted-caller")
	mergeRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(mergeRec, mergeReq)
	if mergeRec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", mergeRec.Code, mergeRec.Body.String())
	}
	if !strings.Contains(mergeRec.Body.String(), `"available":true`) {
		t.Fatalf("merge recovery was not available: %s", mergeRec.Body.String())
	}

	recoverReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/assisted-merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	recoverReq.Header.Set(requestSessionIDHeader, "assisted-caller")
	recoverRec := httptest.NewRecorder()
	srv.handleWorktreeAssistedMerge(recoverRec, recoverReq)
	if recoverRec.Code != http.StatusOK {
		t.Fatalf("assisted merge status = %d body=%s", recoverRec.Code, recoverRec.Body.String())
	}
	var resp struct {
		Result  worktree.AssistedMergeResult `json:"result"`
		Session *session.Session             `json:"session"`
		Notice  string                       `json:"notice"`
		Prompt  string                       `json:"prompt"`
	}
	if err := json.Unmarshal(recoverRec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Session == nil || resp.Session.WorktreeDir != "" || !sameServePath(resp.Session.CWD, repo) {
		t.Fatalf("recovery session = %#v, want root %q", resp.Session, repo)
	}
	if rt.sessionMeta == nil || rt.sessionMeta.WorktreeDir != "" || !sameServePath(rt.sessionMeta.CWD, repo) {
		t.Fatalf("runtime recovery session = %#v, want root %q", rt.sessionMeta, repo)
	}
	if len(resp.Result.ChangedFiles) == 0 || !strings.Contains(resp.Prompt, "failed `/worktree promote`") || !strings.Contains(resp.Prompt, "file.txt") || resp.Notice == "" {
		t.Fatalf("assisted recovery response = %+v", resp)
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); !strings.Contains(status, "file.txt") {
		t.Fatalf("root status after assisted recovery = %q", status)
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("source worktree should remain: %v", err)
	}
}

func TestServeWorktreeAssistedMergeRequiresBoundCaller(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "assisted-unbound-api"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/assisted-merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
	rec := httptest.NewRecorder()
	srv.handleWorktreeAssistedMerge(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "assisted_recovery_unavailable") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServeWorktreeAssistedMergeRetriesFromRootAfterDirtyPreflight(t *testing.T) {
	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "assisted-root-retry-api"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runGitForBindingTest(t, repo, "reset", "--hard", "HEAD")
		_ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true})
	})
	if err := os.WriteFile(filepath.Join(wt.Dir, "recovered.txt"), []byte("recover me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("temporarily dirty root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(context.Background(), &session.Session{ID: "retry-caller", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CWD: wt.Dir, WorktreeDir: wt.Dir, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store, worktreeRootFn: worktreeRootForTest(repo)}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/assisted-merge", bytes.NewBufferString(`{"dir":"`+wt.Dir+`"}`))
		req.Header.Set(requestSessionIDHeader, "retry-caller")
		rec := httptest.NewRecorder()
		srv.handleWorktreeAssistedMerge(rec, req)
		return rec
	}

	blocked := request()
	var blockedResp struct {
		Error   string           `json:"error"`
		Session *session.Session `json:"session"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &blockedResp); err != nil {
		t.Fatal(err)
	}
	if blocked.Code != http.StatusConflict || blockedResp.Error != "root_dirty" || blockedResp.Session == nil || blockedResp.Session.WorktreeDir != "" || !sameServePath(blockedResp.Session.CWD, repo) {
		t.Fatalf("dirty preflight status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	persisted, err := store.Get(context.Background(), "retry-caller")
	if err != nil || persisted == nil || persisted.WorktreeDir != "" || !sameServePath(persisted.CWD, repo) {
		t.Fatalf("moved session = %#v err=%v", persisted, err)
	}

	runGitForBindingTest(t, repo, "checkout", "--", "file.txt")
	retried := request()
	if retried.Code != http.StatusOK || !strings.Contains(retried.Body.String(), `"prompt"`) {
		t.Fatalf("root-bound retry status=%d body=%s", retried.Code, retried.Body.String())
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); !strings.Contains(status, "recovered.txt") {
		t.Fatalf("root status after retry = %q", status)
	}
}

func TestServeWorktreePromoteReturnsRootResult(t *testing.T) {

	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "promote-api"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "api-new.txt"), []byte("hello promote api\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/promote", bytes.NewBufferString(`{"dir":"`+wt.Dir+`","branch":"feature-api-promote"}`))
	rec := httptest.NewRecorder()
	srv.handleWorktreePromote(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result  worktree.PromoteResult `json:"result"`
		Cleanup worktree.CleanupResult `json:"cleanup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode promote response: %v", err)
	}
	if resp.Result.Branch != "feature-api-promote" || resp.Result.RootDir == "" || resp.Result.WorktreeDir == "" || !resp.Result.Applied || !resp.Cleanup.Removed || resp.Result.OriginalWorktreeStillExists {
		t.Fatalf("promote response = %+v", resp)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatalf("promoted worktree stat = %v, want removed", err)
	}
	if got := strings.TrimSpace(runGitForBindingTest(t, repo, "branch", "--show-current")); got != "feature-api-promote" {
		t.Fatalf("root branch = %q, want feature-api-promote", got)
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); !strings.Contains(status, "A  api-new.txt") {
		t.Fatalf("root status = %q, want promoted staged api-new.txt", status)
	}
}

func TestServeWorktreeHandlersRejectUnmanagedDir(t *testing.T) {

	repo := newGitRepoForBindingTest(t)

	externalDir := filepath.Join(t.TempDir(), "external-worktree")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", externalDir, "HEAD")
	t.Cleanup(func() { _ = os.RemoveAll(externalDir) })

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	tests := []struct {
		name string
		req  *http.Request
		run  func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "diff",
			req:  httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+url.QueryEscape(externalDir), nil),
			run:  srv.handleWorktreeDiff,
		},
		{
			name: "merge",
			req:  httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+externalDir+`"}`)),
			run:  srv.handleWorktreeMerge,
		},
		{
			name: "promote",
			req:  httptest.NewRequest(http.MethodPost, "/v1/worktrees/promote", bytes.NewBufferString(`{"dir":"`+externalDir+`","branch":"unsafe"}`)),
			run:  srv.handleWorktreePromote,
		},
		{
			name: "delete",
			req:  httptest.NewRequest(http.MethodDelete, "/v1/worktrees?force=1&dir="+url.QueryEscape(externalDir), nil),
			run:  srv.handleWorktrees,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.run(rec, tt.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
	if _, err := os.Stat(externalDir); err != nil {
		t.Fatalf("external worktree should not be removed: %v", err)
	}
}

func TestServeWorktreeHandlersRejectForeignManagedDir(t *testing.T) {

	repo := newGitRepoForBindingTest(t)
	foreignRepo := newGitRepoForBindingTest(t)
	foreignWT, err := worktree.Create(context.Background(), foreignRepo, worktree.CreateOptions{Name: "foreign"})
	if err != nil {
		t.Fatalf("Create foreign worktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreignWT.Dir) })

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+url.QueryEscape(foreignWT.Dir), nil)
	rec := httptest.NewRecorder()
	srv.handleWorktreeDiff(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}
