package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/worktree"
)

type worktreeCreateRequest struct {
	Name   string `json:"name"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
}

type worktreeMergeRequest struct {
	Dir     string `json:"dir"`
	Commit  bool   `json:"commit"`
	Message string `json:"message"`
	Keep    bool   `json:"keep"`
}

type worktreePromoteRequest struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
}

type worktreeRow struct {
	Name       string                  `json:"name"`
	Dir        string                  `json:"dir"`
	RepoRoot   string                  `json:"repo_root,omitempty"`
	Branch     string                  `json:"branch,omitempty"`
	Detached   bool                    `json:"detached"`
	Base       string                  `json:"base,omitempty"`
	HeadSHA    string                  `json:"head_sha,omitempty"`
	DirtyFiles int                     `json:"dirty_files"`
	Root       bool                    `json:"root,omitempty"`
	InUse      []worktree.InUseSession `json:"in_use,omitempty"`
}

// currentGitRoot resolves the serve process's startup repository once and shares
// it between the HTML capability bootstrap and every worktree API handler.
func (s *serveServer) currentGitRoot() (string, error) {
	s.worktreeRootOnce.Do(func() {
		cwd, err := os.Getwd()
		if s.worktreeRootFn != nil {
			cwd, err = s.worktreeRootFn()
		}
		if err != nil {
			s.worktreeRootErr = err
			return
		}
		if !worktree.IsGitRepo(cwd) {
			s.worktreeRootErr = fmt.Errorf("not a git repository")
			return
		}
		s.worktreeRoot, s.worktreeRootErr = worktree.MainRepoRoot(cwd)
	})
	return s.worktreeRoot, s.worktreeRootErr
}

func (s *serveServer) currentGitRootOr409(w http.ResponseWriter) (string, bool) {
	root, err := s.currentGitRoot()
	if err != nil {
		writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "not a git repository")
		return "", false
	}
	return root, true
}

func managedWorktreeForRoot(root, dir string) (*worktree.Worktree, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree dir: %w", err)
	}
	wt, err := worktree.Get(abs)
	if err != nil {
		return nil, fmt.Errorf("invalid worktree dir: %w", err)
	}
	if !sameServePath(wt.RepoRoot, root) {
		return nil, fmt.Errorf("worktree does not belong to the current repository")
	}
	managedRoot, err := worktree.ManagedRoot(root)
	if err != nil {
		return nil, fmt.Errorf("resolve managed worktree root: %w", err)
	}
	managedRoot, err = canonicalizeWorktreeBoundary(managedRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve managed worktree root: %w", err)
	}
	wtDir, err := canonicalizeWorktreeBoundary(wt.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree dir: %w", err)
	}
	if !pathWithinDir(wtDir, managedRoot) || wtDir == managedRoot {
		return nil, fmt.Errorf("worktree is not managed by term-llm")
	}
	wt.Dir = wtDir
	return wt, nil
}

func canonicalizeWorktreeBoundary(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	return "", err
}

func sameServePath(a, b string) bool {
	aa, errA := canonicalizeWorktreeBoundary(a)
	bb, errB := canonicalizeWorktreeBoundary(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return aa == bb
}

func (s *serveServer) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleWorktreeList(w, r)
	case http.MethodPost:
		s.handleWorktreeCreate(w, r)
	case http.MethodDelete:
		s.handleWorktreeDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *serveServer) handleWorktreeList(w http.ResponseWriter, r *http.Request) {
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	rows := []worktreeRow{{Name: "root", Dir: root, RepoRoot: root, Root: true}}
	items, err := worktree.List(root)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	dirs := make([]string, 0, len(items))
	for _, wt := range items {
		dirs = append(dirs, wt.Dir)
	}
	inUseByDir, _ := worktree.InUseByDir(r.Context(), s.store, dirs)
	for _, wt := range items {
		rows = append(rows, worktreeRow{Name: wt.Name, Dir: wt.Dir, RepoRoot: wt.RepoRoot, Branch: wt.Branch, Detached: wt.Detached, Base: wt.Base, HeadSHA: wt.HeadSHA, DirtyFiles: wt.DirtyFiles, InUse: inUseByDir[wt.Dir]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": rows})
}

func (s *serveServer) handleWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	var req worktreeCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	opts := worktree.CreateOptions{Name: req.Name, Base: req.Base, Branch: req.Branch, SetupTimeout: 10 * time.Minute, MoveChanges: true}
	if opts.Base == "" {
		opts.Base = "HEAD"
	}
	if script := strings.TrimSpace(os.Getenv("TERM_LLM_WORKTREE_SETUP")); script != "" {
		opts.SetupScript = script
	}
	wt, err := worktree.Create(r.Context(), root, opts)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, worktree.ErrExists) {
			status = http.StatusConflict
		}
		writeOpenAIError(w, status, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktree": worktreeRow{Name: wt.Name, Dir: wt.Dir, RepoRoot: wt.RepoRoot, Branch: wt.Branch, Detached: wt.Detached, Base: wt.Base, HeadSHA: wt.HeadSHA, DirtyFiles: wt.DirtyFiles}})
}

func (s *serveServer) handleWorktreeDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	wt, err := managedWorktreeForRoot(root, r.URL.Query().Get("dir"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	diff, err := worktree.Diff(wt.Dir)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": diff})
}

func (s *serveServer) handleWorktreeMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	var req worktreeMergeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Dir) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "dir is required")
		return
	}
	wt, err := managedWorktreeForRoot(root, req.Dir)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if activeRootSessions := s.activeRootRunsForWorktreeMerge(r.Context(), root); len(activeRootSessions) > 0 {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", fmt.Sprintf("root checkout has active session run(s): %s", strings.Join(activeRootSessions, ", ")))
		return
	}
	opts := worktree.MergeOptions{Commit: req.Commit, Message: req.Message}
	var res worktree.MergeResult
	var cleanup worktree.CleanupResult
	if req.Keep {
		res, err = worktree.MergeBack(r.Context(), wt.Dir, opts)
	} else {
		res, cleanup, err = worktree.MergeBackAndCleanup(r.Context(), wt.Dir, opts, s.store, "")
	}
	if errors.Is(err, worktree.ErrConflict) {
		message := "root checkout was reset cleanly after conflicts"
		if !res.ConflictReset {
			message = "merge conflicts occurred and automatic cleanup did not fully complete; inspect the root checkout"
		}
		writeJSON(w, http.StatusConflict, map[string]any{"result": res, "error": "conflicts", "message": message})
		return
	}
	if errors.Is(err, worktree.ErrRootDirty) {
		writeJSON(w, http.StatusConflict, map[string]any{"result": res, "error": "root_dirty", "message": err.Error()})
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "cleanup": cleanup})
}

func (s *serveServer) activeRootRunsForWorktreeMerge(ctx context.Context, root string) []string {
	if s == nil || s.sessionMgr == nil || s.store == nil {
		return nil
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	s.sessionMgr.mu.Lock()
	ids := make([]string, 0, len(s.sessionMgr.sessions))
	for id, rt := range s.sessionMgr.sessions {
		if rt != nil && rt.hasActiveRun() {
			ids = append(ids, id)
		}
	}
	s.sessionMgr.mu.Unlock()
	var active []string
	for _, id := range ids {
		sess, err := s.store.Get(ctx, id)
		if err != nil || sess == nil {
			continue
		}
		if strings.TrimSpace(sess.WorktreeDir) != "" {
			continue
		}
		cwd := strings.TrimSpace(sess.CWD)
		if cwd == "" {
			active = append(active, id)
			continue
		}
		sessRoot, err := worktree.MainRepoRoot(cwd)
		if err != nil || sameServePath(sessRoot, root) {
			active = append(active, id)
		}
	}
	return active
}

func (s *serveServer) handleWorktreePromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	var req worktreePromoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Dir) == "" || strings.TrimSpace(req.Branch) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "dir and branch are required")
		return
	}
	wt, err := managedWorktreeForRoot(root, req.Dir)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if activeRootSessions := s.activeRootRunsForWorktreeMerge(r.Context(), root); len(activeRootSessions) > 0 {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", fmt.Sprintf("root checkout has active session run(s): %s", strings.Join(activeRootSessions, ", ")))
		return
	}
	res, err := worktree.PromoteToRoot(r.Context(), wt.Dir, req.Branch, worktree.PromoteOptions{})
	if errors.Is(err, worktree.ErrRootDirty) {
		writeJSON(w, http.StatusConflict, map[string]any{"result": res, "error": "root_dirty", "message": err.Error()})
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *serveServer) handleWorktreeDelete(w http.ResponseWriter, r *http.Request) {
	root, ok := s.currentGitRootOr409(w)
	if !ok {
		return
	}
	wt, err := managedWorktreeForRoot(root, r.URL.Query().Get("dir"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	force := r.URL.Query().Get("force") == "1" || strings.EqualFold(r.URL.Query().Get("force"), "true")
	inUse, err := worktree.InUse(r.Context(), s.store, wt.Dir)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if len(inUse) > 0 && !force {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "worktree in use", "in_use": inUse})
		return
	}
	if err := worktree.Remove(r.Context(), wt.Dir, worktree.RemoveOptions{Force: force}); err != nil {
		if errors.Is(err, worktree.ErrDirty) {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "worktree has uncommitted changes")
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
