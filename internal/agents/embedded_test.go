package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetBuiltinAgents(t *testing.T) {
	agents := getBuiltinAgents()

	if len(agents) != len(builtinAgentNames) {
		t.Errorf("len(getBuiltinAgents()) = %d, want %d", len(agents), len(builtinAgentNames))
	}

	// Create map for easier lookup
	byName := make(map[string]*Agent)
	for _, a := range agents {
		byName[a.Name] = a
	}

	// Check each builtin exists
	for _, name := range builtinAgentNames {
		agent, ok := byName[name]
		if !ok {
			t.Errorf("missing builtin agent: %s", name)
			continue
		}
		if agent.Source != SourceBuiltin {
			t.Errorf("%s.Source = %v, want %v", name, agent.Source, SourceBuiltin)
		}
	}
}

func TestGetBuiltinAgent(t *testing.T) {
	for _, name := range builtinAgentNames {
		t.Run(name, func(t *testing.T) {
			agent, err := getBuiltinAgent(name)
			if err != nil {
				t.Fatalf("getBuiltinAgent(%s): %v", name, err)
			}

			if agent.Name != name {
				t.Errorf("Name = %q, want %q", agent.Name, name)
			}
			if agent.Source != SourceBuiltin {
				t.Errorf("Source = %v, want %v", agent.Source, SourceBuiltin)
			}
			if agent.Description == "" {
				t.Error("Description should not be empty")
			}
			if agent.SystemPrompt == "" {
				t.Error("SystemPrompt should not be empty")
			}
		})
	}

	// Non-existent builtin
	_, err := getBuiltinAgent("nonexistent")
	if err == nil {
		t.Error("getBuiltinAgent(nonexistent) should return error")
	}
}

func TestIsBuiltinAgent(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"active-review", true},
		{"agent-builder", true},
		{"artist", true},
		{"changelog", true},
		{"codebase", true},
		{"commit-message", true},
		{"contain", true},
		{"developer", true},
		{"editor", true},
		{"file-organizer", true},
		{"planner", true},
		{"web-researcher", true},
		{"reviewer", true},
		{"shell", true},
		{"nonexistent", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsBuiltinAgent(tt.name)
			if result != tt.expected {
				t.Errorf("IsBuiltinAgent(%q) = %v, want %v", tt.name, result, tt.expected)
			}
		})
	}
}

func TestBuiltinAgentConfigs(t *testing.T) {
	// Test specific builtin agent configurations
	tests := []struct {
		name               string
		expectedHasTools   bool
		expectedMaxTurns   int
		expectedHasShell   bool
		expectedAutoRun    bool
		expectedHasScripts bool
		expectedSearch     bool
	}{
		{"active-review", true, 200, false, false, false, false},
		{"agent-builder", true, 200, false, false, false, true},
		{"artist", true, 200, true, true, false, false},
		{"changelog", true, 200, true, true, false, false},
		{"codebase", true, 200, true, true, true, false},
		{"commit-message", true, 200, true, true, true, false},
		{"contain", true, 100, true, true, false, true},
		{"developer", true, 300, true, false, false, true},
		{"editor", true, 200, false, false, false, true},
		{"file-organizer", true, 200, true, true, false, false},
		{"planner", true, 50, true, false, false, true},
		{"web-researcher", true, 200, false, false, false, true},
		{"reviewer", true, 200, true, true, true, false},
		{"shell", true, 200, false, false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := getBuiltinAgent(tt.name)
			if err != nil {
				t.Fatalf("getBuiltinAgent(%s): %v", tt.name, err)
			}

			hasTools := agent.HasEnabledList() || agent.HasDisabledList()
			if hasTools != tt.expectedHasTools {
				t.Errorf("hasTools = %v, want %v", hasTools, tt.expectedHasTools)
			}

			if agent.MaxTurns != tt.expectedMaxTurns {
				t.Errorf("MaxTurns = %d, want %d", agent.MaxTurns, tt.expectedMaxTurns)
			}

			hasShell := len(agent.Shell.Allow) > 0
			if hasShell != tt.expectedHasShell {
				t.Errorf("hasShell = %v, want %v", hasShell, tt.expectedHasShell)
			}

			if agent.Shell.AutoRun != tt.expectedAutoRun {
				t.Errorf("Shell.AutoRun = %v, want %v", agent.Shell.AutoRun, tt.expectedAutoRun)
			}

			hasScripts := len(agent.Shell.Scripts) > 0
			if hasScripts != tt.expectedHasScripts {
				t.Errorf("hasScripts = %v, want %v", hasScripts, tt.expectedHasScripts)
			}

			if agent.Search != tt.expectedSearch {
				t.Errorf("Search = %v, want %v", agent.Search, tt.expectedSearch)
			}
		})
	}
}

func TestDeveloperBuiltinCanSpawnDocumentedSubagents(t *testing.T) {
	agent, err := getBuiltinAgent("developer")
	if err != nil {
		t.Fatalf("getBuiltinAgent(developer): %v", err)
	}

	if !stringSliceContains(agent.Tools.Enabled, "spawn_agent") {
		t.Fatalf("developer tools.enabled = %#v, want spawn_agent", agent.Tools.Enabled)
	}

	wantAllowed := []string{"codebase", "web-researcher", "reviewer"}
	for _, name := range wantAllowed {
		if !stringSliceContains(agent.Spawn.AllowedAgents, name) {
			t.Errorf("developer spawn.allowed_agents = %#v, missing %q", agent.Spawn.AllowedAgents, name)
		}
		if !strings.Contains(agent.SystemPrompt, "`"+name+"`") {
			t.Errorf("developer prompt should document subagent %q", name)
		}
	}
	if agent.Spawn.MaxParallel != 3 {
		t.Errorf("developer spawn.max_parallel = %d, want 3", agent.Spawn.MaxParallel)
	}
	if agent.Spawn.MaxDepth != 1 {
		t.Errorf("developer spawn.max_depth = %d, want 1", agent.Spawn.MaxDepth)
	}
	if agent.Spawn.DefaultTimeout != 600 {
		t.Errorf("developer spawn.timeout = %d, want 600", agent.Spawn.DefaultTimeout)
	}

	if !strings.Contains(agent.SystemPrompt, "`spawn_agent`") {
		t.Errorf("developer prompt should name the actual spawn_agent tool")
	}
	if strings.Contains(agent.SystemPrompt, "`run_agent`") {
		t.Errorf("developer prompt should not refer to nonexistent run_agent tool")
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExtractBuiltinResourcesContainRecipes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)

	dir, err := ExtractBuiltinResources("contain")
	if err != nil {
		t.Fatalf("ExtractBuiltinResources: %v", err)
	}

	for _, rel := range []string{
		"recipes/postgres/compose.yaml",
		"recipes/postgres/.template.yaml",
		"recipes/postgres/.env",
		"recipes/postgres/README.md",
		"recipes/redis/compose.yaml",
		"recipes/ollama/compose.yaml",
		"recipes/code-server/compose.yaml",
		"recipes/code-server/.env",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected %s extracted: %v", rel, err)
		}
	}
	for _, rel := range []string{"recipes/postgres/.env", "recipes/code-server/.env"} {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("expected %s extracted: %v", rel, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", rel, info.Mode().Perm())
		}
	}

	// agent.yaml and system.md must NOT be copied as resources.
	for _, rel := range []string{"agent.yaml", "system.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("did not expect %s in resource dir", rel)
		}
	}
}

func TestContainBuiltinRecipesUseSafeDefaults(t *testing.T) {
	tests := []struct {
		recipe       string
		wantPortLine string
	}{
		{"postgres", `"127.0.0.1:${PG_PORT:-5432}:5432"`},
		{"redis", `"127.0.0.1:${REDIS_PORT:-6379}:6379"`},
		{"ollama", `"127.0.0.1:${OLLAMA_PORT:-11434}:11434"`},
		{"code-server", `"127.0.0.1:${CODE_PORT:-8443}:8080"`},
	}
	for _, tc := range tests {
		t.Run(tc.recipe, func(t *testing.T) {
			data, err := builtinFS.ReadFile("builtin/contain/recipes/" + tc.recipe + "/compose.yaml")
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if !strings.Contains(text, tc.wantPortLine) {
				t.Fatalf("compose.yaml missing loopback port binding %s:\n%s", tc.wantPortLine, text)
			}
		})
	}
}

func TestContainBuiltinRecipeSecretsStayOutOfCompose(t *testing.T) {
	tests := []struct {
		recipe      string
		secretID    string
		envFileLine string
	}{
		{"code-server", "code_password", "CODE_PASSWORD={{code_password}}"},
		{"postgres", "pg_password", "PG_PASSWORD={{pg_password}}"},
	}
	for _, tc := range tests {
		t.Run(tc.recipe, func(t *testing.T) {
			compose, err := builtinFS.ReadFile("builtin/contain/recipes/" + tc.recipe + "/compose.yaml")
			if err != nil {
				t.Fatal(err)
			}
			composeText := string(compose)
			if strings.Contains(composeText, "{{"+tc.secretID+"}}") {
				t.Fatalf("compose.yaml renders secret placeholder %s directly:\n%s", tc.secretID, composeText)
			}
			if !strings.Contains(composeText, "env_file: .env") {
				t.Fatalf("compose.yaml missing env_file: .env:\n%s", composeText)
			}
			envData, err := builtinFS.ReadFile("builtin/contain/recipes/" + tc.recipe + "/.env")
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(envData)) != tc.envFileLine {
				t.Fatalf(".env = %q, want %q", strings.TrimSpace(string(envData)), tc.envFileLine)
			}
		})
	}
}

func TestGetBuiltinAgentNames(t *testing.T) {
	names := GetBuiltinAgentNames()

	expected := map[string]bool{
		"active-review":  true,
		"agent-builder":  true,
		"artist":         true,
		"changelog":      true,
		"codebase":       true,
		"commit-message": true,
		"contain":        true,
		"developer":      true,
		"editor":         true,
		"file-organizer": true,
		"planner":        true,
		"web-researcher": true,
		"reviewer":       true,
		"shell":          true,
	}

	if len(names) != len(expected) {
		t.Errorf("len(GetBuiltinAgentNames()) = %d, want %d", len(names), len(expected))
	}

	for _, name := range names {
		if !expected[name] {
			t.Errorf("unexpected builtin agent: %s", name)
		}
	}
}
