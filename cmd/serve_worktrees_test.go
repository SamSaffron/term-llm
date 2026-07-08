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

func TestServeWorktreeHandlersCreateListDiffDelete(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repo := newGitRepoForBindingTest(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	srv := &serveServer{}

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

func TestServeWorktreeMergeBlocksActiveRootRun(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repo := newGitRepoForBindingTest(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

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
	srv := &serveServer{store: store, sessionMgr: mgr}

	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/merge", bytes.NewBufferString(`{"dir":"`+worktreeDir+`"}`))
	rec := httptest.NewRecorder()
	srv.handleWorktreeMerge(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("merge status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "root-active") {
		t.Fatalf("merge body = %s, want active root session id", rec.Body.String())
	}
}

func TestServeWorktreeHandlersRejectUnmanagedDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repo := newGitRepoForBindingTest(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	externalDir := filepath.Join(t.TempDir(), "external-worktree")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", externalDir, "HEAD")
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", "--force", externalDir)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		_ = cmd.Run()
	})

	srv := &serveServer{}
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
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	repo := newGitRepoForBindingTest(t)
	foreignRepo := newGitRepoForBindingTest(t)
	foreignWT, err := worktree.Create(context.Background(), foreignRepo, worktree.CreateOptions{Name: "foreign"})
	if err != nil {
		t.Fatalf("Create foreign worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), foreignWT.Dir, worktree.RemoveOptions{Force: true})
	})

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	srv := &serveServer{}
	req := httptest.NewRequest(http.MethodGet, "/v1/worktrees/diff?dir="+url.QueryEscape(foreignWT.Dir), nil)
	rec := httptest.NewRecorder()
	srv.handleWorktreeDiff(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}
