package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

const (
	maxProjectRequestBytes      = 64 << 10
	maxEagerProjectStatusChecks = 64
	projectStatusCacheTTL       = 15 * time.Second
)

type projectStatusCacheEntry struct {
	status    projectAPI
	updatedAt time.Time
	checkedAt time.Time
	detailed  bool
}

type resolvedProjectPath struct {
	CanonicalDir string `json:"canonical_dir"`
	DefaultName  string `json:"default_name"`
	Git          bool   `json:"git"`
}

type projectAPI struct {
	session.Project
}

type projectCreateRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type projectCreateResponse struct {
	Project           *projectAPI `json:"project,omitempty"`
	CanonicalDir      string      `json:"canonical_dir"`
	DefaultName       string      `json:"default_name"`
	Git               bool        `json:"git"`
	Duplicate         bool        `json:"duplicate,omitempty"`
	ExistingProjectID string      `json:"existing_project_id,omitempty"`
	Restored          bool        `json:"restored,omitempty"`
}

type projectPatchRequest struct {
	Name      *string `json:"name,omitempty"`
	Archived  *bool   `json:"archived,omitempty"`
	Path      *string `json:"path,omitempty"`
	ProjectID *string `json:"project_id,omitempty"`
}

type projectError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ExistingProjectID string `json:"existing_project_id,omitempty"`
}

func writeProjectError(w http.ResponseWriter, status int, code, message string) {
	var payload projectError
	payload.Error.Code = code
	payload.Error.Message = message
	writeJSON(w, status, payload)
}

func decodeSmallJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectRequestBytes)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func containsProjectControl(path string) bool {
	for _, r := range path {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func canonicalProjectStoragePath(path, goos string) string {
	path = filepath.Clean(path)
	if goos == "windows" {
		// Windows path identity is case-insensitive. Persist one normalized form so
		// both application lookup and SQLite's exact UNIQUE constraint agree.
		return strings.ToLower(path)
	}
	return path
}

// resolveProjectPath is the single validator used by dry-run, create,
// bootstrap, and availability checks.
func resolveProjectPath(path string) (resolvedProjectPath, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return resolvedProjectPath{}, fmt.Errorf("path is required")
	}
	if containsProjectControl(path) {
		return resolvedProjectPath{}, fmt.Errorf("path contains control characters")
	}
	if strings.HasPrefix(path, "~") || strings.Contains(path, "$") {
		return resolvedProjectPath{}, fmt.Errorf("path must not use shell expansion")
	}
	if !filepath.IsAbs(path) {
		return resolvedProjectPath{}, fmt.Errorf("path must be absolute")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return resolvedProjectPath{}, fmt.Errorf("resolve absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return resolvedProjectPath{}, fmt.Errorf("resolve path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if filepath.Dir(resolved) == resolved {
		return resolvedProjectPath{}, fmt.Errorf("filesystem root cannot be registered; use --no-projects for container-wide mode")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return resolvedProjectPath{}, fmt.Errorf("inspect path: %w", err)
	}
	if !info.IsDir() {
		return resolvedProjectPath{}, fmt.Errorf("path is not a directory")
	}
	// Opening the directory catches common unreadable/unsearchable cases without
	// recursively scanning operator data.
	dir, err := os.Open(resolved)
	if err != nil {
		return resolvedProjectPath{}, fmt.Errorf("open directory: %w", err)
	}
	_ = dir.Close()

	isGit := worktree.IsGitRepo(resolved)
	if isGit {
		resolved, err = worktree.MainRepoRoot(resolved)
		if err != nil {
			return resolvedProjectPath{}, fmt.Errorf("resolve main repository: %w", err)
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return resolvedProjectPath{}, fmt.Errorf("canonicalize main repository: %w", err)
		}
		resolved = filepath.Clean(resolved)
		if filepath.Dir(resolved) == resolved {
			return resolvedProjectPath{}, fmt.Errorf("filesystem root cannot be registered; use --no-projects for container-wide mode")
		}
	}
	defaultName := filepath.Base(resolved)
	resolved = canonicalProjectStoragePath(resolved, runtime.GOOS)
	return resolvedProjectPath{CanonicalDir: resolved, DefaultName: defaultName, Git: isGit}, nil
}

func sameCanonicalProjectIdentity(actual, stored string) bool {
	actual = filepath.Clean(actual)
	stored = filepath.Clean(stored)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(actual, stored)
	}
	return actual == stored
}

func projectStatus(p session.Project) projectAPI {
	api := projectAPI{Project: p}
	resolved, err := resolveProjectPath(p.CanonicalDir)
	if err != nil || !sameCanonicalProjectIdentity(resolved.CanonicalDir, p.CanonicalDir) {
		api.Project.Available = false
		api.Project.UnavailableReason = "Project directory is missing or its canonical identity changed"
		return api
	}
	api.Project.Available = true
	api.Project.Git = resolved.Git
	return api
}

func cheapProjectStatus(p session.Project) projectAPI {
	api := projectAPI{Project: p}
	resolved, err := filepath.EvalSymlinks(p.CanonicalDir)
	if err != nil || !sameCanonicalProjectIdentity(resolved, p.CanonicalDir) {
		api.Project.Available = false
		api.Project.UnavailableReason = "Project directory is missing or its canonical identity changed"
		return api
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		api.Project.Available = false
		api.Project.UnavailableReason = "Project directory is missing or its canonical identity changed"
		return api
	}
	api.Project.Available = true
	if gitInfo, statErr := os.Stat(filepath.Join(resolved, ".git")); statErr == nil {
		api.Project.Git = gitInfo.IsDir() || gitInfo.Mode().IsRegular()
	}
	return api
}

func (s *serveServer) clearProjectStatusCache() {
	s.projectStatusMu.Lock()
	s.projectStatuses = nil
	s.projectStatusMu.Unlock()
}

func projectWithDerivedStatus(p session.Project, cached projectAPI) projectAPI {
	api := projectAPI{Project: p}
	api.Project.Available = cached.Project.Available
	api.Project.Git = cached.Project.Git
	api.Project.UnavailableReason = cached.Project.UnavailableReason
	return api
}

func (s *serveServer) cachedProjectStatus(p session.Project, detailed bool) projectAPI {
	now := time.Now()
	s.projectStatusMu.Lock()
	if entry, ok := s.projectStatuses[p.ID]; ok && entry.updatedAt.Equal(p.UpdatedAt) && now.Sub(entry.checkedAt) < projectStatusCacheTTL && (entry.detailed || !detailed) {
		s.projectStatusMu.Unlock()
		return projectWithDerivedStatus(p, entry.status)
	}
	s.projectStatusMu.Unlock()

	status := cheapProjectStatus(p)
	if detailed && status.Available {
		status = projectStatus(p)
	}
	s.projectStatusMu.Lock()
	if s.projectStatuses == nil {
		s.projectStatuses = make(map[string]projectStatusCacheEntry)
	}
	s.projectStatuses[p.ID] = projectStatusCacheEntry{status: status, updatedAt: p.UpdatedAt, checkedAt: now, detailed: detailed}
	s.projectStatusMu.Unlock()
	return status
}

func resolveServeProjectsRequested(cmdProjects, cmdNoProjects, configEnabled, hasWeb bool) (enabled, strict bool) {
	if !hasWeb || cmdNoProjects {
		return false, false
	}
	if cmdProjects {
		return true, true
	}
	return configEnabled, false
}

func initializeServeProjects(ctx context.Context, store session.Store, startupDir string, requested, strict bool, warningWriter io.Writer) (bool, string, error) {
	if !requested {
		return false, "", nil
	}
	projects, ok := session.AsProjectStore(store)
	if !ok {
		if strict {
			return false, "", fmt.Errorf("--projects requires a writable SQLite session store with project support")
		}
		fmt.Fprintln(warningWriter, "warning: projects auto-disabled: writable project session storage is unavailable; use --projects to require it or --no-projects to silence this warning")
		return false, "", nil
	}
	existing, err := projects.ListProjects(ctx, session.ProjectListOptions{IncludeArchived: true})
	if err != nil {
		if strict {
			return false, "", fmt.Errorf("initialize projects: %w", err)
		}
		fmt.Fprintf(warningWriter, "warning: projects auto-disabled: %v\n", err)
		return false, "", nil
	}
	for _, p := range existing {
		if p.IsBootstrap {
			return true, p.ID, nil
		}
	}
	if len(existing) != 0 {
		return true, "", nil
	}
	resolved, err := resolveProjectPath(startupDir)
	if err != nil {
		if strict {
			return false, "", fmt.Errorf("initialize bootstrap project: %w", err)
		}
		fmt.Fprintf(warningWriter, "warning: projects auto-disabled: startup directory cannot be registered (%v); use --projects to require project mode or --no-projects for legacy container mode\n", err)
		return false, "", nil
	}
	bootstrap := &session.Project{Name: resolved.DefaultName, CanonicalDir: resolved.CanonicalDir, IsBootstrap: true}
	matchingIDs, err := bootstrapMatchingSessions(ctx, store, resolved)
	if err != nil {
		return false, "", fmt.Errorf("inspect historical sessions for project bootstrap: %w", err)
	}
	if err := projects.BootstrapProject(ctx, bootstrap, matchingIDs); err != nil {
		return false, "", fmt.Errorf("bootstrap project: %w", err)
	}
	return true, bootstrap.ID, nil
}

func bootstrapMatchingSessions(ctx context.Context, store session.Store, root resolvedProjectPath) ([]string, error) {
	var ids []string
	before := int64(0)
	for {
		summaries, err := store.List(ctx, session.ListOptions{Archived: true, Limit: 200, BeforeNumber: before, SortByNumberDesc: true})
		if err != nil {
			return nil, err
		}
		if len(summaries) == 0 {
			break
		}
		for _, summary := range summaries {
			persisted, err := store.Get(ctx, summary.ID)
			if err != nil {
				return nil, err
			}
			if persisted == nil || persisted.ProjectID != "" {
				continue
			}
			if sessionMatchesProjectBinding(*persisted, root) {
				ids = append(ids, persisted.ID)
			}
		}
		before = summaries[len(summaries)-1].Number
		if len(summaries) < 200 || before <= 0 {
			break
		}
	}
	return ids, nil
}

func sessionMatchesProjectBinding(sess session.Session, project resolvedProjectPath) bool {
	if project.Git {
		cwd := strings.TrimSpace(sess.CWD)
		worktreeDir := strings.TrimSpace(sess.WorktreeDir)
		if worktreeDir != "" {
			// A historical worktree snapshot is assignable only when both exact
			// execution fields describe the same managed checkout. Merely having one
			// field resolve to the repository would create an unusable project session.
			if cwd == "" || !sameServePath(cwd, worktreeDir) {
				return false
			}
			wt, err := managedWorktreeForRoot(project.CanonicalDir, worktreeDir)
			if err != nil {
				return false
			}
			root, err := worktree.MainRepoRoot(wt.Dir)
			return err == nil && sameServePath(root, project.CanonicalDir)
		}
		if cwd == "" || !worktree.IsGitRepo(cwd) {
			return false
		}
		root, err := worktree.MainRepoRoot(cwd)
		return err == nil && sameServePath(root, project.CanonicalDir)
	}
	// Non-Git projects never have a legal worktree snapshot.
	if strings.TrimSpace(sess.WorktreeDir) != "" {
		return false
	}
	candidate := strings.TrimSpace(sess.CWD)
	if candidate == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	return err == nil && sameServePath(resolved, project.CanonicalDir)
}

func (s *serveServer) projectStore() (session.ProjectStore, bool) {
	if s == nil || !s.projectsEnabled {
		return nil, false
	}
	return session.AsProjectStore(s.store)
}

func (s *serveServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	worktreesEnabled := false
	if s.projectsEnabled {
		if projects, ok := s.projectStore(); ok {
			if list, err := projects.ListProjects(r.Context(), session.ProjectListOptions{}); err == nil {
				for i, p := range list {
					if status := s.cachedProjectStatus(p, i < maxEagerProjectStatusChecks); status.Available && status.Git {
						worktreesEnabled = true
						break
					}
				}
			}
		}
	} else if _, err := s.currentGitRoot(); err == nil {
		worktreesEnabled = true
	}
	w.Header().Set("ETag", fmt.Sprintf(`W/"projects-%t-worktrees-%t"`, s.projectsEnabled, worktreesEnabled))
	writeJSON(w, http.StatusOK, map[string]any{
		"projects":  map[string]bool{"enabled": s.projectsEnabled},
		"worktrees": map[string]bool{"enabled": worktreesEnabled},
	})
}

func (s *serveServer) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		includeArchived := r.URL.Query().Get("include_archived") == "1"
		list, err := projects.ListProjects(r.Context(), session.ProjectListOptions{IncludeArchived: includeArchived})
		if err != nil {
			writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not load projects")
			return
		}
		result := make([]projectAPI, 0, len(list))
		for i, p := range list {
			result = append(result, s.cachedProjectStatus(p, i < maxEagerProjectStatusChecks))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": result})
	case http.MethodPost:
		var req projectCreateRequest
		if err := decodeSmallJSON(w, r, &req); err != nil {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", "invalid project request: "+err.Error())
			return
		}
		resolved, err := resolveProjectPath(req.Path)
		if err != nil {
			log.Printf("[serve] rejected project path canonicalization: %v", err)
			writeProjectError(w, http.StatusBadRequest, "invalid_project_path", err.Error())
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = resolved.DefaultName
		}
		if name == "" || len([]rune(name)) > 120 {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", "project name must contain 1 to 120 characters")
			return
		}
		existing, err := projects.GetProjectByCanonicalDir(r.Context(), resolved.CanonicalDir)
		if err != nil {
			writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not check project")
			return
		}
		response := projectCreateResponse{CanonicalDir: resolved.CanonicalDir, DefaultName: resolved.DefaultName, Git: resolved.Git}
		if existing != nil {
			response.Duplicate = true
			response.ExistingProjectID = existing.ID
		}
		if r.URL.Query().Get("dry_run") == "1" {
			if existing != nil {
				api := projectStatus(*existing)
				response.Project = &api
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
		p := &session.Project{Name: name, CanonicalDir: resolved.CanonicalDir}
		wasArchived := existing != nil && existing.Archived()
		err = projects.CreateProject(r.Context(), p)
		if errors.Is(err, session.ErrProjectDuplicate) {
			var payload projectError
			payload.Error.Code = "project_duplicate"
			payload.Error.Message = "this directory is already registered"
			payload.ExistingProjectID = p.ID
			writeJSON(w, http.StatusConflict, payload)
			return
		}
		if err != nil {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", err.Error())
			return
		}
		api := projectStatus(*p)
		response.Project = &api
		response.Restored = wasArchived
		log.Printf("[serve] project %s: id=%s path=%s", map[bool]string{true: "restored", false: "created"}[wasArchived], p.ID, p.CanonicalDir)
		writeJSON(w, http.StatusCreated, response)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func projectIDFromPath(path string) (string, string) {
	path = strings.TrimPrefix(path, "/v1/projects/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (s *serveServer) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	id, suffix := projectIDFromPath(r.URL.Path)
	if id == "" {
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project not found")
		return
	}
	if strings.HasPrefix(suffix, "worktrees") {
		s.handleProjectWorktrees(w, r, id, strings.TrimPrefix(suffix, "worktrees"))
		return
	}
	if suffix != "" {
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project route not found")
		return
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	p, err := projects.GetProject(r.Context(), id)
	if err != nil || p == nil {
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, projectStatus(*p))
	case http.MethodPatch:
		var req projectPatchRequest
		if err := decodeSmallJSON(w, r, &req); err != nil {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", err.Error())
			return
		}
		if req.Path != nil || req.ProjectID != nil || (req.Name == nil && req.Archived == nil) {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", "only name and archived may be changed")
			return
		}
		updated, err := projects.UpdateProject(r.Context(), id, session.ProjectUpdate{Name: req.Name, Archived: req.Archived})
		if err != nil {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", err.Error())
			return
		}
		action := "renamed"
		if req.Archived != nil {
			if *req.Archived {
				action = "archived"
			} else {
				action = "restored"
			}
		}
		log.Printf("[serve] project %s: id=%s path=%s", action, updated.ID, updated.CanonicalDir)
		writeJSON(w, http.StatusOK, projectStatus(*updated))
	default:
		w.Header().Set("Allow", "GET, PATCH")
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *serveServer) handleSidebar(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	perProject, _ := strconv.Atoi(r.URL.Query().Get("per_project"))
	if r.URL.Query().Get("refresh_status") == "1" {
		s.clearProjectStatusCache()
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	groups, err := projects.Sidebar(r.Context(), session.SidebarOptions{
		PerProject:              perProject,
		IncludeArchivedProjects: r.URL.Query().Get("include_archived_projects") == "1",
		IncludeArchivedSessions: r.URL.Query().Get("include_archived_sessions") == "1",
	})
	if err != nil {
		writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not load projects and conversations")
		return
	}
	// Availability is intentionally derived rather than persisted.
	for i := range groups {
		if groups[i].Project != nil {
			api := s.cachedProjectStatus(*groups[i].Project, i < maxEagerProjectStatusChecks)
			groups[i].Project = &api.Project
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *serveServer) handleSessionProjectAssignment(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeSmallJSON(w, r, &req); err != nil || strings.TrimSpace(req.ProjectID) == "" {
		writeProjectError(w, http.StatusBadRequest, "invalid_project", "project_id is required")
		return
	}
	var releaseAssignmentGuard func()
	var guardedRuntime *serveRuntime
	if s.sessionMgr != nil {
		// Hold the manager map lock so a response cannot create/acquire this
		// session's runtime between the active-run check and the conditional DB
		// assignment. An existing runtime's operation lock closes the same race for
		// requests that already acquired it.
		s.sessionMgr.mu.Lock()
		if rt := s.sessionMgr.sessions[sessionID]; rt != nil {
			if rt.hasActiveRun() || !rt.mu.TryLock() {
				s.sessionMgr.mu.Unlock()
				writeProjectError(w, http.StatusConflict, "workspace_conflict", "an active response cannot be reassigned")
				return
			}
			if rt.hasActiveRun() {
				rt.mu.Unlock()
				s.sessionMgr.mu.Unlock()
				writeProjectError(w, http.StatusConflict, "workspace_conflict", "an active response cannot be reassigned")
				return
			}
			guardedRuntime = rt
			releaseAssignmentGuard = func() {
				rt.mu.Unlock()
				s.sessionMgr.mu.Unlock()
			}
		} else {
			releaseAssignmentGuard = s.sessionMgr.mu.Unlock
		}
		defer releaseAssignmentGuard()
	}
	persisted, err := s.store.Get(r.Context(), sessionID)
	if err != nil || persisted == nil {
		writeProjectError(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	if strings.TrimSpace(persisted.ProjectID) != "" {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "this conversation already has a project")
		return
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	project, err := projects.GetProject(r.Context(), strings.TrimSpace(req.ProjectID))
	if err != nil || project == nil {
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project not found")
		return
	}
	status := projectStatus(*project)
	if !status.Available {
		writeProjectError(w, http.StatusConflict, "project_unavailable", status.UnavailableReason)
		return
	}
	if project.Archived() {
		writeProjectError(w, http.StatusConflict, "project_archived", "restore the project before assigning conversations")
		return
	}
	if !sessionMatchesProjectBinding(*persisted, resolvedProjectPath{CanonicalDir: project.CanonicalDir, Git: status.Git}) {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "the persisted conversation workspace does not match this project")
		return
	}
	if err := projects.AssignSessionProject(r.Context(), sessionID, project.ID, persisted.CWD, persisted.WorktreeDir); errors.Is(err, session.ErrWorkspaceConflict) {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "this conversation was assigned by another request")
		return
	} else if err != nil {
		writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not assign the project")
		return
	}
	persisted.ProjectID = project.ID
	persisted.ProjectName = project.Name
	if guardedRuntime != nil {
		guardedRuntime.sessionMeta = persisted
	}
	writeJSON(w, http.StatusOK, persisted)
}

func (s *serveServer) handleProjectWorktrees(w http.ResponseWriter, r *http.Request, projectID, suffix string) {
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	project, err := projects.GetProject(r.Context(), projectID)
	if err != nil || project == nil {
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project not found")
		return
	}
	status := projectStatus(*project)
	if !status.Available {
		writeProjectError(w, http.StatusConflict, "project_unavailable", status.UnavailableReason)
		return
	}
	if !status.Git {
		writeProjectError(w, http.StatusConflict, "worktrees_unavailable", "worktrees unavailable — not a Git repository")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), serveWorktreeRootContextKey{}, project.CanonicalDir))
	switch suffix {
	case "", "/":
		s.handleWorktrees(w, r)
	case "/diff":
		s.handleWorktreeDiff(w, r)
	case "/merge":
		s.handleWorktreeMerge(w, r)
	case "/promote":
		s.handleWorktreePromote(w, r)
	default:
		writeProjectError(w, http.StatusNotFound, "project_not_found", "project worktree route not found")
	}
}
