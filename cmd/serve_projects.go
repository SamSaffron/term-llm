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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	projectpkg "github.com/samsaffron/term-llm/internal/project"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	maxProjectRequestBytes         = 64 << 10
	maxEagerProjectStatusChecks    = 64
	maxProjectDirectoryEntries     = 500
	maxProjectDirectoryScanEntries = 5000
	projectStatusCacheTTL          = 15 * time.Second
)

type projectStatusCacheEntry struct {
	status    projectAPI
	updatedAt time.Time
	checkedAt time.Time
	detailed  bool
}

type resolvedProjectPath = projectpkg.Resolved

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

type projectDirectoryEntry struct {
	Name              string `json:"name"`
	Path              string `json:"path"`
	Git               bool   `json:"git,omitempty"`
	ExistingProjectID string `json:"existing_project_id,omitempty"`
}

type projectDirectoryBreadcrumb struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type projectDirectoryResponse struct {
	Path        string                       `json:"path"`
	Parent      string                       `json:"parent,omitempty"`
	Home        string                       `json:"home,omitempty"`
	Breadcrumbs []projectDirectoryBreadcrumb `json:"breadcrumbs"`
	Entries     []projectDirectoryEntry      `json:"entries"`
	Truncated   bool                         `json:"truncated,omitempty"`
}

type sessionProjectCandidate struct {
	CanonicalDir      string `json:"canonical_dir"`
	DefaultName       string `json:"default_name"`
	Git               bool   `json:"git"`
	ExistingProjectID string `json:"existing_project_id,omitempty"`
	ExistingName      string `json:"existing_name,omitempty"`
	ExistingArchived  bool   `json:"existing_archived,omitempty"`
}

type sessionProjectAssignmentInfo struct {
	CWD         string                   `json:"cwd,omitempty"`
	WorktreeDir string                   `json:"worktree_dir,omitempty"`
	ProjectID   string                   `json:"project_id,omitempty"`
	Candidate   *sessionProjectCandidate `json:"candidate,omitempty"`
}

type sessionProjectAssignmentRequest struct {
	ProjectID           string `json:"project_id"`
	CreateFromWorkspace bool   `json:"create_from_workspace,omitempty"`
	Name                string `json:"name,omitempty"`
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
	return projectpkg.CanonicalStoragePathForOS(path, goos)
}

// resolveProjectPath is the single validator used by dry-run, create,
// bootstrap, and availability checks.
func resolveProjectPath(path string) (resolvedProjectPath, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return resolveProjectPathContext(ctx, path)
}

func resolveProjectPathContext(ctx context.Context, path string) (resolvedProjectPath, error) {
	return projectpkg.Resolve(ctx, path)
}

func sameCanonicalProjectIdentity(actual, stored string) bool {
	return projectpkg.SameIdentity(actual, stored)
}

func projectStatus(p session.Project) projectAPI {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return projectStatusContext(ctx, p)
}

func projectStatusContext(ctx context.Context, p session.Project) projectAPI {
	api := projectAPI{Project: p}
	resolved, err := resolveProjectPathContext(ctx, p.CanonicalDir)
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

func keepCachedProjectStatus(entry projectStatusCacheEntry, p session.Project, startedAt, now time.Time, detailed bool) bool {
	if !entry.updatedAt.Equal(p.UpdatedAt) {
		return false
	}
	if entry.detailed != detailed {
		return entry.detailed && now.Sub(entry.checkedAt) < projectStatusCacheTTL
	}
	return entry.checkedAt.After(startedAt)
}

func (s *serveServer) cachedProjectStatus(ctx context.Context, p session.Project, detailed bool) projectAPI {
	now := time.Now()
	s.projectStatusMu.Lock()
	if entry, ok := s.projectStatuses[p.ID]; ok && entry.updatedAt.Equal(p.UpdatedAt) && now.Sub(entry.checkedAt) < projectStatusCacheTTL && (entry.detailed || !detailed) {
		s.projectStatusMu.Unlock()
		return projectWithDerivedStatus(p, entry.status)
	}
	s.projectStatusMu.Unlock()

	status := cheapProjectStatus(p)
	if detailed && status.Available {
		status = projectStatusContext(ctx, p)
		if ctx.Err() != nil {
			return status
		}
	}
	s.projectStatusMu.Lock()
	if entry, ok := s.projectStatuses[p.ID]; ok && keepCachedProjectStatus(entry, p, now, time.Now(), detailed) {
		s.projectStatusMu.Unlock()
		return projectWithDerivedStatus(p, entry.status)
	}
	if s.projectStatuses == nil {
		s.projectStatuses = make(map[string]projectStatusCacheEntry)
	}
	s.projectStatuses[p.ID] = projectStatusCacheEntry{status: status, updatedAt: p.UpdatedAt, checkedAt: now, detailed: detailed}
	s.projectStatusMu.Unlock()
	return status
}

func resolveServeProjectsRequested(projectsSet, projectsValue, cmdNoProjects, configEnabled, hasWeb bool) (enabled, strict bool) {
	if !hasWeb || cmdNoProjects {
		return false, false
	}
	if projectsSet {
		return projectsValue, projectsValue
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
	resolved, err := resolveProjectPathContext(ctx, startupDir)
	if err != nil {
		if strict {
			return false, "", fmt.Errorf("initialize bootstrap project: %w", err)
		}
		fmt.Fprintf(warningWriter, "warning: projects auto-disabled: startup directory cannot be registered (%v); use --projects to require project mode or --no-projects for single-workspace mode\n", err)
		return false, "", nil
	}
	bootstrap := &session.Project{Name: resolved.DefaultName, CanonicalDir: resolved.CanonicalDir, IsBootstrap: true}
	matchingSessions, err := bootstrapMatchingSessions(ctx, store, resolved)
	if err != nil {
		if strict {
			return false, "", fmt.Errorf("inspect historical sessions for project bootstrap: %w", err)
		}
		fmt.Fprintf(warningWriter, "warning: projects auto-disabled: could not inspect historical sessions (%v)\n", err)
		return false, "", nil
	}
	if err := projects.BootstrapProject(ctx, bootstrap, matchingSessions); err != nil {
		if strict {
			return false, "", fmt.Errorf("bootstrap project: %w", err)
		}
		fmt.Fprintf(warningWriter, "warning: projects auto-disabled: could not create bootstrap project (%v)\n", err)
		return false, "", nil
	}
	return true, bootstrap.ID, nil
}

func bootstrapMatchingSessions(ctx context.Context, store session.Store, root resolvedProjectPath) ([]session.ProjectSessionMatch, error) {
	return projectpkg.MatchingSessionsForResolved(ctx, store, root)
}

func sessionMatchesProjectBinding(sess session.Session, project resolvedProjectPath) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sessionMatchesProjectBindingContext(ctx, sess, project)
}

func sessionMatchesProjectBindingContext(ctx context.Context, sess session.Session, root resolvedProjectPath) bool {
	return projectpkg.MatchesWorkspace(ctx, sess.CWD, sess.WorktreeDir, root)
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
				statusCtx, cancelStatus := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancelStatus()
				for i, p := range list {
					if status := s.cachedProjectStatus(statusCtx, p, i < maxEagerProjectStatusChecks); status.Available && status.Git {
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

func projectDirectoryBreadcrumbs(path string) []projectDirectoryBreadcrumb {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	separator := string(filepath.Separator)
	breadcrumbs := make([]projectDirectoryBreadcrumb, 0, 8)
	current := volume
	if strings.HasPrefix(remainder, separator) {
		current += separator
		label := current
		if label == "" {
			label = separator
		}
		breadcrumbs = append(breadcrumbs, projectDirectoryBreadcrumb{Label: label, Path: current})
		remainder = strings.TrimPrefix(remainder, separator)
	}
	for _, part := range strings.Split(remainder, separator) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		breadcrumbs = append(breadcrumbs, projectDirectoryBreadcrumb{Label: part, Path: current})
	}
	return breadcrumbs
}

func projectDirectoryErrorStatus(err error) int {
	switch {
	case errors.Is(err, os.ErrPermission):
		return http.StatusForbidden
	case errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func canonicalBrowseDirectory(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		var err error
		path, err = os.UserHomeDir()
		if err != nil || path == "" {
			path, err = os.Getwd()
		}
		if err != nil {
			return "", fmt.Errorf("find starting directory: %w", err)
		}
	}
	if containsProjectControl(path) {
		return "", fmt.Errorf("path contains control characters")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("open directory: %w", err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	return resolved, nil
}

func listProjectDirectories(path string, showHidden bool, existing map[string]string) (projectDirectoryResponse, error) {
	resolved, err := canonicalBrowseDirectory(path)
	if err != nil {
		return projectDirectoryResponse{}, err
	}
	dir, err := os.Open(resolved)
	if err != nil {
		return projectDirectoryResponse{}, fmt.Errorf("open directory: %w", err)
	}
	defer dir.Close()

	result := projectDirectoryResponse{
		Path:        resolved,
		Breadcrumbs: projectDirectoryBreadcrumbs(resolved),
		Entries:     make([]projectDirectoryEntry, 0, 32),
	}
	if parent := filepath.Dir(resolved); parent != resolved {
		result.Parent = parent
	}
	if home, homeErr := canonicalBrowseDirectory(""); homeErr == nil {
		result.Home = home
	}

	scanned := 0
	for scanned < maxProjectDirectoryScanEntries && len(result.Entries) < maxProjectDirectoryEntries {
		batch, readErr := dir.ReadDir(128)
		for _, entry := range batch {
			scanned++
			if scanned > maxProjectDirectoryScanEntries {
				result.Truncated = true
				break
			}
			name := entry.Name()
			if !showHidden && strings.HasPrefix(name, ".") {
				continue
			}
			fullPath := filepath.Join(resolved, name)
			isDir := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				info, statErr := os.Stat(fullPath)
				isDir = statErr == nil && info.IsDir()
			}
			if !isDir {
				continue
			}
			item := projectDirectoryEntry{Name: name, Path: fullPath}
			if _, statErr := os.Stat(filepath.Join(fullPath, ".git")); statErr == nil {
				item.Git = true
			}
			identity := fullPath
			if canonical, canonicalErr := filepath.EvalSymlinks(fullPath); canonicalErr == nil {
				identity = canonicalProjectStoragePath(canonical, runtime.GOOS)
			}
			item.ExistingProjectID = existing[identity]
			result.Entries = append(result.Entries, item)
			if len(result.Entries) == maxProjectDirectoryEntries {
				result.Truncated = true
				break
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return projectDirectoryResponse{}, fmt.Errorf("read directory: %w", readErr)
		}
	}
	if scanned >= maxProjectDirectoryScanEntries {
		result.Truncated = true
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		left, right := result.Entries[i], result.Entries[j]
		if left.Git != right.Git {
			return left.Git
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
	return result, nil
}

func (s *serveServer) projectBrowseDirectoryAllowed(path string, projects []session.Project) bool {
	roots := make([]string, 0, len(projects)+2)
	if home, err := canonicalBrowseDirectory(""); err == nil {
		roots = append(roots, home)
	}
	if startup := strings.TrimSpace(s.startupDir); startup != "" {
		if resolved, err := canonicalBrowseDirectory(startup); err == nil {
			roots = append(roots, resolved)
		}
	}
	for _, project := range projects {
		if root := strings.TrimSpace(project.CanonicalDir); root != "" {
			roots = append(roots, root)
		}
	}
	for _, root := range roots {
		if pathWithinDirForOS(path, root, runtime.GOOS) {
			return true
		}
	}
	return false
}

func (s *serveServer) handleProjectDirectories(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	existing := make(map[string]string)
	var registered []session.Project
	if projects, ok := s.projectStore(); ok {
		if list, err := projects.ListProjects(r.Context(), session.ProjectListOptions{IncludeArchived: true}); err == nil {
			registered = list
			for _, project := range list {
				existing[canonicalProjectStoragePath(project.CanonicalDir, runtime.GOOS)] = project.ID
			}
		}
	}
	requested, err := canonicalBrowseDirectory(r.URL.Query().Get("path"))
	if err != nil {
		writeProjectError(w, projectDirectoryErrorStatus(err), "directory_unavailable", err.Error())
		return
	}
	if !s.projectBrowseDirectoryAllowed(requested, registered) {
		writeProjectError(w, http.StatusForbidden, "directory_unavailable", "directory is outside the allowed browse roots")
		return
	}
	listing, err := listProjectDirectories(requested, r.URL.Query().Get("show_hidden") == "1", existing)
	if err != nil {
		writeProjectError(w, projectDirectoryErrorStatus(err), "directory_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listing)
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
		statusCtx, cancelStatus := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancelStatus()
		for i, p := range list {
			result = append(result, s.cachedProjectStatus(statusCtx, p, i < maxEagerProjectStatusChecks))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": result})
	case http.MethodPost:
		var req projectCreateRequest
		if err := decodeSmallJSON(w, r, &req); err != nil {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", "invalid project request: "+err.Error())
			return
		}
		resolved, err := resolveProjectPathContext(r.Context(), req.Path)
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
		claimed, claimErr := projectpkg.ClaimMatchingSessions(r.Context(), s.store, *p)
		if claimErr != nil {
			log.Printf("[serve] reconcile project history after create: id=%s: %v", p.ID, claimErr)
		}
		api := projectStatus(*p)
		response.Project = &api
		response.Restored = wasArchived
		log.Printf("[serve] project %s: id=%s path=%s claimed=%d", map[bool]string{true: "restored", false: "created"}[wasArchived], p.ID, p.CanonicalDir, claimed)
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
		wasArchived := p.Archived()
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
		claimed := 0
		if wasArchived && req.Archived != nil && !*req.Archived {
			claimed, err = projectpkg.ClaimMatchingSessions(r.Context(), s.store, *updated)
			if err != nil {
				log.Printf("[serve] reconcile project history after restore: id=%s: %v", updated.ID, err)
			}
		}
		log.Printf("[serve] project %s: id=%s path=%s claimed=%d", action, updated.ID, updated.CanonicalDir, claimed)
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
	statusCtx, cancelStatus := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancelStatus()
	for i := range groups {
		if groups[i].Project != nil {
			api := s.cachedProjectStatus(statusCtx, *groups[i].Project, i < maxEagerProjectStatusChecks)
			groups[i].Project = &api.Project
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func sessionProjectCandidateFor(ctx context.Context, persisted *session.Session, projects session.ProjectStore) (*sessionProjectCandidate, error) {
	if persisted == nil {
		return nil, nil
	}
	candidatePath := strings.TrimSpace(persisted.CWD)
	if candidatePath == "" {
		candidatePath = strings.TrimSpace(persisted.WorktreeDir)
	}
	if candidatePath == "" {
		return nil, nil
	}
	resolved, err := resolveProjectPathContext(ctx, candidatePath)
	if err != nil {
		return nil, nil
	}
	candidate := &sessionProjectCandidate{CanonicalDir: resolved.CanonicalDir, DefaultName: resolved.DefaultName, Git: resolved.Git}
	existing, err := projects.GetProjectByCanonicalDir(ctx, resolved.CanonicalDir)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		candidate.ExistingProjectID = existing.ID
		candidate.ExistingName = existing.Name
		candidate.ExistingArchived = existing.Archived()
	}
	return candidate, nil
}

func (s *serveServer) handleSessionProjectAssignment(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	if r.Method == http.MethodGet {
		persisted, err := s.store.Get(r.Context(), sessionID)
		if err != nil || persisted == nil {
			writeProjectError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		info := sessionProjectAssignmentInfo{CWD: persisted.CWD, WorktreeDir: persisted.WorktreeDir, ProjectID: persisted.ProjectID}
		if persisted.ProjectID == "" {
			info.Candidate, err = sessionProjectCandidateFor(r.Context(), persisted, projects)
			if err != nil {
				writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not inspect the conversation workspace")
				return
			}
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	var req sessionProjectAssignmentRequest
	if err := decodeSmallJSON(w, r, &req); err != nil {
		writeProjectError(w, http.StatusBadRequest, "invalid_project", "invalid project assignment request: "+err.Error())
		return
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	if (req.ProjectID == "" && !req.CreateFromWorkspace) || (req.ProjectID != "" && req.CreateFromWorkspace) {
		writeProjectError(w, http.StatusBadRequest, "invalid_project", "provide project_id or create_from_workspace")
		return
	}
	var guardedRuntime *serveRuntime
	persisted, err := s.store.Get(r.Context(), sessionID)
	if err != nil || persisted == nil {
		writeProjectError(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	if strings.TrimSpace(persisted.ProjectID) != "" {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "this conversation already has a project")
		return
	}
	var project *session.Project
	if req.CreateFromWorkspace {
		candidate, candidateErr := sessionProjectCandidateFor(r.Context(), persisted, projects)
		if candidateErr != nil {
			writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "could not inspect the conversation workspace")
			return
		}
		if candidate == nil {
			writeProjectError(w, http.StatusConflict, "candidate_unavailable", "the conversation workspace cannot be registered as a project")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = candidate.DefaultName
			if candidate.ExistingName != "" {
				name = candidate.ExistingName
			}
		}
		project = &session.Project{Name: name, CanonicalDir: candidate.CanonicalDir}
		if err := projects.CreateProject(r.Context(), project); err != nil && !errors.Is(err, session.ErrProjectDuplicate) {
			writeProjectError(w, http.StatusBadRequest, "invalid_project", err.Error())
			return
		}
		s.clearProjectStatusCache()
		log.Printf("[serve] project upgraded from conversation workspace: id=%s path=%s", project.ID, project.CanonicalDir)
	} else {
		project, err = projects.GetProject(r.Context(), req.ProjectID)
		if err != nil || project == nil {
			writeProjectError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
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
	if !sessionMatchesProjectBindingContext(r.Context(), *persisted, resolvedProjectPath{CanonicalDir: project.CanonicalDir, Git: status.Git}) {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "the persisted conversation workspace does not match this project")
		return
	}

	// Keep the process-wide session map lock only around the final race-sensitive
	// recheck and assignment. Project discovery and Git inspection above may be
	// slow and must not block unrelated conversations.
	var releaseAssignmentGuard func()
	if s.sessionMgr != nil {
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
	}
	releaseGuard := func() {
		if releaseAssignmentGuard != nil {
			release := releaseAssignmentGuard
			releaseAssignmentGuard = nil
			release()
		}
	}
	defer releaseGuard()
	latest, getErr := s.store.Get(r.Context(), sessionID)
	if getErr != nil || latest == nil || latest.ProjectID != "" || latest.CWD != persisted.CWD || latest.WorktreeDir != persisted.WorktreeDir {
		writeProjectError(w, http.StatusConflict, "workspace_conflict", "the conversation workspace changed while it was being inspected")
		return
	}
	persisted = latest
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
	releaseGuard()
	claimed, claimErr := projectpkg.ClaimMatchingSessions(r.Context(), s.store, *project)
	if claimErr != nil {
		log.Printf("[serve] reconcile project history after assignment: id=%s: %v", project.ID, claimErr)
	} else if claimed > 0 {
		log.Printf("[serve] project history reconciled after assignment: id=%s claimed=%d", project.ID, claimed)
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
