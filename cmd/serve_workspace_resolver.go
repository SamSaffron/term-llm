package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

type serveWorkspaceRequest struct {
	SessionID         string
	ProjectID         string
	WorktreeDir       string
	FirstPartyUI      bool
	FreshConversation bool
	AllowNoProject    bool
}

type serveWorkspaceBinding struct {
	ProjectID   string
	RootDir     string
	RuntimeDir  string
	WorktreeDir string
	RepoRoot    string
}

type serveWorkspaceError struct {
	Code   string
	Status int
	Msg    string
}

func (e *serveWorkspaceError) Error() string { return e.Msg }

func workspaceError(status int, code, message string) error {
	return &serveWorkspaceError{Code: code, Status: status, Msg: message}
}

func writeWorkspaceError(w http.ResponseWriter, err error) {
	var typed *serveWorkspaceError
	if errors.As(err, &typed) {
		writeProjectError(w, typed.Status, typed.Code, typed.Msg)
		return
	}
	writeProjectError(w, http.StatusInternalServerError, "workspace_error", "could not bind the project workspace")
}

func (s *serveServer) resolveWorkspace(ctx context.Context, req serveWorkspaceRequest) (serveWorkspaceBinding, error) {
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.WorktreeDir = strings.TrimSpace(req.WorktreeDir)
	var persisted *session.Session
	if s.store != nil && strings.TrimSpace(req.SessionID) != "" {
		var err error
		persisted, err = s.store.Get(ctx, req.SessionID)
		if err != nil {
			return serveWorkspaceBinding{}, fmt.Errorf("load persisted workspace: %w", err)
		}
	}

	if req.AllowNoProject && req.ProjectID != "" {
		return serveWorkspaceBinding{}, workspaceError(http.StatusBadRequest, "invalid_project_selection", "no_project and project_id are mutually exclusive")
	}
	if !s.projectsEnabled {
		if req.ProjectID != "" {
			return serveWorkspaceBinding{}, workspaceError(http.StatusBadRequest, "projects_disabled", "project_id is not accepted while project mode is disabled")
		}
		return serveWorkspaceBinding{}, nil
	}
	if req.ProjectID == "" && persisted != nil {
		req.ProjectID = strings.TrimSpace(persisted.ProjectID)
	}
	if req.AllowNoProject && req.ProjectID != "" {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "this conversation is already bound to a project")
	}
	if req.FirstPartyUI && req.FreshConversation && req.ProjectID == "" && !req.AllowNoProject && (persisted == nil || strings.TrimSpace(persisted.ProjectID) == "") {
		return serveWorkspaceBinding{}, workspaceError(http.StatusBadRequest, "project_required", "choose a project before starting a conversation")
	}
	if req.ProjectID == "" {
		// An explicit No project selection uses the persisted immutable snapshot,
		// or the server startup directory for a new conversation, without adding
		// project provenance.
		if req.AllowNoProject && req.FirstPartyUI {
			if persisted != nil {
				persistedWorktree := strings.TrimSpace(persisted.WorktreeDir)
				if req.WorktreeDir != "" && !sameServePath(req.WorktreeDir, persistedWorktree) {
					return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "this conversation is already bound to another worktree")
				}
				root := persistedWorktree
				if root == "" {
					root = strings.TrimSpace(persisted.CWD)
				}
				if root == "" {
					root = strings.TrimSpace(s.startupDir)
				}
				if root == "" {
					return serveWorkspaceBinding{}, workspaceError(http.StatusServiceUnavailable, "workspace_unavailable", "the default workspace is unavailable")
				}
				return serveWorkspaceBinding{RootDir: root, RuntimeDir: root, WorktreeDir: strings.TrimSpace(persisted.WorktreeDir)}, nil
			}
			if req.FreshConversation {
				if req.WorktreeDir != "" {
					return serveWorkspaceBinding{}, workspaceError(http.StatusBadRequest, "project_required", "worktree_dir requires project_id")
				}
				root := strings.TrimSpace(s.startupDir)
				if root == "" {
					return serveWorkspaceBinding{}, workspaceError(http.StatusServiceUnavailable, "workspace_unavailable", "the default workspace is unavailable")
				}
				return serveWorkspaceBinding{RootDir: root, RuntimeDir: root}, nil
			}
		}
		// Third-party omission and legacy null-project resumes retain their existing
		// unbound/explicit behavior. A first-party UI may not use an arbitrary path
		// as a substitute for selecting a registry project.
		if req.WorktreeDir != "" && req.FirstPartyUI {
			return serveWorkspaceBinding{}, workspaceError(http.StatusBadRequest, "project_required", "worktree_dir requires project_id")
		}
		return serveWorkspaceBinding{}, nil
	}
	if persisted != nil && persisted.ProjectID != "" && persisted.ProjectID != req.ProjectID {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "this conversation is already bound to another project")
	}
	projects, ok := s.projectStore()
	if !ok {
		return serveWorkspaceBinding{}, workspaceError(http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
	}
	project, err := projects.GetProject(ctx, req.ProjectID)
	if err != nil {
		return serveWorkspaceBinding{}, fmt.Errorf("get project: %w", err)
	}
	if project == nil {
		return serveWorkspaceBinding{}, workspaceError(http.StatusNotFound, "project_not_found", "this project no longer exists")
	}
	status := projectStatus(*project)
	if !status.Available {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", status.UnavailableReason)
	}
	if project.Archived() && req.FreshConversation {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_archived", "archived projects cannot start new conversations")
	}
	binding := serveWorkspaceBinding{ProjectID: project.ID, RootDir: project.CanonicalDir, RuntimeDir: project.CanonicalDir}
	if status.Git {
		binding.RepoRoot = project.CanonicalDir
	}

	if persisted != nil && persisted.ProjectID == "" {
		// A claimed first-party fresh request creates an empty Web shell before the
		// conditional bind. If the bind itself fails transiently, a retry must be
		// able to finish that same first binding. Keep every populated or historical
		// null-project session behind the dedicated assignment endpoint.
		recoverableShell := req.FirstPartyUI && req.FreshConversation &&
			persisted.Origin == session.OriginWeb && persisted.Status == session.StatusActive &&
			strings.TrimSpace(persisted.CWD) == "" && strings.TrimSpace(persisted.WorktreeDir) == "" &&
			persisted.MessageCount == 0
		if !recoverableShell {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "assign historical conversations through the dedicated project assignment action before resuming them")
		}
	}

	// Existing snapshots are authoritative. Validate their relationship rather
	// than deriving new execution paths from current UI state.
	if persisted != nil && persisted.ProjectID != "" {
		return persistedProjectWorkspaceBinding(*project, status, *persisted, req.WorktreeDir)
	}

	if req.WorktreeDir != "" {
		if !status.Git {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "worktrees_unavailable", "worktrees unavailable — not a Git repository")
		}
		wt, err := managedWorktreeForRoot(project.CanonicalDir, req.WorktreeDir)
		if err != nil {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", err.Error())
		}
		mainRoot, err := worktree.MainRepoRoot(wt.Dir)
		if err != nil || !sameServePath(mainRoot, project.CanonicalDir) {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "worktree does not belong to the selected project")
		}
		binding.WorktreeDir = wt.Dir
		binding.RuntimeDir = wt.Dir
	}
	return binding, nil
}

func persistedProjectWorkspaceBinding(project session.Project, status projectAPI, persisted session.Session, requestedWorktree string) (serveWorkspaceBinding, error) {
	binding := serveWorkspaceBinding{
		ProjectID:  project.ID,
		RootDir:    project.CanonicalDir,
		RuntimeDir: project.CanonicalDir,
	}
	if status.Git {
		binding.RepoRoot = project.CanonicalDir
	}
	persistedCWD := strings.TrimSpace(persisted.CWD)
	persistedWorktree := strings.TrimSpace(persisted.WorktreeDir)
	if persistedWorktree != "" && (persistedCWD == "" || !sameServePath(persistedCWD, persistedWorktree)) {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", "the persisted worktree and execution directory no longer describe the same immutable workspace")
	}
	matches := sessionMatchesProjectBinding(persisted, resolvedProjectPath{CanonicalDir: project.CanonicalDir, Git: status.Git})
	if !matches && status.Git && persistedWorktree != "" {
		// A project-bound worktree was validated when its immutable snapshot was
		// first committed. If that checkout later disappears, only a path in this
		// project's managed bucket may use the documented root fallback.
		if _, statErr := os.Stat(persistedWorktree); os.IsNotExist(statErr) {
			managedRoot, rootErr := worktree.ManagedRoot(project.CanonicalDir)
			if rootErr == nil {
				candidate, candidateErr := canonicalizeWorktreeBoundary(persistedWorktree)
				managedRoot, rootErr = canonicalizeWorktreeBoundary(managedRoot)
				matches = rootErr == nil && candidateErr == nil && candidate != managedRoot && pathWithinDir(candidate, managedRoot)
			}
		}
	}
	if !matches {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", "the persisted conversation workspace no longer matches its project")
	}
	binding.WorktreeDir = persistedWorktree
	binding.RuntimeDir = persistedCWD
	if binding.RuntimeDir == "" {
		binding.RuntimeDir = project.CanonicalDir
	}
	if binding.WorktreeDir != "" {
		if _, statErr := os.Stat(binding.WorktreeDir); os.IsNotExist(statErr) {
			binding.RuntimeDir = project.CanonicalDir
		} else if statErr != nil {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", "the persisted worktree cannot be validated")
		} else if _, wtErr := managedWorktreeForRoot(project.CanonicalDir, binding.WorktreeDir); wtErr != nil {
			return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", "the persisted worktree is no longer a managed worktree of this project")
		}
	}
	if requestedWorktree != "" && !sameServePath(requestedWorktree, binding.WorktreeDir) {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "workspace_conflict", "this conversation is already bound to another worktree")
	}
	return binding, nil
}

func (s *serveServer) resolvePersistedProjectWorkspace(ctx context.Context, persisted session.Session) (serveWorkspaceBinding, error) {
	projects, ok := session.AsProjectReader(s.store)
	if !ok {
		return serveWorkspaceBinding{}, workspaceError(http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
	}
	project, err := projects.GetProject(ctx, persisted.ProjectID)
	if err != nil {
		return serveWorkspaceBinding{}, fmt.Errorf("get persisted project: %w", err)
	}
	if project == nil {
		return serveWorkspaceBinding{}, workspaceError(http.StatusNotFound, "project_not_found", "this project no longer exists")
	}
	status := projectStatus(*project)
	if !status.Available {
		return serveWorkspaceBinding{}, workspaceError(http.StatusConflict, "project_unavailable", status.UnavailableReason)
	}
	return persistedProjectWorkspaceBinding(*project, status, persisted, "")
}

func (s *serveServer) bindResolvedWorkspace(ctx context.Context, sessionID string, rt *serveRuntime, binding serveWorkspaceBinding) error {
	var persisted *session.Session
	if binding.ProjectID == "" {
		if strings.TrimSpace(binding.RuntimeDir) == "" {
			return nil
		}
		var err error
		persisted, err = s.store.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("load no-project session shell: %w", err)
		}
		if persisted == nil {
			return errors.New("load no-project session shell: session not found")
		}
		if strings.TrimSpace(persisted.ProjectID) != "" ||
			(strings.TrimSpace(persisted.CWD) != "" && !sameServePath(persisted.CWD, binding.RuntimeDir)) ||
			(strings.TrimSpace(persisted.WorktreeDir) != "" && !sameServePath(persisted.WorktreeDir, binding.WorktreeDir)) {
			return workspaceError(http.StatusConflict, "workspace_conflict", "another request already bound this conversation to a different workspace")
		}
		persisted.CWD = binding.RuntimeDir
		persisted.WorktreeDir = binding.WorktreeDir
		if err := s.store.Update(ctx, persisted); err != nil {
			return fmt.Errorf("persist no-project workspace: %w", err)
		}
	} else {
		current, err := s.store.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("load project session shell: %w", err)
		}
		if current != nil && strings.TrimSpace(current.ProjectID) == binding.ProjectID {
			// Existing immutable project snapshots are already persisted. RuntimeDir
			// may intentionally be a root fallback for a vanished worktree and must
			// not be written back over that snapshot.
			persisted = current
		} else {
			binder, ok := session.AsSessionWorkspaceBinder(s.store)
			if !ok {
				return workspaceError(http.StatusServiceUnavailable, "projects_unavailable", "session workspace binding is unavailable")
			}
			persisted, err = binder.BindSessionWorkspace(ctx, sessionID, session.SessionWorkspaceBinding{
				ProjectID:   binding.ProjectID,
				CWD:         binding.RuntimeDir,
				WorktreeDir: binding.WorktreeDir,
			})
			if errors.Is(err, session.ErrWorkspaceConflict) {
				return workspaceError(http.StatusConflict, "workspace_conflict", "another request already bound this conversation to a different workspace")
			}
			if err != nil {
				return err
			}
		}
	}
	if rt != nil && rt.toolMgr != nil {
		if err := rt.toolMgr.ConfigureWorkspacePersistence(ctx, s.store, sessionID); err != nil {
			return fmt.Errorf("configure workspace persistence: %w", err)
		}
		if err := rt.toolMgr.SetBaseDirWithContext(ctx, binding.RuntimeDir); err != nil {
			return fmt.Errorf("apply project workspace: %w", err)
		}
		s.configureRuntimeSkillsForDir(rt, binding.RuntimeDir)
	}
	if binding.WorktreeDir != "" {
		_ = worktree.TouchLastBound(binding.WorktreeDir)
	}
	if rt != nil {
		rt.mu.Lock()
		rt.sessionMeta = persisted
		rt.mu.Unlock()
	}
	return nil
}
