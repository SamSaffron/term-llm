package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func newServeProjectTestServer(t *testing.T) (*serveServer, *session.SQLiteStore) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &serveServer{store: store, projectsEnabled: true}, store
}

func initGitProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v: %s", err, out)
	}
	return dir
}

func TestResolveServeProjectsRequestedTriStateAndWebScope(t *testing.T) {
	for _, tc := range []struct {
		name                                                   string
		projectsSet, projectsValue, cmdNoProjects, config, web bool
		wantEnabled, wantStrict                                bool
	}{
		{"default enabled for web", false, false, false, true, true, true, false},
		{"config disables", false, false, false, false, true, false, false},
		{"strict flag overrides disabled config", true, true, false, false, true, true, true},
		{"explicit projects false overrides enabled config", true, false, false, true, true, false, false},
		{"no-projects overrides enabled config", false, false, true, true, true, false, false},
		{"non-web never initializes", true, true, false, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled, strict := resolveServeProjectsRequested(tc.projectsSet, tc.projectsValue, tc.cmdNoProjects, tc.config, tc.web)
			if enabled != tc.wantEnabled || strict != tc.wantStrict {
				t.Fatalf("resolve = (%v,%v), want (%v,%v)", enabled, strict, tc.wantEnabled, tc.wantStrict)
			}
		})
	}
}

func TestResolveProjectPathValidationAndGitNormalization(t *testing.T) {
	nonGit := t.TempDir()
	resolved, err := resolveProjectPath(nonGit)
	if err != nil || resolved.Git || !sameServePath(resolved.CanonicalDir, nonGit) {
		t.Fatalf("non-Git resolution = %#v, %v", resolved, err)
	}
	gitRoot := initGitProject(t)
	subdir := filepath.Join(gitRoot, "packages", "web")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolveProjectPath(subdir)
	if err != nil || !resolved.Git || !sameServePath(resolved.CanonicalDir, gitRoot) {
		t.Fatalf("Git resolution = %#v, %v", resolved, err)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(subdir, link); err == nil {
		linked, err := resolveProjectPath(link)
		if err != nil || !sameServePath(linked.CanonicalDir, gitRoot) {
			t.Fatalf("symlink resolution = %#v, %v", linked, err)
		}
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	invalid := []string{"relative", "~/repo", "$HOME/repo", string(filepath.Separator), file, filepath.Join(t.TempDir(), "missing"), nonGit + "\x00bad"}
	for _, path := range invalid {
		if _, err := resolveProjectPath(path); err == nil {
			t.Errorf("resolveProjectPath(%q) unexpectedly succeeded", path)
		}
	}
}

func TestProjectDirectoryBrowserListsFoldersAndMetadata(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	gitDir := filepath.Join(root, "repository")
	hidden := filepath.Join(root, ".hidden")
	for _, dir := range []string{plain, filepath.Join(gitDir, ".git"), hidden} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-folder.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	listing, err := listProjectDirectories(root, false, map[string]string{canonicalProjectStoragePath(plain, runtime.GOOS): "prj_plain"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameServePath(listing.Path, root) || listing.Parent == "" || len(listing.Breadcrumbs) == 0 {
		t.Fatalf("listing navigation metadata = %#v", listing)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("entries = %#v, want two visible directories", listing.Entries)
	}
	if listing.Entries[0].Name != "repository" || !listing.Entries[0].Git {
		t.Fatalf("Git directory was not sorted and badged first: %#v", listing.Entries)
	}
	if listing.Entries[1].Name != "plain" || listing.Entries[1].ExistingProjectID != "prj_plain" {
		t.Fatalf("existing project metadata missing: %#v", listing.Entries[1])
	}

	withHidden, err := listProjectDirectories(root, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(withHidden.Entries) != 3 || withHidden.Entries[0].Name != "repository" {
		t.Fatalf("show-hidden entries = %#v", withHidden.Entries)
	}
}

func TestProjectDirectoryHandlerValidation(t *testing.T) {
	srv, _ := newServeProjectTestServer(t)
	root := t.TempDir()
	srv.startupDir = root
	target := "/v1/project-directories?path=" + url.QueryEscape(root)
	rr := httptest.NewRecorder()
	srv.handleProjectDirectories(rr, httptest.NewRequest(http.MethodGet, target, nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"breadcrumbs"`) {
		t.Fatalf("directory listing status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.handleProjectDirectories(rr, httptest.NewRequest(http.MethodPost, target, nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("invalid method status=%d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}

	rr = httptest.NewRecorder()
	srv.handleProjectDirectories(rr, httptest.NewRequest(http.MethodGet, "/v1/project-directories?path=relative", nil))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "path must be absolute") {
		t.Fatalf("relative path status=%d body=%s", rr.Code, rr.Body.String())
	}

	outside := t.TempDir()
	rr = httptest.NewRecorder()
	srv.handleProjectDirectories(rr, httptest.NewRequest(http.MethodGet, "/v1/project-directories?path="+url.QueryEscape(outside), nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("outside path status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCanonicalProjectStoragePathUsesWindowsCaseIdentity(t *testing.T) {
	upper := canonicalProjectStoragePath(`C:\\Users\\Operator\\Repo`, "windows")
	lower := canonicalProjectStoragePath(`c:\\users\\operator\\repo`, "windows")
	if upper != lower {
		t.Fatalf("Windows canonical forms differ: %q != %q", upper, lower)
	}
	if got := canonicalProjectStoragePath("/Srv/Repo", "linux"); got != filepath.Clean("/Srv/Repo") {
		t.Fatalf("Unix canonical form changed case: %q", got)
	}
	if !pathWithinDirForOS("/ROOT/Managed/Child", "/root/managed", "windows") {
		t.Fatal("Windows containment did not use case-insensitive identity")
	}
	if pathWithinDirForOS("/root/managed-other", "/root/managed", "windows") {
		t.Fatal("Windows containment ignored a path-component boundary")
	}
}

func TestProjectCreateClaimsMatchingHistoricalSessions(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	projectDir := t.TempDir()
	otherDir := t.TempDir()
	ctx := context.Background()
	matching := &session.Session{ID: "historical-project-match", Provider: "mock", Model: "mock", CWD: projectDir, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	other := &session.Session{ID: "historical-project-other", Provider: "mock", Model: "mock", CWD: otherDir, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	for _, sess := range []*session.Session{matching, other} {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(`{"path":`+mustJSONQuote(projectDir)+`}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleProjects(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response projectCreateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.Project == nil {
		t.Fatalf("response = %#v, %v", response, err)
	}
	gotMatching, _ := store.Get(ctx, matching.ID)
	gotOther, _ := store.Get(ctx, other.ID)
	if gotMatching.ProjectID != response.Project.ID || gotOther.ProjectID != "" {
		t.Fatalf("historical assignment = matching %#v, other %#v", gotMatching, gotOther)
	}
}

func TestProjectPatchRestoreClaimsSessionsCreatedWhileArchived(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	p := &session.Project{Name: "Restorable", CanonicalDir: root}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := store.UpdateProject(ctx, p.ID, session.ProjectUpdate{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	legacy := &session.Session{ID: "archived-project-history", Provider: "mock", Model: "mock", CWD: root, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/v1/projects/"+p.ID, strings.NewReader(`{"archived":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleProjectByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", rr.Code, rr.Body.String())
	}
	persisted, err := store.Get(ctx, legacy.ID)
	if err != nil || persisted.ProjectID != p.ID {
		t.Fatalf("restored history = %#v, %v", persisted, err)
	}
}

func TestProjectHandlersDryRunCreateDuplicateArchiveRestore(t *testing.T) {
	srv, _ := newServeProjectTestServer(t)
	projectDir := t.TempDir()
	request := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		if target == "/v1/projects" || strings.HasPrefix(target, "/v1/projects?") {
			srv.handleProjects(rr, req)
		} else {
			srv.handleProjectByID(rr, req)
		}
		return rr
	}
	body := `{"path":` + mustJSONQuote(projectDir) + `}`
	longNameBody := `{"path":` + mustJSONQuote(projectDir) + `,"name":` + mustJSONQuote(strings.Repeat("x", 121)) + `}`
	for _, target := range []string{"/v1/projects?dry_run=1", "/v1/projects"} {
		invalid := request(http.MethodPost, target, longNameBody)
		if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "1 to 120") {
			t.Fatalf("invalid name %s status=%d body=%s", target, invalid.Code, invalid.Body.String())
		}
	}
	dry := request(http.MethodPost, "/v1/projects?dry_run=1", body)
	if dry.Code != http.StatusOK {
		t.Fatalf("dry run status=%d body=%s", dry.Code, dry.Body.String())
	}
	var preview projectCreateResponse
	if err := json.Unmarshal(dry.Body.Bytes(), &preview); err != nil || !sameServePath(preview.CanonicalDir, projectDir) {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	created := request(http.MethodPost, "/v1/projects", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var response projectCreateResponse
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil || response.Project == nil {
		t.Fatalf("create response = %#v, %v", response, err)
	}
	id := response.Project.ID
	listed := request(http.MethodGet, "/v1/projects", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"conversation_count":0`) {
		t.Fatalf("empty project list status=%d body=%s", listed.Code, listed.Body.String())
	}
	duplicate := request(http.MethodPost, "/v1/projects", body)
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), id) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	archived := request(http.MethodPatch, "/v1/projects/"+id, `{"archived":true}`)
	if archived.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", archived.Code, archived.Body.String())
	}
	restored := request(http.MethodPost, "/v1/projects", body)
	if restored.Code != http.StatusCreated || !strings.Contains(restored.Body.String(), `"restored":true`) || !strings.Contains(restored.Body.String(), id) {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
}

func mustJSONQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestKeepCachedProjectStatusPrefersFreshDetailAndNewerResults(t *testing.T) {
	updated := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	started := updated.Add(time.Minute)
	now := started.Add(time.Second)
	project := session.Project{UpdatedAt: updated}
	for _, tc := range []struct {
		name     string
		entry    projectStatusCacheEntry
		detailed bool
		want     bool
	}{
		{name: "fresh detail over cheap", entry: projectStatusCacheEntry{updatedAt: updated, checkedAt: started, detailed: true}, want: true},
		{name: "stale detail yields to cheap", entry: projectStatusCacheEntry{updatedAt: updated, checkedAt: now.Add(-projectStatusCacheTTL - time.Second), detailed: true}, want: false},
		{name: "newer equivalent result", entry: projectStatusCacheEntry{updatedAt: updated, checkedAt: started.Add(time.Millisecond), detailed: true}, detailed: true, want: true},
		{name: "older equivalent result", entry: projectStatusCacheEntry{updatedAt: updated, checkedAt: started.Add(-time.Millisecond), detailed: true}, detailed: true, want: false},
		{name: "detail upgrades cheap", entry: projectStatusCacheEntry{updatedAt: updated, checkedAt: started.Add(time.Millisecond)}, detailed: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepCachedProjectStatus(tc.entry, project, started, now, tc.detailed); got != tc.want {
				t.Fatalf("keepCachedProjectStatus() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveWorkspacePolicyAndImmutableBinding(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	project := &session.Project{Name: "Project", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	binding, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: "fresh", ProjectID: project.ID, FirstPartyUI: true, FreshConversation: true})
	if err != nil || binding.ProjectID != project.ID || !sameServePath(binding.RuntimeDir, root) {
		t.Fatalf("fresh binding = %#v, %v", binding, err)
	}
	if _, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: "fresh", FirstPartyUI: true, FreshConversation: true}); err == nil {
		t.Fatal("missing first-party project unexpectedly succeeded")
	}
	noProjectRoot := t.TempDir()
	srv.startupDir = noProjectRoot
	binding, err = srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: "fresh", FirstPartyUI: true, FreshConversation: true, AllowNoProject: true})
	if err != nil || binding.ProjectID != "" || !sameServePath(binding.RuntimeDir, noProjectRoot) {
		t.Fatalf("explicit no-project binding = %#v, %v", binding, err)
	}
	now := time.Now()
	noProjectShell := &session.Session{ID: "no-project-shell", Provider: "mock", Model: "mock", Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, noProjectShell); err != nil {
		t.Fatal(err)
	}
	if err := srv.bindResolvedWorkspace(ctx, noProjectShell.ID, nil, binding); err != nil {
		t.Fatalf("bind explicit no-project workspace: %v", err)
	}
	persistedNoProject, err := store.Get(ctx, noProjectShell.ID)
	if err != nil || persistedNoProject.ProjectID != "" || !sameServePath(persistedNoProject.CWD, noProjectRoot) {
		t.Fatalf("persisted no-project workspace = %#v, %v", persistedNoProject, err)
	}
	now = time.Now()
	sess := &session.Session{ID: "bound", Provider: "mock", Model: "mock", Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	binder, ok := session.AsSessionWorkspaceBinder(store)
	if !ok {
		t.Fatal("workspace binder unavailable")
	}
	if _, err := binder.BindSessionWorkspace(ctx, sess.ID, session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: root}); err != nil {
		t.Fatal(err)
	}
	other := &session.Project{Name: "Other", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	_, err = srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: sess.ID, ProjectID: other.ID})
	var typed *serveWorkspaceError
	if !errors.As(err, &typed) || typed.Code != "workspace_conflict" || typed.Status != http.StatusConflict {
		t.Fatalf("conflict error = %#v", err)
	}

	archive := true
	if _, err := store.UpdateProject(ctx, project.ID, session.ProjectUpdate{Archived: &archive}); err != nil {
		t.Fatal(err)
	}
	_, err = srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: sess.ID, ProjectID: project.ID, FirstPartyUI: true, FreshConversation: true})
	if !errors.As(err, &typed) || typed.Code != "project_archived" {
		t.Fatalf("fresh archived project error = %#v", err)
	}
	if _, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: sess.ID, ProjectID: project.ID}); err != nil {
		t.Fatalf("archived existing session did not resume: %v", err)
	}

	binding, err = srv.resolveWorkspace(ctx, serveWorkspaceRequest{WorktreeDir: t.TempDir(), FirstPartyUI: false})
	if err != nil || binding.ProjectID != "" {
		t.Fatalf("third-party explicit workspace compatibility = %#v, %v", binding, err)
	}
	_, err = srv.resolveWorkspace(ctx, serveWorkspaceRequest{WorktreeDir: t.TempDir(), FirstPartyUI: true})
	if !errors.As(err, &typed) || typed.Code != "project_required" {
		t.Fatalf("first-party arbitrary worktree error = %#v", err)
	}
}

func TestProjectWorkspaceRejectsCrossProjectAndFallsBackOnlyAfterManagedWorktreeDisappears(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	repoA, repoB := newGitRepoForBindingTest(t), newGitRepoForBindingTest(t)
	projectA, projectB := &session.Project{Name: "A", CanonicalDir: repoA}, &session.Project{Name: "B", CanonicalDir: repoB}
	for _, project := range []*session.Project{projectA, projectB} {
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	managedA, err := worktree.Create(ctx, repoA, worktree.CreateOptions{Name: "project-a"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managedA.Dir, worktree.RemoveOptions{Force: true}) })
	var typed *serveWorkspaceError
	if _, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{ProjectID: projectB.ID, WorktreeDir: managedA.Dir, FirstPartyUI: true, FreshConversation: true}); !errors.As(err, &typed) || typed.Code != "workspace_conflict" {
		t.Fatalf("cross-project worktree error = %#v", err)
	}

	externalDir := filepath.Join(t.TempDir(), "external-worktree")
	cmd := exec.Command("git", "-C", repoA, "worktree", "add", "--detach", externalDir, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create external worktree: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", repoA, "worktree", "remove", "--force", externalDir).Run() })
	if _, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{ProjectID: projectA.ID, WorktreeDir: externalDir, FirstPartyUI: true, FreshConversation: true}); !errors.As(err, &typed) || typed.Code != "workspace_conflict" {
		t.Fatalf("external worktree error = %#v", err)
	}

	now := time.Now()
	inconsistent := &session.Session{ID: "inconsistent-project-worktree", Provider: "mock", Model: "mock", ProjectID: projectA.ID, CWD: repoA, WorktreeDir: managedA.Dir, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, inconsistent); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: inconsistent.ID, ProjectID: projectA.ID}); !errors.As(err, &typed) || typed.Code != "project_unavailable" {
		t.Fatalf("inconsistent persisted snapshot error = %#v", err)
	}
	persisted := &session.Session{ID: "missing-project-worktree", Provider: "mock", Model: "mock", ProjectID: projectA.ID, CWD: managedA.Dir, WorktreeDir: managedA.Dir, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	if err := worktree.Remove(ctx, managedA.Dir, worktree.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	binding, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: persisted.ID, ProjectID: projectA.ID})
	if err != nil || !sameServePath(binding.RuntimeDir, repoA) || binding.WorktreeDir != managedA.Dir {
		t.Fatalf("missing managed worktree fallback = %#v, %v", binding, err)
	}
	after, err := store.Get(ctx, persisted.ID)
	if err != nil || after.CWD != managedA.Dir || after.WorktreeDir != managedA.Dir {
		t.Fatalf("fallback rewrote immutable snapshot = %#v, %v", after, err)
	}
	toolCfg := tools.DefaultToolConfig()
	manager, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreWorktreeBinding(ctx, store, after, manager); err != nil {
		t.Fatalf("restore missing project worktree: %v", err)
	}
	if !sameServePath(manager.BaseDir(), repoA) {
		t.Fatalf("restored missing worktree base = %q, want %q", manager.BaseDir(), repoA)
	}
}

func TestWorkspaceResolverRetriesOnlyEmptyFirstBindShell(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	project := &session.Project{Name: "Retry shell", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	shell := &session.Session{
		ID: "retry-first-bind", Provider: "mock", Model: "mock", Origin: session.OriginWeb,
		CreatedAt: now, UpdatedAt: now, Status: session.StatusActive,
	}
	if err := store.Create(ctx, shell); err != nil {
		t.Fatal(err)
	}
	var typed *serveWorkspaceError
	for name, req := range map[string]serveWorkspaceRequest{
		"third-party": {SessionID: shell.ID, ProjectID: project.ID, FreshConversation: true},
		"resume":      {SessionID: shell.ID, ProjectID: project.ID, FirstPartyUI: true},
	} {
		if _, err := srv.resolveWorkspace(ctx, req); !errors.As(err, &typed) || typed.Code != "workspace_conflict" {
			t.Fatalf("%s shell resolution error = %#v", name, err)
		}
	}
	binding, err := srv.resolveWorkspace(ctx, serveWorkspaceRequest{
		SessionID: shell.ID, ProjectID: project.ID, FirstPartyUI: true, FreshConversation: true,
	})
	if err != nil || binding.ProjectID != project.ID || !sameServePath(binding.RuntimeDir, root) {
		t.Fatalf("fresh shell retry binding = %#v, %v", binding, err)
	}
	if err := srv.bindResolvedWorkspace(ctx, shell.ID, nil, binding); err != nil {
		t.Fatalf("complete retried binding: %v", err)
	}
	persisted, err := store.Get(ctx, shell.ID)
	if err != nil || persisted.ProjectID != project.ID || !sameServePath(persisted.CWD, root) {
		t.Fatalf("retried shell persisted = %#v, %v", persisted, err)
	}
}

func TestBindingProjectSetsRuntimeBaseWithoutGrantingAuthority(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	project := &session.Project{Name: "Project", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persisted := &session.Session{ID: "runtime-bind", Provider: "mock", Model: "mock", CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	manager, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{toolMgr: manager}
	binding := serveWorkspaceBinding{ProjectID: project.ID, RootDir: root, RuntimeDir: root}
	if err := srv.bindResolvedWorkspace(ctx, persisted.ID, rt, binding); err != nil {
		t.Fatal(err)
	}
	if !sameServePath(manager.BaseDir(), root) {
		t.Fatalf("tool base = %q, want %q", manager.BaseDir(), root)
	}
	if manager.ApprovalMgr.IsWorkspacePathAllowed(filepath.Join(root, "file.txt"), false) || manager.ApprovalMgr.IsWorkspacePathAllowed(filepath.Join(root, "file.txt"), true) {
		t.Fatal("project selection granted file authority before workspace approval")
	}
	grants, err := store.ListWorkspaceGrants(ctx, persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		if grant.Provenance != "primary-proposed" {
			t.Fatalf("project selection created an authoritative grant: %#v", grants)
		}
	}
	prompted := 0
	manager.ApprovalMgr.PromptUIFunc = func(path string, isWrite, isShell bool, workDir string) (tools.ApprovalResult, error) {
		prompted++
		return tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}, nil
	}
	outcome, err := manager.ApprovalMgr.CheckShellApproval("printf project-binding", root)
	if err != nil || outcome != tools.Cancel || prompted != 1 {
		t.Fatalf("project selection bypassed shell approval: outcome=%v prompted=%d err=%v", outcome, prompted, err)
	}
}

func TestBindingApplicationFailureKeepsDurableWorkspaceForRetry(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	missingRoot := filepath.Join(t.TempDir(), "appears-later")
	project := &session.Project{Name: "Retry", CanonicalDir: missingRoot}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persisted := &session.Session{ID: "binding-application-retry", Provider: "mock", Model: "mock", CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	toolCfg := tools.DefaultToolConfig()
	manager, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	binding := serveWorkspaceBinding{ProjectID: project.ID, RootDir: missingRoot, RuntimeDir: missingRoot}
	if err := srv.bindResolvedWorkspace(ctx, persisted.ID, &serveRuntime{toolMgr: manager}, binding); err == nil {
		t.Fatal("binding application unexpectedly accepted a missing runtime directory")
	}
	bound, err := store.Get(ctx, persisted.ID)
	if err != nil || bound.ProjectID != project.ID || bound.CWD != missingRoot {
		t.Fatalf("durable binding was rolled back after runtime failure: %#v, %v", bound, err)
	}
	if err := os.Mkdir(missingRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := srv.bindResolvedWorkspace(ctx, persisted.ID, &serveRuntime{toolMgr: manager}, binding); err != nil {
		t.Fatalf("matching retry did not apply committed binding: %v", err)
	}
	if !sameServePath(manager.BaseDir(), missingRoot) {
		t.Fatalf("retry base dir = %q", manager.BaseDir())
	}
}

func TestReadOnlyCurrentSchemaRestoresProjectSessionThroughProjectReader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "readonly-projects.db")
	writable, err := session.NewStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	projects, ok := session.AsProjectStore(writable)
	if !ok {
		t.Fatal("writable project store unavailable")
	}
	project := &session.Project{Name: "Read only", CanonicalDir: root}
	if err := projects.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persisted := &session.Session{ID: "readonly-project-session", Provider: "mock", Model: "mock", Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := writable.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	binder, _ := session.AsSessionWorkspaceBinder(writable)
	if _, err := binder.BindSessionWorkspace(ctx, persisted.ID, session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: root}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := session.NewStore(session.Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, ok := session.AsProjectStore(readOnly); ok {
		t.Fatal("read-only database advertised project mutation")
	}
	if _, ok := session.AsProjectReader(readOnly); !ok {
		t.Fatal("read-only current schema did not advertise project reads")
	}
	toolCfg := tools.DefaultToolConfig()
	manager, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: readOnly, projectsEnabled: false}
	if err := srv.ensureRuntimeBaseDirForSession(ctx, persisted.ID, &serveRuntime{toolMgr: manager}); err != nil {
		t.Fatal(err)
	}
	if !sameServePath(manager.BaseDir(), root) {
		t.Fatalf("read-only restored base = %q, want %q", manager.BaseDir(), root)
	}
}

func TestProjectDiscoveryRevalidatesCanonicalIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink replacement requires platform privileges")
	}
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	project := &session.Project{Name: "Discovery", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persisted := &session.Session{ID: "project-discovery", Provider: "mock", Model: "mock", ProjectID: project.ID, CWD: root, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	resolved, err := srv.sessionForProjectDiscovery(ctx, persisted)
	if err != nil || !sameServePath(resolved.CWD, root) {
		t.Fatalf("resolved discovery session = %#v, %v", resolved, err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, root); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := srv.sessionForProjectDiscovery(ctx, persisted); err == nil {
		t.Fatal("project-local discovery accepted replaced canonical root")
	}
}

func TestProjectMentionAndSkillDiscoveryStayWithinResolvedBinding(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	projectA, projectB := &session.Project{Name: "A", CanonicalDir: t.TempDir()}, &session.Project{Name: "B", CanonicalDir: t.TempDir()}
	for _, project := range []*session.Project{projectA, projectB} {
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	persisted := &session.Session{ID: "project-discovery-scope", Provider: "mock", Model: "mock", ProjectID: projectA.ID, CWD: projectA.CanonicalDir, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	skillSession, err := srv.sessionForProjectDiscovery(ctx, persisted)
	if err != nil || !sameServePath(skillSession.CWD, projectA.CanonicalDir) {
		t.Fatalf("skill discovery binding = %#v, %v", skillSession, err)
	}
	mentionRoot, err := srv.resolveMentionSearchRootForProject(ctx, "", projectB.ID, "", false, true)
	if err != nil || !sameServePath(mentionRoot, projectB.CanonicalDir) {
		t.Fatalf("draft mention root = %q, %v", mentionRoot, err)
	}
	noProjectRoot := t.TempDir()
	srv.startupDir = noProjectRoot
	noProjectSession := &session.Session{ID: "no-project-discovery", Provider: "mock", Model: "mock", CWD: noProjectRoot, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, noProjectSession); err != nil {
		t.Fatal(err)
	}
	mentionRoot, err = srv.resolveMentionSearchRootForProject(ctx, noProjectSession.ID, "", "", true, true)
	if err != nil || !sameServePath(mentionRoot, noProjectRoot) {
		t.Fatalf("no-project mention root = %q, %v", mentionRoot, err)
	}
	if _, err := srv.resolveMentionSearchRootForProject(ctx, persisted.ID, projectB.ID, "", false, true); err == nil {
		t.Fatal("persisted project A session accepted project B mention context")
	}
	if _, err := srv.resolveMentionSearchRootForProject(ctx, "", projectA.ID, t.TempDir(), false, true); err == nil {
		t.Fatal("project mention search accepted arbitrary worktree path")
	}
}

func TestSessionProjectAssignmentValidatesAndPreservesWorkspace(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	project := &session.Project{Name: "Root", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	createLegacy := func(id, cwd string) {
		now := time.Now()
		if err := store.Create(ctx, &session.Session{ID: id, Provider: "mock", Model: "mock", CWD: cwd, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}); err != nil {
			t.Fatal(err)
		}
	}
	createLegacy("matching", root)
	manager := newServeSessionManager(time.Hour, 10, nil)
	runtime := &serveRuntime{sessionMeta: &session.Session{ID: "matching", CWD: root}}
	putTestSession(manager, "matching", runtime)
	srv.sessionMgr = manager
	active := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	runtime.setActiveInterrupt(active)
	t.Cleanup(func() {
		runtime.clearActiveInterrupt(active)
		manager.Close()
	})
	requestProject := func(id, projectID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/project", strings.NewReader(`{"project_id":"`+projectID+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		return rr
	}
	request := func(id string) *httptest.ResponseRecorder {
		return requestProject(id, project.ID)
	}
	if rr := request("matching"); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "active response") {
		t.Fatalf("active assignment status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stillUnassigned, err := store.Get(ctx, "matching"); err != nil || stillUnassigned.ProjectID != "" {
		t.Fatalf("active assignment changed persisted session: %#v, %v", stillUnassigned, err)
	}
	runtime.clearActiveInterrupt(active)
	if rr := request("matching"); rr.Code != http.StatusOK {
		t.Fatalf("assignment status=%d body=%s", rr.Code, rr.Body.String())
	}
	assigned, _ := store.Get(ctx, "matching")
	if assigned.ProjectID != project.ID || assigned.CWD != root || assigned.WorktreeDir != "" {
		t.Fatalf("assigned workspace changed: %#v", assigned)
	}
	if runtime.sessionMeta == nil || runtime.sessionMeta.ProjectID != project.ID {
		t.Fatalf("cached runtime metadata not refreshed: %#v", runtime.sessionMeta)
	}
	if rr := request("matching"); rr.Code != http.StatusConflict {
		t.Fatalf("second assignment status=%d body=%s", rr.Code, rr.Body.String())
	}
	createLegacy("mismatch", t.TempDir())
	if rr := request("mismatch"); rr.Code != http.StatusConflict {
		t.Fatalf("mismatch status=%d body=%s", rr.Code, rr.Body.String())
	}
	now := time.Now()
	if err := store.Create(ctx, &session.Session{ID: "non-git-worktree", Provider: "mock", Model: "mock", CWD: root, WorktreeDir: t.TempDir(), CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}); err != nil {
		t.Fatal(err)
	}
	if rr := request("non-git-worktree"); rr.Code != http.StatusConflict {
		t.Fatalf("non-Git assignment with worktree status=%d body=%s", rr.Code, rr.Body.String())
	}
	goneRoot := t.TempDir()
	gone := &session.Project{Name: "Archived unavailable", CanonicalDir: goneRoot}
	if err := store.CreateProject(ctx, gone); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := store.UpdateProject(ctx, gone.ID, session.ProjectUpdate{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	createLegacy("archived-unavailable", goneRoot)
	if err := os.Remove(goneRoot); err != nil {
		t.Fatal(err)
	}
	if rr := requestProject("archived-unavailable", gone.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "project_unavailable") {
		t.Fatalf("archived unavailable assignment status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionProjectCandidateUpgradeCreatesAndAssigns(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	now := time.Now()
	legacy := &session.Session{ID: "upgrade-candidate", Provider: "mock", Model: "mock", CWD: root, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	infoRR := httptest.NewRecorder()
	srv.handleSessionByID(infoRR, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+legacy.ID+"/project", nil))
	if infoRR.Code != http.StatusOK {
		t.Fatalf("candidate status=%d body=%s", infoRR.Code, infoRR.Body.String())
	}
	var info sessionProjectAssignmentInfo
	if err := json.Unmarshal(infoRR.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Candidate == nil || !sameServePath(info.Candidate.CanonicalDir, root) || info.Candidate.DefaultName != filepath.Base(root) {
		t.Fatalf("candidate info = %#v", info)
	}

	upgradeReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+legacy.ID+"/project", strings.NewReader(`{"create_from_workspace":true,"name":"Upgraded workspace"}`))
	upgradeReq.Header.Set("Content-Type", "application/json")
	upgradeRR := httptest.NewRecorder()
	srv.handleSessionByID(upgradeRR, upgradeReq)
	if upgradeRR.Code != http.StatusOK {
		t.Fatalf("upgrade status=%d body=%s", upgradeRR.Code, upgradeRR.Body.String())
	}
	assigned, err := store.Get(ctx, legacy.ID)
	if err != nil || assigned == nil || assigned.ProjectID == "" || assigned.ProjectName != "Upgraded workspace" || !sameServePath(assigned.CWD, root) {
		t.Fatalf("upgraded session = %#v, %v", assigned, err)
	}
	project, err := store.GetProject(ctx, assigned.ProjectID)
	if err != nil || project == nil || project.Name != "Upgraded workspace" || !sameServePath(project.CanonicalDir, root) {
		t.Fatalf("created project = %#v, %v", project, err)
	}
}

func TestSessionMetadataPatchRejectsWorkspaceIdentityFields(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	now := time.Now()
	sess := &session.Session{ID: "protected", Provider: "mock", Model: "mock", CWD: t.TempDir(), CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"project_id":"prj_other"}`, `{"cwd":"/tmp/other"}`, `{"worktree_dir":"/tmp/other"}`} {
		req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+sess.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rr.Code, rr.Body.String())
		}
	}
	persisted, err := store.Get(context.Background(), sess.ID)
	if err != nil || persisted.CWD != sess.CWD || persisted.ProjectID != "" || persisted.WorktreeDir != "" {
		t.Fatalf("protected metadata changed: %#v, %v", persisted, err)
	}
}

func TestProjectByIDWithoutProjectStoreReturnsTypedUnavailable(t *testing.T) {
	srv := &serveServer{projectsEnabled: true}
	rr := httptest.NewRecorder()
	srv.handleProjectByID(rr, httptest.NewRequest(http.MethodGet, "/v1/projects/prj_missing", nil))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "projects_unavailable") {
		t.Fatalf("missing project store status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestProjectsDisabledRoutesAndResponseProjectRejection(t *testing.T) {
	srv, _ := newServeProjectTestServer(t)
	srv.projectsEnabled = false
	rr := httptest.NewRecorder()
	srv.handleProjects(rr, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "projects_disabled") {
		t.Fatalf("disabled projects status=%d body=%s", rr.Code, rr.Body.String())
	}
	_, err := srv.resolveWorkspace(context.Background(), serveWorkspaceRequest{ProjectID: "prj_spoofed"})
	var typed *serveWorkspaceError
	if !errors.As(err, &typed) || typed.Status != http.StatusBadRequest || typed.Code != "projects_disabled" {
		t.Fatalf("disabled project request = %#v", err)
	}
}

func TestDisabledModeRestoresExistingProjectSessionSnapshot(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	project := &session.Project{Name: "Existing", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persisted := &session.Session{ID: "disabled-project-resume", Provider: "mock", Model: "mock", Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	binder, _ := session.AsSessionWorkspaceBinder(store)
	if _, err := binder.BindSessionWorkspace(ctx, persisted.ID, session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: root}); err != nil {
		t.Fatal(err)
	}
	srv.projectsEnabled = false
	toolCfg := tools.DefaultToolConfig()
	manager, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{toolMgr: manager}
	if err := srv.ensureRuntimeBaseDirForSession(ctx, persisted.ID, rt); err != nil {
		t.Fatalf("restore disabled-mode project session: %v", err)
	}
	if !sameServePath(manager.BaseDir(), root) {
		t.Fatalf("restored base dir = %q, want %q", manager.BaseDir(), root)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	noToolsRuntime := &serveRuntime{}
	var workspaceErr *serveWorkspaceError
	if err := srv.ensureRuntimeBaseDirForSession(ctx, persisted.ID, noToolsRuntime); !errors.As(err, &workspaceErr) || workspaceErr.Code != "project_unavailable" {
		t.Fatalf("missing project root without tool manager did not fail closed: %v", err)
	}
}

func TestEvictedProjectRuntimeRecreatesExactWorkspaceBeforeResponse(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	project := &session.Project{Name: "Eviction", CanonicalDir: repo}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	managed, err := worktree.Create(ctx, repo, worktree.CreateOptions{Name: "eviction-restore"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true}) })
	now := time.Now()
	persisted := &session.Session{ID: "evicted-project-runtime", Provider: "mock", Model: "mock-model", Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	binder, _ := session.AsSessionWorkspaceBinder(store)
	if _, err := binder.BindSessionWorkspace(ctx, persisted.ID, session.SessionWorkspaceBinding{ProjectID: project.ID, CWD: managed.Dir, WorktreeDir: managed.Dir}); err != nil {
		t.Fatal(err)
	}
	user := session.NewMessage(persisted.ID, llm.UserText("before eviction"), 0)
	if err := store.AddMessage(ctx, persisted.ID, user); err != nil {
		t.Fatal(err)
	}
	assistant := session.NewMessage(persisted.ID, llm.AssistantText("durable anchor"), 1)
	if err := store.AddMessage(ctx, persisted.ID, assistant); err != nil {
		t.Fatal(err)
	}
	var manager *tools.ToolManager
	var provider *llm.MockProvider
	factoryCalls := 0
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		factoryCalls++
		provider = llm.NewMockProvider("mock").AddTextResponse("after eviction")
		toolCfg := tools.DefaultToolConfig()
		var managerErr error
		manager, managerErr = tools.NewToolManager(&toolCfg, &config.Config{})
		if managerErr != nil {
			return nil, managerErr
		}
		rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), toolMgr: manager, defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	})
	t.Cleanup(srv.sessionMgr.Close)
	previousID := fmt.Sprintf("%s%d", durableResponseMessagePrefix, assistant.ID)
	body := fmt.Sprintf(`{"input":"resume","previous_response_id":%q,"client_message_id":"msg-after-eviction","project_id":%q}`, previousID, project.ID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("evicted resume status=%d body=%s", rr.Code, rr.Body.String())
	}
	baseDir := ""
	if manager != nil {
		baseDir = manager.BaseDir()
	}
	if factoryCalls != 1 || provider == nil || len(provider.Requests) != 1 || manager == nil || !sameServePath(baseDir, managed.Dir) {
		t.Fatalf("recreated runtime calls=%d provider=%#v base=%q", factoryCalls, provider, baseDir)
	}
}

func TestFirstPartyResponseBindsProjectAndManagedWorktreeBeforeProvider(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	project := &session.Project{Name: "Git project", CanonicalDir: repo}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	managed, err := worktree.Create(ctx, repo, worktree.CreateOptions{Name: "response-binding"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true}) })

	var provider *llm.MockProvider
	var manager *tools.ToolManager
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		provider = llm.NewMockProvider("mock")
		provider.AddTextResponse("bound")
		toolCfg := tools.DefaultToolConfig()
		var managerErr error
		manager, managerErr = tools.NewToolManager(&toolCfg, &config.Config{})
		if managerErr != nil {
			return nil, managerErr
		}
		rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), toolMgr: manager, defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	})
	t.Cleanup(srv.sessionMgr.Close)

	body := fmt.Sprintf(`{"input":"hello","client_message_id":"msg-project-worktree","project_id":%q,"worktree_dir":%q}`, project.ID, managed.Dir)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("response status=%d body=%s", rr.Code, rr.Body.String())
	}
	sessionID := rr.Header().Get("x-session-id")
	persisted, err := store.Get(ctx, sessionID)
	if err != nil || persisted == nil {
		t.Fatalf("load bound session: %#v, %v", persisted, err)
	}
	if persisted.ProjectID != project.ID || !sameServePath(persisted.CWD, managed.Dir) || !sameServePath(persisted.WorktreeDir, managed.Dir) {
		t.Fatalf("persisted binding = %#v", persisted)
	}
	if manager == nil || !sameServePath(manager.BaseDir(), managed.Dir) {
		t.Fatalf("runtime base = %q, want %q", manager.BaseDir(), managed.Dir)
	}
	if provider == nil || len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %#v", provider)
	}

	externalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"external unbound"}`))
	externalReq.Header.Set("Content-Type", "application/json")
	externalRR := httptest.NewRecorder()
	srv.handleResponses(externalRR, externalReq)
	if externalRR.Code != http.StatusOK {
		t.Fatalf("header-less response status=%d body=%s", externalRR.Code, externalRR.Body.String())
	}
	externalSession, err := store.Get(ctx, externalRR.Header().Get("x-session-id"))
	if err != nil || externalSession == nil || externalSession.ProjectID != "" || externalSession.CWD != "" || externalSession.WorktreeDir != "" {
		t.Fatalf("header-less omission became project-dependent: %#v, %v", externalSession, err)
	}
	explicitWorktreeReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"input":"external worktree","worktree_dir":%q}`, managed.Dir)))
	explicitWorktreeReq.Header.Set("Content-Type", "application/json")
	explicitWorktreeRR := httptest.NewRecorder()
	srv.handleResponses(explicitWorktreeRR, explicitWorktreeReq)
	explicitWorktreeSession, err := store.Get(ctx, explicitWorktreeRR.Header().Get("x-session-id"))
	if explicitWorktreeRR.Code != http.StatusOK || err != nil || explicitWorktreeSession == nil || explicitWorktreeSession.ProjectID != "" || !sameServePath(explicitWorktreeSession.CWD, managed.Dir) || !sameServePath(explicitWorktreeSession.WorktreeDir, managed.Dir) {
		t.Fatalf("explicit third-party worktree status=%d session=%#v err=%v body=%s", explicitWorktreeRR.Code, explicitWorktreeSession, err, explicitWorktreeRR.Body.String())
	}
	explicitExternalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"input":"external project","project_id":%q}`, project.ID)))
	explicitExternalReq.Header.Set("Content-Type", "application/json")
	explicitExternalRR := httptest.NewRecorder()
	srv.handleResponses(explicitExternalRR, explicitExternalReq)
	explicitExternalSession, err := store.Get(ctx, explicitExternalRR.Header().Get("x-session-id"))
	if explicitExternalRR.Code != http.StatusOK || err != nil || explicitExternalSession == nil || explicitExternalSession.ProjectID != project.ID || !sameServePath(explicitExternalSession.CWD, repo) {
		t.Fatalf("explicit third-party project status=%d session=%#v err=%v body=%s", explicitExternalRR.Code, explicitExternalSession, err, explicitExternalRR.Body.String())
	}

	srv.bootstrapProjectID = project.ID
	legacyReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"legacy UI","client_message_id":"msg-legacy-default","use_default_workspace":true}`))
	legacyReq.Header.Set("Content-Type", "application/json")
	legacyReq.Header.Set("X-Term-LLM-UI-Version", "old")
	legacyRR := httptest.NewRecorder()
	srv.handleResponses(legacyRR, legacyReq)
	if legacyRR.Code != http.StatusOK {
		t.Fatalf("legacy default translation status=%d body=%s", legacyRR.Code, legacyRR.Body.String())
	}
	legacySession, err := store.Get(ctx, legacyRR.Header().Get("x-session-id"))
	if err != nil || legacySession == nil || legacySession.ProjectID != project.ID || !sameServePath(legacySession.CWD, repo) {
		t.Fatalf("legacy default translation binding = %#v, %v", legacySession, err)
	}
	archivedState := true
	if _, err := store.UpdateProject(ctx, project.ID, session.ProjectUpdate{Archived: &archivedState}); err != nil {
		t.Fatal(err)
	}
	archivedLegacyReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"stale archived bootstrap","client_message_id":"msg-legacy-archived","use_default_workspace":true}`))
	archivedLegacyReq.Header.Set("Content-Type", "application/json")
	archivedLegacyReq.Header.Set("X-Term-LLM-UI-Version", "old")
	archivedLegacyRR := httptest.NewRecorder()
	srv.handleResponses(archivedLegacyRR, archivedLegacyReq)
	if archivedLegacyRR.Code != http.StatusConflict || !strings.Contains(archivedLegacyRR.Body.String(), "refresh_required") {
		t.Fatalf("archived bootstrap translation status=%d body=%s", archivedLegacyRR.Code, archivedLegacyRR.Body.String())
	}
	archivedState = false
	if _, err := store.UpdateProject(ctx, project.ID, session.ProjectUpdate{Archived: &archivedState}); err != nil {
		t.Fatal(err)
	}

	srv.projectsEnabled = false
	srv.cfg.ui = true
	srv.worktreeRootOnce = sync.Once{}
	srv.worktreeRootFn = worktreeRootForTest(repo)
	disabledGitReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"disabled Git","client_message_id":"msg-disabled-git","use_default_workspace":true}`))
	disabledGitReq.Header.Set("Content-Type", "application/json")
	disabledGitReq.Header.Set("X-Term-LLM-UI-Version", "test")
	disabledGitRR := httptest.NewRecorder()
	srv.handleResponses(disabledGitRR, disabledGitReq)
	disabledGitSession, err := store.Get(ctx, disabledGitRR.Header().Get("x-session-id"))
	if disabledGitRR.Code != http.StatusOK || err != nil || disabledGitSession == nil || disabledGitSession.ProjectID != "" || !sameServePath(disabledGitSession.CWD, repo) {
		t.Fatalf("disabled Git default status=%d session=%#v err=%v body=%s", disabledGitRR.Code, disabledGitSession, err, disabledGitRR.Body.String())
	}

	srv.worktreeRootOnce = sync.Once{}
	srv.worktreeRootFn = func() (string, error) { return "", errors.New("not a Git repository") }
	disabledNonGitReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"disabled non-Git","client_message_id":"msg-disabled-nongit","use_default_workspace":true}`))
	disabledNonGitReq.Header.Set("Content-Type", "application/json")
	disabledNonGitReq.Header.Set("X-Term-LLM-UI-Version", "test")
	disabledNonGitRR := httptest.NewRecorder()
	srv.handleResponses(disabledNonGitRR, disabledNonGitReq)
	disabledNonGitSession, err := store.Get(ctx, disabledNonGitRR.Header().Get("x-session-id"))
	if disabledNonGitRR.Code != http.StatusOK || err != nil || disabledNonGitSession == nil || disabledNonGitSession.ProjectID != "" || disabledNonGitSession.CWD != "" || disabledNonGitSession.WorktreeDir != "" {
		t.Fatalf("disabled non-Git default status=%d session=%#v err=%v body=%s", disabledNonGitRR.Code, disabledNonGitSession, err, disabledNonGitRR.Body.String())
	}
}

func TestConcurrentFreshResponsesFirstBindingWinsAndLoserConflicts(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	projectA, projectB := &session.Project{Name: "A", CanonicalDir: t.TempDir()}, &session.Project{Name: "B", CanonicalDir: t.TempDir()}
	for _, project := range []*session.Project{projectA, projectB} {
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Text: "winner", Delay: 150 * time.Millisecond})
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	})
	t.Cleanup(srv.sessionMgr.Close)

	const sessionID = "concurrent-project-bind"
	makeRequest := func(projectID, messageID, model string) *http.Request {
		body := fmt.Sprintf(`{"input":"hello","client_message_id":%q,"project_id":%q,"model":%q}`, messageID, projectID, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Term-LLM-UI-Version", "test")
		req.Header.Set("session_id", sessionID)
		return req
	}
	type result struct {
		project *session.Project
		model   string
		rr      *httptest.ResponseRecorder
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i, project := range []*session.Project{projectA, projectB} {
		project, messageID, model := project, fmt.Sprintf("msg-racer-%d", i), fmt.Sprintf("model-racer-%d", i)
		go func() {
			<-start
			rr := httptest.NewRecorder()
			srv.handleResponses(rr, makeRequest(project.ID, messageID, model))
			results <- result{project: project, model: model, rr: rr}
		}()
	}
	close(start)
	first, second := <-results, <-results
	var winner, loser result
	for _, candidate := range []result{first, second} {
		switch candidate.rr.Code {
		case http.StatusOK:
			winner = candidate
		case http.StatusConflict:
			loser = candidate
		}
	}
	if winner.rr == nil || loser.rr == nil {
		t.Fatalf("concurrent statuses = %d (%s), %d (%s); want one 200 and one 409", first.rr.Code, first.rr.Body.String(), second.rr.Code, second.rr.Body.String())
	}
	if len(provider.RecordedRequests()) != 1 {
		t.Fatalf("provider requests = %d, want only winner", len(provider.RecordedRequests()))
	}
	persisted, err := store.Get(ctx, sessionID)
	if err != nil || persisted.ProjectID != winner.project.ID || persisted.Model != winner.model || !sameServePath(persisted.CWD, winner.project.CanonicalDir) {
		t.Fatalf("winning binding = %#v, winner=%s loser-body=%s err=%v", persisted, winner.project.ID, loser.rr.Body.String(), err)
	}
}

func TestResponseProjectValidationFailsBeforeRuntimeOrProvider(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	ctx := context.Background()
	factoryCalls := 0
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		factoryCalls++
		return nil, errors.New("runtime factory must not run")
	})
	t.Cleanup(srv.sessionMgr.Close)

	request := func(sessionID, projectID string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"input":"hello","client_message_id":"msg-project-validation","project_id":%q}`, projectID)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Term-LLM-UI-Version", "test")
		if sessionID != "" {
			req.Header.Set("session_id", sessionID)
		}
		rr := httptest.NewRecorder()
		srv.handleResponses(rr, req)
		return rr
	}

	if rr := request("", ""); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "project_required") {
		t.Fatalf("first-party omission status=%d body=%s", rr.Code, rr.Body.String())
	}
	legacyReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"legacy","client_message_id":"msg-stale-assets","use_default_workspace":true}`))
	legacyReq.Header.Set("Content-Type", "application/json")
	legacyReq.Header.Set("X-Term-LLM-UI-Version", "old")
	legacyRR := httptest.NewRecorder()
	srv.handleResponses(legacyRR, legacyReq)
	if legacyRR.Code != http.StatusConflict || !strings.Contains(legacyRR.Body.String(), "refresh_required") {
		t.Fatalf("unusable legacy default status=%d body=%s", legacyRR.Code, legacyRR.Body.String())
	}
	if rr := request("", "prj_missing"); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "project_not_found") {
		t.Fatalf("missing project status=%d body=%s", rr.Code, rr.Body.String())
	}

	archived := &session.Project{Name: "Archived", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, archived); err != nil {
		t.Fatal(err)
	}
	archive := true
	if _, err := store.UpdateProject(ctx, archived.ID, session.ProjectUpdate{Archived: &archive}); err != nil {
		t.Fatal(err)
	}
	if rr := request("", archived.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "project_archived") {
		t.Fatalf("archived project status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := os.Remove(archived.CanonicalDir); err != nil {
		t.Fatal(err)
	}
	if rr := request("", archived.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "project_unavailable") {
		t.Fatalf("archived unavailable project status=%d body=%s", rr.Code, rr.Body.String())
	}

	unavailableDir := filepath.Join(t.TempDir(), "unavailable")
	if err := os.Mkdir(unavailableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unavailable := &session.Project{Name: "Unavailable", CanonicalDir: unavailableDir}
	if err := store.CreateProject(ctx, unavailable); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(unavailableDir); err != nil {
		t.Fatal(err)
	}
	if rr := request("", unavailable.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "project_unavailable") {
		t.Fatalf("unavailable project status=%d body=%s", rr.Code, rr.Body.String())
	}

	rootA, rootB := t.TempDir(), t.TempDir()
	projectA, projectB := &session.Project{Name: "A", CanonicalDir: rootA}, &session.Project{Name: "B", CanonicalDir: rootB}
	for _, project := range []*session.Project{projectA, projectB} {
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	historical := &session.Session{ID: "response-historical-assignment-bypass", Provider: "mock", Model: "mock", CWD: rootA, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, historical); err != nil {
		t.Fatal(err)
	}
	if rr := request(historical.ID, projectA.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "workspace_conflict") {
		t.Fatalf("historical assignment bypass status=%d body=%s", rr.Code, rr.Body.String())
	}
	unchanged, err := store.Get(ctx, historical.ID)
	if err != nil || unchanged.ProjectID != "" {
		t.Fatalf("Responses API assigned historical session: %#v, %v", unchanged, err)
	}
	bound := &session.Session{ID: "response-project-conflict", Provider: "mock", Model: "mock", ProjectID: projectA.ID, CWD: rootA, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
	if err := store.Create(ctx, bound); err != nil {
		t.Fatal(err)
	}
	if rr := request(bound.ID, projectB.ID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "workspace_conflict") {
		t.Fatalf("conflicting project status=%d body=%s", rr.Code, rr.Body.String())
	}
	if factoryCalls != 0 {
		t.Fatalf("runtime/provider factory calls = %d, want zero", factoryCalls)
	}
}

func TestProjectSessionCursorIsBoundToItsGroup(t *testing.T) {
	srv, _ := newServeProjectTestServer(t)
	cursor := session.EncodeProjectSessionCursor(session.SessionSummary{ProjectID: "prj_a", Number: 10, CreatedAt: time.Now()})
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?project_id=prj_b&cursor="+cursor, nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_cursor") {
		t.Fatalf("cross-group cursor status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFlatSessionListingPaginatesWhenProjectsDisabled(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.projectsEnabled = false
	ctx := context.Background()
	legacy := &session.Project{Name: "Legacy", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		sess := &session.Session{ID: fmt.Sprintf("flat-%d", i), Provider: "mock", Model: "mock", Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
		if i < 2 {
			// Sessions assigned while projects were enabled still belong to
			// the flat listing once projects are disabled.
			sess.ProjectID = legacy.ID
			sess.CWD = legacy.CanonicalDir
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	page := func(target string) ([]string, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleSessions(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", target, rr.Code, rr.Body.String())
		}
		var payload struct {
			Sessions []struct {
				ID string `json:"id"`
			} `json:"sessions"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(payload.Sessions))
		for _, entry := range payload.Sessions {
			ids = append(ids, entry.ID)
		}
		return ids, payload.NextCursor
	}

	got, cursor := page("/v1/sessions?limit=2")
	if len(got) != 2 || cursor == "" {
		t.Fatalf("first page = %v cursor=%q, want 2 sessions and a cursor", got, cursor)
	}
	for steps := 0; cursor != ""; steps++ {
		if steps > 5 {
			t.Fatalf("cursor never terminated; collected %v", got)
		}
		ids, next := page("/v1/sessions?limit=2&cursor=" + cursor)
		got = append(got, ids...)
		cursor = next
	}
	if len(got) != 5 {
		t.Fatalf("paged sessions = %v, want all 5", got)
	}
	want := []string{"flat-4", "flat-3", "flat-2", "flat-1", "flat-0"}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("paged order = %v, want %v", got, want)
		}
	}

	// Legacy callers without an explicit limit keep the bounded snapshot.
	all, legacyCursor := page("/v1/sessions")
	if len(all) != 5 || legacyCursor != "" {
		t.Fatalf("legacy listing = %d sessions cursor=%q, want 5 and no cursor", len(all), legacyCursor)
	}
}

func TestSidebarAndStatusHTTPExposeBoundedProjectMetadata(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfgRef = &config.Config{Providers: map[string]config.ProviderConfig{}}
	ctx := context.Background()
	project := &session.Project{Name: "HTTP project", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		now := time.Now().Add(time.Duration(i) * time.Second)
		sess := &session.Session{ID: fmt.Sprintf("http-project-%d", i), Provider: "ChatGPT (gpt-5.6-sol, effort=low)", Model: "gpt-5.6-sol", ProjectID: project.ID, CWD: project.CanonicalDir, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	sidebarRR := httptest.NewRecorder()
	srv.handleSidebar(sidebarRR, httptest.NewRequest(http.MethodGet, "/v1/sidebar?per_project=1&include_archived_projects=1", nil))
	if sidebarRR.Code != http.StatusOK {
		t.Fatalf("sidebar status=%d body=%s", sidebarRR.Code, sidebarRR.Body.String())
	}
	var sidebarPayload struct {
		Groups []session.SidebarGroup `json:"groups"`
	}
	if err := json.Unmarshal(sidebarRR.Body.Bytes(), &sidebarPayload); err != nil || len(sidebarPayload.Groups) != 1 {
		t.Fatalf("sidebar payload = %#v, %v", sidebarPayload, err)
	}
	group := sidebarPayload.Groups[0]
	if group.Project == nil || group.Project.ID != project.ID || group.SessionCount != 3 || len(group.Sessions) != 1 || group.NextCursor == "" {
		t.Fatalf("bounded sidebar group = %#v", group)
	}
	if got := group.Sessions[0].ProviderKey; got != "chatgpt" {
		t.Fatalf("sidebar provider_key = %q, want canonical chatgpt", got)
	}

	statusRR := httptest.NewRecorder()
	srv.handleSessionsStatus(statusRR, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status endpoint=%d body=%s", statusRR.Code, statusRR.Body.String())
	}
	var statusPayload struct {
		Sessions []struct {
			ID          string `json:"id"`
			ProjectID   string `json:"project_id"`
			ProjectName string `json:"project_name"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(statusRR.Body.Bytes(), &statusPayload); err != nil || len(statusPayload.Sessions) != 3 {
		t.Fatalf("status payload = %#v, %v", statusPayload, err)
	}
	for _, entry := range statusPayload.Sessions {
		if entry.ProjectID != project.ID || entry.ProjectName != project.Name {
			t.Fatalf("status project metadata = %#v", entry)
		}
	}
}

func TestProjectScopedWorktreesUseIndependentRepositoriesAndLegacyAlias(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv, store := newServeProjectTestServer(t)
	repoA := newGitRepoForBindingTest(t)
	repoB := newGitRepoForBindingTest(t)
	projectA := &session.Project{Name: "A", CanonicalDir: repoA}
	projectB := &session.Project{Name: "B", CanonicalDir: repoB}
	for _, project := range []*session.Project{projectA, projectB} {
		if err := store.CreateProject(context.Background(), project); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		project *session.Project
		root    string
	}{{projectA, repoA}, {projectB, repoB}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/projects/"+test.project.ID+"/worktrees", nil)
		rr := httptest.NewRecorder()
		srv.handleProjectByID(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), filepath.Clean(test.root)) {
			t.Fatalf("project %s worktrees status=%d body=%s", test.project.Name, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Deprecation") != "" {
			t.Fatalf("new project route marked deprecated: %v", rr.Header())
		}
	}
	createdDirs := make(map[string]string)
	for _, project := range []*session.Project{projectA, projectB} {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/"+project.ID+"/worktrees", strings.NewReader(`{"name":"independent"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleProjectByID(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("create project %s worktree status=%d body=%s", project.Name, rr.Code, rr.Body.String())
		}
		var payload struct {
			Worktree struct {
				Dir string `json:"dir"`
			} `json:"worktree"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload.Worktree.Dir == "" {
			t.Fatalf("decode project %s worktree: %#v, %v", project.Name, payload, err)
		}
		createdDirs[project.ID] = payload.Worktree.Dir
		dir := payload.Worktree.Dir
		t.Cleanup(func() { _ = worktree.Remove(context.Background(), dir, worktree.RemoveOptions{Force: true}) })
	}
	listA := httptest.NewRecorder()
	srv.handleProjectByID(listA, httptest.NewRequest(http.MethodGet, "/v1/projects/"+projectA.ID+"/worktrees", nil))
	if !strings.Contains(listA.Body.String(), createdDirs[projectA.ID]) || strings.Contains(listA.Body.String(), createdDirs[projectB.ID]) {
		t.Fatalf("project A list crossed repository boundary: %s", listA.Body.String())
	}
	crossDiff := httptest.NewRecorder()
	srv.handleProjectByID(crossDiff, httptest.NewRequest(http.MethodGet, "/v1/projects/"+projectA.ID+"/worktrees/diff?dir="+url.QueryEscape(createdDirs[projectB.ID]), nil))
	if crossDiff.Code != http.StatusBadRequest {
		t.Fatalf("cross-project diff status=%d body=%s", crossDiff.Code, crossDiff.Body.String())
	}
	for _, mutation := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/projects/" + projectA.ID + "/worktrees/merge", `{"dir":` + mustJSONQuote(createdDirs[projectB.ID]) + `}`},
		{http.MethodPost, "/v1/projects/" + projectA.ID + "/worktrees/promote", `{"dir":` + mustJSONQuote(createdDirs[projectB.ID]) + `,"branch":"foreign"}`},
		{http.MethodDelete, "/v1/projects/" + projectA.ID + "/worktrees?dir=" + url.QueryEscape(createdDirs[projectB.ID]), ""},
	} {
		req := httptest.NewRequest(mutation.method, mutation.path, strings.NewReader(mutation.body))
		if mutation.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		srv.handleProjectByID(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("cross-project %s status=%d body=%s", mutation.path, rr.Code, rr.Body.String())
		}
	}
	archiveB := true
	if _, err := store.UpdateProject(context.Background(), projectB.ID, session.ProjectUpdate{Archived: &archiveB}); err != nil {
		t.Fatal(err)
	}
	merge := httptest.NewRecorder()
	mergeReq := httptest.NewRequest(http.MethodPost, "/v1/projects/"+projectB.ID+"/worktrees/merge", strings.NewReader(`{"dir":`+mustJSONQuote(createdDirs[projectB.ID])+`}`))
	mergeReq.Header.Set("Content-Type", "application/json")
	srv.handleProjectByID(merge, mergeReq)
	if merge.Code != http.StatusOK {
		t.Fatalf("archived project B merge status=%d body=%s", merge.Code, merge.Body.String())
	}
	srv.worktreeRootFn = worktreeRootForTest(repoA)
	legacy := httptest.NewRecorder()
	srv.handleWorktrees(legacy, httptest.NewRequest(http.MethodGet, "/v1/worktrees", nil))
	if legacy.Code != http.StatusOK || legacy.Header().Get("Deprecation") != "true" || !strings.Contains(legacy.Body.String(), filepath.Clean(repoA)) {
		t.Fatalf("legacy worktrees status=%d headers=%v body=%s", legacy.Code, legacy.Header(), legacy.Body.String())
	}
}

func TestBootstrapMatchingRulesAreConservative(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	gitRoot := newGitRepoForBindingTest(t)
	gitSub := filepath.Join(gitRoot, "nested")
	if err := os.MkdirAll(gitSub, 0o755); err != nil {
		t.Fatal(err)
	}
	otherGit := newGitRepoForBindingTest(t)
	managed, err := worktree.Create(context.Background(), gitRoot, worktree.CreateOptions{Name: "bootstrap-match"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true}) })
	gitProject := resolvedProjectPath{CanonicalDir: gitRoot, Git: true}
	for _, tc := range []struct {
		name string
		sess session.Session
		want bool
	}{
		{"git root", session.Session{CWD: gitRoot}, true},
		{"git subdirectory", session.Session{CWD: gitSub}, true},
		{"managed worktree", session.Session{CWD: managed.Dir, WorktreeDir: managed.Dir}, true},
		{"inconsistent worktree and cwd", session.Session{CWD: otherGit, WorktreeDir: managed.Dir}, false},
		{"missing managed-looking worktree", session.Session{CWD: filepath.Join(filepath.Dir(managed.Dir), "missing"), WorktreeDir: filepath.Join(filepath.Dir(managed.Dir), "missing")}, false},
		{"different repository", session.Session{CWD: otherGit}, false},
		{"empty", session.Session{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionMatchesProjectBinding(tc.sess, gitProject); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
	nonGit := t.TempDir()
	nonGitProject := resolvedProjectPath{CanonicalDir: nonGit}
	if !sessionMatchesProjectBinding(session.Session{CWD: nonGit}, nonGitProject) {
		t.Fatal("exact non-Git root did not match")
	}
	nonGitSub := filepath.Join(nonGit, "nested")
	if err := os.MkdirAll(nonGitSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if sessionMatchesProjectBinding(session.Session{CWD: nonGitSub}, nonGitProject) {
		t.Fatal("non-Git subdirectory matched an exact-root project")
	}
	if sessionMatchesProjectBinding(session.Session{CWD: nonGit, WorktreeDir: t.TempDir()}, nonGitProject) {
		t.Fatal("non-Git project accepted a worktree snapshot")
	}
}

func TestInitializeServeProjectsBootstrapsAndBackfillsOnlyOnce(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	_ = srv
	gitRoot := newGitRepoForBindingTest(t)
	gitSub := filepath.Join(gitRoot, "nested")
	if err := os.MkdirAll(gitSub, 0o755); err != nil {
		t.Fatal(err)
	}
	managed, err := worktree.Create(ctx, gitRoot, worktree.CreateOptions{Name: "bootstrap-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true}) })
	other := t.TempDir()
	createHistorical := func(id, cwd, worktreeDir string) {
		now := time.Now()
		if err := store.Create(ctx, &session.Session{ID: id, Provider: "mock", Model: "mock", CWD: cwd, WorktreeDir: worktreeDir, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete}); err != nil {
			t.Fatal(err)
		}
	}
	createHistorical("bootstrap-root", gitRoot, "")
	createHistorical("bootstrap-subdir", gitSub, "")
	createHistorical("bootstrap-managed", managed.Dir, managed.Dir)
	createHistorical("bootstrap-ambiguous", other, "")

	var warnings bytes.Buffer
	enabled, bootstrapID, err := initializeServeProjects(ctx, store, gitSub, true, false, &warnings)
	if err != nil || !enabled || bootstrapID == "" {
		t.Fatalf("initialize projects = enabled %v id %q err %v", enabled, bootstrapID, err)
	}
	for _, id := range []string{"bootstrap-root", "bootstrap-subdir", "bootstrap-managed"} {
		persisted, err := store.Get(ctx, id)
		if err != nil || persisted.ProjectID != bootstrapID {
			t.Fatalf("%s backfill = %#v, %v", id, persisted, err)
		}
	}
	ambiguous, err := store.Get(ctx, "bootstrap-ambiguous")
	if err != nil || ambiguous.ProjectID != "" {
		t.Fatalf("ambiguous backfill = %#v, %v", ambiguous, err)
	}

	createHistorical("bootstrap-late", gitRoot, "")
	enabled, repeatedID, err := initializeServeProjects(ctx, store, gitRoot, true, false, &warnings)
	if err != nil || !enabled || repeatedID != bootstrapID {
		t.Fatalf("repeated initialize = enabled %v id %q err %v", enabled, repeatedID, err)
	}
	late, err := store.Get(ctx, "bootstrap-late")
	if err != nil || late.ProjectID != "" {
		t.Fatalf("repeated startup reran one-time backfill: %#v, %v", late, err)
	}

	_, nonGitStore := newServeProjectTestServer(t)
	nonGitRoot := t.TempDir()
	nonGitSub := filepath.Join(nonGitRoot, "nested")
	if err := os.Mkdir(nonGitSub, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, historical := range []*session.Session{
		{ID: "non-git-exact", Provider: "mock", Model: "mock", CWD: nonGitRoot, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete},
		{ID: "non-git-subdir", Provider: "mock", Model: "mock", CWD: nonGitSub, CreatedAt: now, UpdatedAt: now, Status: session.StatusComplete},
	} {
		if err := nonGitStore.Create(ctx, historical); err != nil {
			t.Fatal(err)
		}
	}
	_, nonGitID, err := initializeServeProjects(ctx, nonGitStore, nonGitRoot, true, false, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	exact, _ := nonGitStore.Get(ctx, "non-git-exact")
	subdir, _ := nonGitStore.Get(ctx, "non-git-subdir")
	if exact.ProjectID != nonGitID || subdir.ProjectID != "" {
		t.Fatalf("non-Git backfill exact=%#v subdir=%#v", exact, subdir)
	}
}

func TestInitializeProjectsRootAndReadOnlyFallbackPolicy(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	var warnings bytes.Buffer
	enabled, _, err := initializeServeProjects(context.Background(), nil, root, true, false, &warnings)
	if err != nil || enabled || !strings.Contains(warnings.String(), "auto-disabled") {
		t.Fatalf("default fallback enabled=%v err=%v warning=%q", enabled, err, warnings.String())
	}
	if _, _, err := initializeServeProjects(context.Background(), nil, root, true, true, &warnings); err == nil {
		t.Fatal("strict projects unexpectedly accepted unavailable store")
	}
	_, store := newServeProjectTestServer(t)
	warnings.Reset()
	if enabled, _, err := initializeServeProjects(context.Background(), store, root, true, false, &warnings); err != nil || enabled || !strings.Contains(warnings.String(), "filesystem root") {
		t.Fatalf("root fallback enabled=%v err=%v warning=%q", enabled, err, warnings.String())
	}
	if _, _, err := initializeServeProjects(context.Background(), store, root, true, true, &warnings); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("strict root startup error = %v", err)
	}
}
