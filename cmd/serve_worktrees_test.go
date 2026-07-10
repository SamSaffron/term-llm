package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func worktreeRootForTest(repo string) func() (string, error) {
	return func() (string, error) { return repo, nil }
}

func TestServeWorktreeHandlersCreateListDiffDelete(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForBindingTest(t)
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

	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/worktrees?force=1&dir="+createResp.Worktree.Dir, nil)
	deleteRec := httptest.NewRecorder()
	srv.handleWorktrees(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

type worktreeAPIResponse struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
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
		t.Run(tt.name, func(t *testing.T) {
			repo := newGitRepoForBindingTest(t)
			wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "serve-" + strings.ReplaceAll(tt.name, " ", "-")})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
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
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
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

func TestServeWorktreeMergeBlocksActiveRootRun(t *testing.T) {
	t.Parallel()

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
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["root-active"] = &serveRuntime{activeInterrupt: &runtimeInterruptState{}}
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		delete(mgr.sessions, "root-active")
		mgr.mu.Unlock()
	}()
	// Leave one active root run registered and exercise both root-mutating endpoints.
	srv := &serveServer{store: store, sessionMgr: mgr, worktreeRootFn: worktreeRootForTest(repo)}

	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+worktreeDir+`"}`))
	mergeRec := httptest.NewRecorder()
	srv.handleWorktreeMerge(mergeRec, mergeReq)
	if mergeRec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", mergeRec.Code, mergeRec.Body.String())
	}
	if !strings.Contains(mergeRec.Body.String(), "root-active") {
		t.Fatalf("merge body = %s, want active root session id", mergeRec.Body.String())
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
}

func TestServeWorktreeMergeConflictReturnsRicherResult(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForBindingTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-conflict-api"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })
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
		Error  string               `json:"error"`
		Result worktree.MergeResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode merge response: %v", err)
	}
	if resp.Error != "conflicts" || !resp.Result.ConflictReset || resp.Result.RootDir == "" || resp.Result.WorktreeDir == "" || len(resp.Result.Conflicts) == 0 {
		t.Fatalf("merge conflict response = %+v", resp)
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("root status after API conflict = %q, want clean", status)
	}
}

func TestServeWorktreePromoteReturnsRootResult(t *testing.T) {
	t.Parallel()

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
		Result worktree.PromoteResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode promote response: %v", err)
	}
	if resp.Result.Branch != "feature-api-promote" || resp.Result.RootDir == "" || resp.Result.WorktreeDir == "" || !resp.Result.Applied {
		t.Fatalf("promote response = %+v", resp.Result)
	}
	if got := strings.TrimSpace(runGitForBindingTest(t, repo, "branch", "--show-current")); got != "feature-api-promote" {
		t.Fatalf("root branch = %q, want feature-api-promote", got)
	}
	if status := runGitForBindingTest(t, repo, "status", "--porcelain"); !strings.Contains(status, "A  api-new.txt") {
		t.Fatalf("root status = %q, want promoted staged api-new.txt", status)
	}
}

func TestServeWorktreeHandlersRejectUnmanagedDir(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForBindingTest(t)

	externalDir := filepath.Join(t.TempDir(), "external-worktree")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", externalDir, "HEAD")
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", "--force", externalDir)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		_ = cmd.Run()
	})

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
	t.Parallel()

	repo := newGitRepoForBindingTest(t)
	foreignRepo := newGitRepoForBindingTest(t)
	foreignWT, err := worktree.Create(context.Background(), foreignRepo, worktree.CreateOptions{Name: "foreign"})
	if err != nil {
		t.Fatalf("Create foreign worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), foreignWT.Dir, worktree.RemoveOptions{Force: true})
	})

	srv := &serveServer{worktreeRootFn: worktreeRootForTest(repo)}
	req := httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+url.QueryEscape(foreignWT.Dir), nil)
	rec := httptest.NewRecorder()
	srv.handleWorktreeDiff(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}
