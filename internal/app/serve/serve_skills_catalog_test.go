package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func writeDraftSkillsTestSkill(t *testing.T, baseDir, name, description string) {
	t.Helper()
	dir := filepath.Join(baseDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nBody\n", name, description)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

type draftSkillsPayload struct {
	Skills []struct {
		Name                string `json:"name"`
		Description         string `json:"description"`
		Source              string `json:"source"`
		CollidesWithBuiltin bool   `json:"collides_with_builtin"`
	} `json:"skills"`
	Runs []any `json:"runs"`
}

func draftSkillsRequest(t *testing.T, srv *serveServer, method, target string, firstParty bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if firstParty {
		req.Header.Set("X-Term-LLM-UI-Version", "test")
	}
	rr := httptest.NewRecorder()
	srv.handleDraftSkills(rr, req)
	return rr
}

func decodeDraftSkills(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var payload draftSkillsPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Runs == nil {
		t.Fatalf("runs must be an empty list for a draft, body=%s", rr.Body.String())
	}
	result := make(map[string]string, len(payload.Skills))
	for _, item := range payload.Skills {
		result[item.Name] = item.Description
	}
	return result
}

func draftSkillsTestServer(t *testing.T) (*serveServer, string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	writeDraftSkillsTestSkill(t, filepath.Join(configHome, "term-llm", "skills"), "shared", "User skill")
	gitProject := filepath.Join(root, "git-project")
	startup := filepath.Join(root, "startup")
	writeDraftSkillsTestSkill(t, filepath.Join(gitProject, ".skills"), "git-only", "Git project skill")
	writeDraftSkillsTestSkill(t, filepath.Join(startup, ".skills"), "startup-only", "Startup skill")
	skillsCfg := config.SkillsConfig{
		Enabled: true, AutoInvoke: true, IncludeProjectSkills: true, IncludeEcosystemPaths: false,
	}
	srv := &serveServer{
		cfgRef:         &config.Config{Skills: skillsCfg},
		worktreeRootFn: func() (string, error) { return gitProject, nil },
		startupDir:     startup,
	}
	return srv, gitProject, startup
}

func TestDraftSkillsListsUserAndWorkspaceSkillsWithoutSession(t *testing.T) {
	srv, _, _ := draftSkillsTestServer(t)

	got := decodeDraftSkills(t, draftSkillsRequest(t, srv, http.MethodGet, "/v1/skills", true))
	if got["shared"] != "User skill" {
		t.Fatalf("user skill missing from draft listing: %#v", got)
	}
	if got["git-only"] != "Git project skill" {
		t.Fatalf("workspace skill missing from draft listing: %#v", got)
	}
	if _, ok := got["startup-only"]; ok {
		t.Fatalf("draft listing used the wrong workspace: %#v", got)
	}
}

func TestDraftSkillsUsesStartupDirectoryForNoProjectDraft(t *testing.T) {
	srv, _, _ := draftSkillsTestServer(t)
	srv.projectsEnabled = true

	got := decodeDraftSkills(t, draftSkillsRequest(t, srv, http.MethodGet, "/v1/skills?no_project=true", true))
	if got["startup-only"] != "Startup skill" || got["shared"] != "User skill" {
		t.Fatalf("no-project draft listing = %#v", got)
	}
	if _, ok := got["git-only"]; ok {
		t.Fatalf("no-project draft leaked another workspace: %#v", got)
	}

	if rr := draftSkillsRequest(t, srv, http.MethodGet, "/v1/skills", true); rr.Code != http.StatusBadRequest {
		t.Fatalf("draft without a project selection status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestDraftSkillsRejectsInvalidRequests(t *testing.T) {
	srv, _, _ := draftSkillsTestServer(t)

	if rr := draftSkillsRequest(t, srv, http.MethodPost, "/v1/skills", true); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rr.Code)
	}
	srv.projectsEnabled = true
	if rr := draftSkillsRequest(t, srv, http.MethodGet, "/v1/skills?no_project=true&project_id=p1", true); rr.Code != http.StatusBadRequest {
		t.Fatalf("conflicting project selection status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}
