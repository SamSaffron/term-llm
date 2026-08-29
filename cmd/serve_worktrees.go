package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
	worktreerecovery "github.com/samsaffron/term-llm/internal/worktree/recovery"
)

type worktreeCreateRequest struct {
	Name   string `json:"name"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
	Clean  bool   `json:"clean"`
}

type worktreeSwitchRequest struct {
	Dir string `json:"dir"`
}

type worktreeMergeRequest struct {
	Dir     string `json:"dir"`
	Commit  bool   `json:"commit"`
	Message string `json:"message"`
	Keep    bool   `json:"keep"`
	Force   bool   `json:"force"`
}

type worktreeAssistedMergeRequest struct {
	Dir string `json:"dir"`
}

type worktreePromoteRequest struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
}

type worktreeRow struct {
	Name              string                  `json:"name"`
	Dir               string                  `json:"dir"`
	RepoRoot          string                  `json:"repo_root,omitempty"`
	Branch            string                  `json:"branch,omitempty"`
	Detached          bool                    `json:"detached"`
	Base              string                  `json:"base,omitempty"`
	HeadSHA           string                  `json:"head_sha,omitempty"`
	Upstream          string                  `json:"upstream,omitempty"`
	UpstreamAvailable bool                    `json:"upstream_available"`
	Ahead             int                     `json:"ahead"`
	Behind            int                     `json:"behind"`
	Diverged          bool                    `json:"diverged"`
	MetadataError     string                  `json:"metadata_error,omitempty"`
	DirtyFiles        int                     `json:"dirty_files"`
	Root              bool                    `json:"root,omitempty"`
	InUse             []worktree.InUseSession `json:"in_use,omitempty"`
}

type serveWorktreeRootContextKey struct{}

// currentGitRoot resolves the serve process's captured startup repository once and shares
// it between the HTML capability bootstrap and every legacy worktree API handler.
func (s *serveServer) currentGitRoot() (string, error) {
	s.worktreeRootOnce.Do(func() {
		cwd := strings.TrimSpace(s.startupDir)
		var err error
		if cwd == "" {
			cwd, err = os.Getwd()
		}
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

func (s *serveServer) currentGitRootOr409(w http.ResponseWriter, r ...*http.Request) (string, bool) {
	if len(r) != 0 && r[0] != nil {
		if root, ok := r[0].Context().Value(serveWorktreeRootContextKey{}).(string); ok && root != "" {
			return root, true
		}
	}
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
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty worktree path")
	}
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
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return strings.TrimSpace(a) == "" && strings.TrimSpace(b) == ""
	}
	aa, errA := canonicalizeWorktreeBoundary(a)
	bb, errB := canonicalizeWorktreeBoundary(b)
	if errA != nil || errB != nil {
		cleanA, cleanB := filepath.Clean(a), filepath.Clean(b)
		if runtime.GOOS == "windows" {
			return strings.EqualFold(cleanA, cleanB)
		}
		return cleanA == cleanB
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func markLegacyWorktreeRoute(w http.ResponseWriter) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `</v1/projects/{project_id}/worktrees>; rel="successor-version"`)
}

func (s *serveServer) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(serveWorktreeRootContextKey{}).(string); !ok {
		markLegacyWorktreeRoute(w)
	}
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
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	rootCheckout, rootDetailErr := worktree.DescribeCheckout(root)
	rootRow := worktreeRow{Name: "root", Dir: root, RepoRoot: root, Root: true}
	if rootDetailErr != nil {
		rootRow.MetadataError = "HEAD metadata unavailable"
	} else {
		rootRow.Branch = rootCheckout.Branch
		rootRow.Detached = rootCheckout.Detached
		rootRow.HeadSHA = rootCheckout.HeadSHA
		rootRow.Upstream = rootCheckout.Upstream
		rootRow.UpstreamAvailable = rootCheckout.UpstreamAvailable
		rootRow.Ahead = rootCheckout.Ahead
		rootRow.Behind = rootCheckout.Behind
		rootRow.Diverged = rootCheckout.Diverged
		rootRow.MetadataError = rootCheckout.MetadataError
		rootRow.DirtyFiles = rootCheckout.DirtyFiles
	}
	rows := []worktreeRow{rootRow}
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
		rows = append(rows, worktreeRow{
			Name: wt.Name, Dir: wt.Dir, RepoRoot: wt.RepoRoot, Branch: wt.Branch, Detached: wt.Detached,
			Base: wt.Base, HeadSHA: wt.HeadSHA, Upstream: wt.Upstream, UpstreamAvailable: wt.UpstreamAvailable,
			Ahead: wt.Ahead, Behind: wt.Behind, Diverged: wt.Diverged, MetadataError: wt.MetadataError,
			DirtyFiles: wt.DirtyFiles, InUse: inUseByDir[wt.Dir],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": rows})
}

func (s *serveServer) handleWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	var req worktreeCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	opts := worktree.CreateOptions{Name: req.Name, Base: req.Base, Branch: req.Branch, SetupTimeout: 10 * time.Minute, MoveChanges: !req.Clean}
	if opts.Base == "" {
		opts.Base = "HEAD"
	}
	if script := strings.TrimSpace(os.Getenv("TERM_LLM_WORKTREE_SETUP")); script != "" {
		opts.SetupScript = script
	}
	releaseMutation := func() {}
	if !req.Clean {
		var admitted bool
		releaseMutation, admitted = s.acquireRootMutation(w, r.Context(), root, false)
		if !admitted {
			return
		}
	}
	defer releaseMutation()
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

func (s *serveServer) handleWorktreeSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	sessionID := resolveRequestSessionID(r)
	if sessionID == "" || s.store == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "session id is required")
		return
	}
	var req worktreeSwitchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	projects, ok := session.AsProjectReader(s.store)
	if !ok || strings.TrimSpace(sess.ProjectID) == "" {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "session is not bound to a project")
		return
	}
	project, err := projects.GetProject(r.Context(), sess.ProjectID)
	if err != nil || project == nil || !sameServePath(project.CanonicalDir, root) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "session belongs to a different project")
		return
	}
	targetDir, worktreeDir := root, ""
	if strings.TrimSpace(req.Dir) != "" {
		wt, err := managedWorktreeForRoot(root, req.Dir)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		targetDir, worktreeDir = wt.Dir, wt.Dir
	}
	if sameServePath(sess.CWD, targetDir) && sameServePath(sess.WorktreeDir, worktreeDir) {
		writeJSON(w, http.StatusOK, map[string]any{"session": sess, "worktree_dir": worktreeDir, "cwd": targetDir})
		return
	}
	if s.responseRuns != nil && s.responseRuns.activeRunID(sessionID) != "" {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot switch worktrees while a response is active")
		return
	}
	var rt *serveRuntime
	release := func() {}
	if s.sessionMgr != nil {
		rt, release, err = s.sessionMgr.lockIdleMetadataMutation(sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot switch worktrees while the conversation is active")
			return
		}
	}
	defer release()
	switcher, ok := session.AsSessionWorkspaceSwitcher(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session workspace switching is unavailable")
		return
	}
	if rt != nil {
		resolved := *sess
		if err := bindRuntimeWorkspace(r.Context(), rt, &resolved, targetDir, worktreeDir); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "the live runtime could not switch workspaces")
			return
		}
	}
	binding := session.SessionWorkspaceBinding{ProjectID: sess.ProjectID, CWD: targetDir, WorktreeDir: worktreeDir}
	persisted, err := switcher.SwitchSessionWorkspace(r.Context(), sessionID, binding)
	if err != nil {
		if rt != nil {
			rollback := *sess
			if rollbackErr := bindRuntimeWorkspace(r.Context(), rt, &rollback, sess.CWD, sess.WorktreeDir); rollbackErr == nil {
				s.configureRuntimeSkillsForDirLocked(rt, sess.CWD)
			}
		}
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "could not switch the conversation workspace")
		return
	}
	if rt != nil {
		resolved := *persisted
		rt.sessionMeta = &resolved
		s.configureRuntimeSkillsForDirLocked(rt, targetDir)
	}
	release()
	s.publishEvent(serveEventInput{Type: serveEventSessionMetadataChanged, SessionID: sessionID, ProjectID: sess.ProjectID, Reason: "worktree"})
	s.publishEvent(serveEventInput{Type: serveEventFilesChanged, SessionID: sessionID, ProjectID: sess.ProjectID, Reason: "worktree"})
	writeJSON(w, http.StatusOK, map[string]any{"session": persisted, "worktree_dir": worktreeDir, "cwd": targetDir})
}

func bindRuntimeWorkspace(ctx context.Context, rt *serveRuntime, sess *session.Session, cwd, worktreeDir string) error {
	if rt == nil {
		return nil
	}
	if strings.TrimSpace(worktreeDir) == "" {
		return BindRootSession(ctx, nil, sess, rt.toolMgr, cwd)
	}
	return BindWorktreeSession(ctx, nil, sess, rt.toolMgr, worktreeDir)
}

func (s *serveServer) handleWorktreeDiff(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(serveWorktreeRootContextKey{}).(string); !ok {
		markLegacyWorktreeRoute(w)
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	wt, err := managedWorktreeForRoot(root, r.URL.Query().Get("dir"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	result, err := worktree.DiffContext(r.Context(), wt.Dir)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *serveServer) cleanupCallerForWorktree(ctx context.Context, r *http.Request, dir string) string {
	sessionID := resolveRequestSessionID(r)
	if sessionID == "" || s.store == nil {
		return ""
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil || !sameServePath(sess.WorktreeDir, dir) {
		return ""
	}
	return sessionID
}

func (s *serveServer) assistedRecoveryCaller(ctx context.Context, r *http.Request, worktreeDir, root string) (string, *session.Session) {
	sessionID := resolveRequestSessionID(r)
	if sessionID == "" || s.store == nil {
		return "", nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return "", nil
	}
	if strings.TrimSpace(sess.WorktreeDir) != "" && sameServePath(sess.WorktreeDir, worktreeDir) {
		return sessionID, sess
	}
	if strings.TrimSpace(sess.WorktreeDir) == "" && sameServePath(sess.CWD, root) {
		return sessionID, sess
	}
	return "", nil
}

func (s *serveServer) moveCleanupCallerToRoot(ctx context.Context, sessionID, worktreeDir, root string) (*session.Session, error) {
	if sessionID == "" || s.store == nil {
		return nil, nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load calling session: %w", err)
	}
	if sess == nil || !sameServePath(sess.WorktreeDir, worktreeDir) {
		return nil, fmt.Errorf("calling session is no longer bound to the source worktree")
	}
	var rt *serveRuntime
	release := func() {}
	if s.sessionMgr != nil {
		rt, release, err = s.sessionMgr.lockIdleMetadataMutation(sessionID)
		if err != nil {
			return nil, fmt.Errorf("calling conversation is active: %w", err)
		}
	}
	defer release()
	resolved := *sess
	if rt != nil {
		if err := bindRuntimeWorkspace(ctx, rt, &resolved, root, ""); err != nil {
			return nil, fmt.Errorf("move live runtime to root: %w", err)
		}
	}
	var persisted *session.Session
	if strings.TrimSpace(sess.ProjectID) != "" {
		switcher, ok := session.AsSessionWorkspaceSwitcher(s.store)
		if !ok {
			if rt != nil {
				rollback := *sess
				_ = bindRuntimeWorkspace(ctx, rt, &rollback, sess.CWD, sess.WorktreeDir)
			}
			return nil, fmt.Errorf("session workspace switching is unavailable")
		}
		persisted, err = switcher.SwitchSessionWorkspace(ctx, sessionID, session.SessionWorkspaceBinding{ProjectID: sess.ProjectID, CWD: root})
	} else {
		persisted = &session.Session{}
		*persisted = *sess
		err = BindRootSession(ctx, s.store, persisted, nil, root)
	}
	if err != nil {
		if rt != nil {
			rollback := *sess
			if rollbackErr := bindRuntimeWorkspace(ctx, rt, &rollback, sess.CWD, sess.WorktreeDir); rollbackErr == nil {
				s.configureRuntimeSkillsForDirLocked(rt, sess.CWD)
			}
		}
		return nil, fmt.Errorf("persist root workspace: %w", err)
	}
	if rt != nil {
		resolved = *persisted
		rt.sessionMeta = &resolved
		s.configureRuntimeSkillsForDirLocked(rt, root)
	}
	release()
	return persisted, nil
}

func (s *serveServer) handleWorktreeMerge(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(serveWorktreeRootContextKey{}).(string); !ok {
		markLegacyWorktreeRoute(w)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w, r)
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
	releaseMutation, ok := s.acquireRootMutation(w, r.Context(), root, req.Force)
	if !ok {
		return
	}
	defer releaseMutation()
	opts := worktree.MergeOptions{Commit: req.Commit, Message: req.Message}
	var res worktree.MergeResult
	var cleanup worktree.CleanupResult
	callerSessionID := s.cleanupCallerForWorktree(r.Context(), r, wt.Dir)
	recoveryCallerID, _ := s.assistedRecoveryCaller(r.Context(), r, wt.Dir, root)
	res, err = worktree.MergeBack(r.Context(), wt.Dir, opts)
	if errors.Is(err, worktree.ErrConflict) {
		message := "root checkout was reset cleanly after conflicts"
		if !res.ConflictReset {
			message = "merge conflicts occurred and automatic cleanup did not fully complete; inspect the root checkout"
		}
		offer := worktreerecovery.OfferForMerge(worktreerecovery.KindConflict, res, 0)
		switch {
		case !res.ConflictReset:
			offer.Available = false
			offer.UnavailableReason = "Automatic conflict cleanup did not complete. Inspect and clean the root checkout before retrying."
		case recoveryCallerID == "":
			offer.Available = false
			offer.UnavailableReason = worktreerecovery.UnavailableCallerReason
		}
		writeJSON(w, http.StatusConflict, map[string]any{"result": res, "error": "conflicts", "message": message, "recovery": offer})
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
	var movedSession *session.Session
	if !req.Keep {
		if callerSessionID != "" {
			movedSession, err = s.moveCleanupCallerToRoot(r.Context(), callerSessionID, wt.Dir, res.RootDir)
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]any{
					"result":  res,
					"error":   "workspace_move_failed",
					"message": fmt.Sprintf("changes merged, but the conversation could not move to root; the source checkout was kept: %v", err),
				})
				return
			}
		}
		cleanup, err = worktree.CleanupAfterOperation(r.Context(), wt.Dir, s.store, "")
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"result":  res,
				"cleanup": cleanup,
				"session": movedSession,
				"warning": fmt.Sprintf("changes merged, but source checkout cleanup failed: %v", err),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "cleanup": cleanup, "session": movedSession})
}

func (s *serveServer) handleWorktreeAssistedMerge(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(serveWorktreeRootContextKey{}).(string); !ok {
		markLegacyWorktreeRoute(w)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	var req worktreeAssistedMergeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Dir) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "dir is required")
		return
	}
	wt, err := managedWorktreeForRoot(root, req.Dir)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	callerSessionID, callerSession := s.assistedRecoveryCaller(r.Context(), r, wt.Dir, root)
	if callerSessionID == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "assisted_recovery_unavailable",
			"message": worktreerecovery.UnavailableCallerReason,
		})
		return
	}
	releaseMutation, ok := s.acquireRootMutation(w, r.Context(), root, false)
	if !ok {
		return
	}
	defer releaseMutation()
	movedSession := callerSession
	if strings.TrimSpace(callerSession.WorktreeDir) != "" {
		movedSession, err = s.moveCleanupCallerToRoot(r.Context(), callerSessionID, wt.Dir, root)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "workspace_move_failed",
				"message": fmt.Sprintf("assisted recovery could not move the conversation to root: %v", err),
			})
			return
		}
	}
	releaseCaller := func() {}
	if s.sessionMgr != nil {
		_, releaseCaller, err = s.sessionMgr.lockIdleMetadataMutation(callerSessionID)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "workspace_move_failed",
				"message": fmt.Sprintf("assisted recovery could not lock the root conversation: %v", err),
				"session": movedSession,
			})
			return
		}
	}
	defer releaseCaller()
	if _, latest := s.assistedRecoveryCaller(r.Context(), r, wt.Dir, root); latest == nil || strings.TrimSpace(latest.WorktreeDir) != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "workspace_move_failed",
			"message": "the calling conversation is no longer bound to the root checkout",
		})
		return
	} else {
		movedSession = latest
	}
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	res, err := worktree.StartAssistedMerge(operationCtx, wt.Dir, worktree.AssistedMergeOptions{})
	if errors.Is(err, worktree.ErrRootDirty) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"result":  res,
			"session": movedSession,
			"error":   "root_dirty",
			"message": worktreerecovery.AssistedMergeRootDirtyMessage(res),
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "server_error",
			"message": err.Error(),
			"result":  res,
			"session": movedSession,
		})
		return
	}
	if len(res.ChangedFiles) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"result":  res,
			"session": movedSession,
			"message": worktreerecovery.AssistedMergeNothingToApplyMessage(res),
		})
		return
	}
	mergeRes := worktree.MergeResult{WorktreeName: res.WorktreeName, WorktreeDir: res.WorktreeDir, RootDir: res.RootDir}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":  res,
		"session": movedSession,
		"notice":  worktreerecovery.StartingMessage(mergeRes),
		"prompt":  worktreerecovery.AssistedMergePrompt(res),
	})
}

func writeActiveRootRunConflict(w http.ResponseWriter, active []string) {
	errorBody := map[string]any{
		"message": "The root checkout has an active run. Merging now may disrupt or overwrite that run's work.",
		"type":    "root_checkout_active_runs",
	}
	if len(active) > 0 {
		errorBody["active_runs"] = active
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": errorBody})
}

func (s *serveServer) acquireRootMutation(w http.ResponseWriter, ctx context.Context, root string, allowActiveRuns bool) (func(), bool) {
	release, blocked, err := processRootCheckoutLeases.tryAcquireMutation(root, allowActiveRuns)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", fmt.Sprintf("coordinate root checkout mutation: %v", err))
		return nil, false
	}
	if blocked == rootCheckoutMutationBlockedByMutation {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "root checkout has an active worktree mutation")
		return nil, false
	}
	if blocked == rootCheckoutMutationBlockedByRun {
		writeActiveRootRunConflict(w, s.activeRootRunsForWorktreeMerge(ctx, root))
		return nil, false
	}
	if !allowActiveRuns {
		if active := s.activeRootRunsForWorktreeMerge(ctx, root); len(active) > 0 {
			release()
			writeActiveRootRunConflict(w, active)
			return nil, false
		}
	}
	if s.rootMutationAdmitted != nil {
		s.rootMutationAdmitted()
	}
	return release, true
}

func directoryGitMetadataState(dir string) (hasGit, known bool) {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(dir))
	if err != nil {
		return false, false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return false, false
	}
	for current := filepath.Clean(resolved); ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return true, true
		} else if !os.IsNotExist(err) {
			return false, false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, true
		}
	}
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
	projects, _ := session.AsProjectReader(s.store)
	for _, id := range ids {
		sess, err := s.store.Get(ctx, id)
		if err != nil || sess == nil {
			continue
		}
		sessRoot := ""
		if projects != nil && strings.TrimSpace(sess.ProjectID) != "" {
			project, projectErr := projects.GetProject(ctx, sess.ProjectID)
			if projectErr == nil && project != nil {
				sessRoot = strings.TrimSpace(project.CanonicalDir)
			}
		}
		if sessRoot == "" {
			candidate := strings.TrimSpace(sess.WorktreeDir)
			if candidate == "" {
				candidate = strings.TrimSpace(sess.CWD)
			}
			if candidate == "" {
				// An unbound serve runtime executes relative to the server checkout.
				// Without persisted provenance, fail closed for repository mutation.
				active = append(active, id)
				continue
			}
			resolvedRoot, rootErr := worktree.MainRepoRootContext(ctx, candidate)
			if rootErr != nil {
				if hasGit, known := directoryGitMetadataState(candidate); known && !hasGit {
					continue
				}
				// Git inspection failures are not proof that an active runtime is
				// unrelated. Fail closed unless the directory is provably non-Git.
				active = append(active, id)
				continue
			}
			sessRoot = resolvedRoot
		}
		if sameServePath(sessRoot, root) {
			active = append(active, id)
		}
	}
	return active
}

func (s *serveServer) handleWorktreePromote(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(serveWorktreeRootContextKey{}).(string); !ok {
		markLegacyWorktreeRoute(w)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	root, ok := s.currentGitRootOr409(w, r)
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
	releaseMutation, ok := s.acquireRootMutation(w, r.Context(), root, false)
	if !ok {
		return
	}
	defer releaseMutation()
	callerSessionID := s.cleanupCallerForWorktree(r.Context(), r, wt.Dir)
	res, err := worktree.PromoteToRoot(r.Context(), wt.Dir, req.Branch, worktree.PromoteOptions{})
	if errors.Is(err, worktree.ErrRootDirty) {
		writeJSON(w, http.StatusConflict, map[string]any{"result": res, "error": "root_dirty", "message": err.Error()})
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	var movedSession *session.Session
	if callerSessionID != "" {
		movedSession, err = s.moveCleanupCallerToRoot(r.Context(), callerSessionID, wt.Dir, res.RootDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"result":  res,
				"error":   "workspace_move_failed",
				"message": fmt.Sprintf("changes promoted, but the conversation could not move to root; the source checkout was kept: %v", err),
			})
			return
		}
	}
	cleanup, err := worktree.CleanupAfterOperation(r.Context(), wt.Dir, s.store, "")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"result":  res,
			"cleanup": cleanup,
			"session": movedSession,
			"warning": fmt.Sprintf("changes promoted, but source checkout cleanup failed: %v", err),
		})
		return
	}
	if cleanup.Removed {
		res.OriginalWorktreeStillExists = false
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "cleanup": cleanup, "session": movedSession})
}

func (s *serveServer) handleWorktreeDelete(w http.ResponseWriter, r *http.Request) {
	root, ok := s.currentGitRootOr409(w, r)
	if !ok {
		return
	}
	wt, err := managedWorktreeForRoot(root, r.URL.Query().Get("dir"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	releaseMutation, ok := s.acquireRootMutation(w, r.Context(), root, false)
	if !ok {
		return
	}
	defer releaseMutation()
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
