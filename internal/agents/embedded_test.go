package agents

import (
	"io/fs"
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
		{"widget-builder", true},
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
		{"developer", true, 2000, true, false, false, true},
		{"editor", true, 200, false, false, false, true},
		{"file-organizer", true, 200, true, true, false, false},
		{"planner", true, 300, true, false, false, true},
		{"web-researcher", true, 200, false, false, false, true},
		{"widget-builder", true, 500, true, false, false, true},
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

func TestOnlyDeveloperBuiltinOptsIntoUpdatePlan(t *testing.T) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agent, err := getBuiltinAgent(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		hasPlan := stringSliceContains(agent.Tools.Enabled, "update_plan")
		if entry.Name() == "developer" {
			if !hasPlan {
				t.Fatalf("developer update_plan opt-in missing")
			}
			if strings.Contains(agent.SystemPrompt, "update_plan") {
				t.Fatalf("developer prompt contains ungated update_plan guidance")
			}
		} else if hasPlan || strings.Contains(agent.SystemPrompt, "update_plan") {
			t.Fatalf("builtin %s unexpectedly opts into update_plan", entry.Name())
		}
	}
}

func TestBuiltinPromptsAvoidVolatileDirectoryContext(t *testing.T) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		t.Fatalf("read builtin agents: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		prompt, err := builtinFS.ReadFile("builtin/" + name + "/system.md")
		if err != nil {
			t.Errorf("read %s system prompt: %v", name, err)
			continue
		}
		text := string(prompt)
		for _, token := range []string{"{{cwd}}", "{{git_branch}}"} {
			if strings.Contains(text, token) {
				t.Errorf("%s system prompt contains volatile token %s", name, token)
			}
		}

		agent, err := getBuiltinAgent(name)
		if err != nil {
			t.Errorf("getBuiltinAgent(%s): %v", name, err)
			continue
		}
		hasShell := stringSliceContains(agent.Tools.Enabled, "shell")
		instructsPwd := strings.Contains(text, "run `pwd`")
		if instructsPwd && !hasShell && name != "agent-builder" {
			t.Errorf("shell-less %s prompt instructs agent to run pwd", name)
		}
		if instructsPwd && hasShell && len(agent.Shell.Allow) > 0 && !stringSliceContains(agent.Shell.Allow, "pwd") {
			t.Errorf("%s shell.allow = %#v, missing pwd required by its prompt", name, agent.Shell.Allow)
		}
	}

	planner, err := getBuiltinAgent("planner")
	if err != nil {
		t.Fatalf("getBuiltinAgent(planner): %v", err)
	}
	if got := strings.Count(planner.SystemPrompt, "{{handover_path}}"); got != 1 {
		t.Errorf("planner handover path count = %d, want 1", got)
	}
}

func TestBuiltinPromptsUseCapabilityAwareDirectoryGuidance(t *testing.T) {
	const shellGuidance = "Use relative paths; the working directory may change. Run `pwd` when you need its current absolute path. Do not reuse old absolute paths."
	const shellLessGuidance = "Use relative paths; the working directory may change. Do not rely on old absolute paths."

	tests := []struct {
		name     string
		guidance string
	}{
		{name: "active-review", guidance: shellLessGuidance},
		{name: "codebase", guidance: shellGuidance},
		{name: "commit-message", guidance: shellGuidance},
		{name: "developer", guidance: shellGuidance},
		{name: "editor", guidance: shellLessGuidance},
		{name: "file-organizer", guidance: shellGuidance},
		{name: "planner", guidance: shellGuidance},
		{name: "reviewer", guidance: shellGuidance},
		{name: "shell", guidance: shellGuidance},
		{name: "widget-builder", guidance: shellGuidance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := getBuiltinAgent(tt.name)
			if err != nil {
				t.Fatalf("getBuiltinAgent(%s): %v", tt.name, err)
			}
			if !strings.Contains(agent.SystemPrompt, tt.guidance) {
				t.Errorf("system prompt missing capability-aware directory guidance %q", tt.guidance)
			}
		})
	}

	contain, err := getBuiltinAgent("contain")
	if err != nil {
		t.Fatalf("getBuiltinAgent(contain): %v", err)
	}
	for _, want := range []string{"Before creating a bind mount, run `pwd`", "Never reuse an old path"} {
		if !strings.Contains(contain.SystemPrompt, want) {
			t.Errorf("contain system prompt missing %q", want)
		}
	}
}

func TestAgentBuilderDocumentsVolatileDirectoryContext(t *testing.T) {
	agent, err := getBuiltinAgent("agent-builder")
	if err != nil {
		t.Fatalf("getBuiltinAgent(agent-builder): %v", err)
	}
	for _, want := range []string{"Avoid `{{!cwd}}`, `{{!cwd_name}}`, and `{{!git_branch}}`; they can go stale", "If `shell.allow` is set, include `pwd`"} {
		if !strings.Contains(agent.SystemPrompt, want) {
			t.Errorf("agent-builder system prompt missing %q", want)
		}
	}
}

func TestWidgetBuilderBuiltinContract(t *testing.T) {
	agent, err := getBuiltinAgent("widget-builder")
	if err != nil {
		t.Fatalf("getBuiltinAgent(widget-builder): %v", err)
	}
	if err := agent.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if agent.AgentsMd != "false" {
		t.Errorf("AgentsMd = %q, want false", agent.AgentsMd)
	}
	if !agent.Search {
		t.Error("Search = false, want true")
	}
	if agent.Shell.AutoRun {
		t.Error("Shell.AutoRun = true, want false")
	}

	for _, tool := range []string{
		"read_file", "write_file", "edit_file", "glob", "grep", "shell",
		"view_image", "image_generate", "ask_user",
	} {
		if !stringSliceContains(agent.Tools.Enabled, tool) {
			t.Errorf("Tools.Enabled = %#v, missing %q", agent.Tools.Enabled, tool)
		}
	}
	for _, tool := range []string{"spawn_agent", "update_plan"} {
		if stringSliceContains(agent.Tools.Enabled, tool) {
			t.Errorf("Tools.Enabled unexpectedly contains %q", tool)
		}
	}

	for _, want := range []string{
		"$PORT",
		"$SOCKET",
		"BASE_PATH",
		"X-Forwarded-Prefix",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"node-local startup fallback",
		"127.0.0.1",
		"relative asset, link, form, and fetch URLs",
		"ten-second startup window",
		"may be stopped after being idle",
		"Authorization",
		"Cookie",
		"admin/widgets/reload",
		"admin/widgets/<mount>/stop",
		"admin/widgets/stop",
		"SIGUSR1",
		"/home/agent/.config/term-llm/widgets/",
		"~/.config/term-llm/widgets/",
	} {
		if !strings.Contains(agent.SystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

func TestReviewerBuiltinUsesGitHubPRDiffForPRReviews(t *testing.T) {
	agent, err := getBuiltinAgent("reviewer")
	if err != nil {
		t.Fatalf("getBuiltinAgent(reviewer): %v", err)
	}

	promptChecks := []string{
		"GitHub pull request review",
		"When the user writes `#353`, pass `353` to `gh`",
		"gh pr view <number-or-url>",
		"gh pr diff <number-or-url> --patch --color=never",
		"Do **not** use plain `git diff`",
		"stale local checkout",
		"unrelated branch changes",
		"git merge-base HEAD origin/<base>",
		"git diff <merge-base>...HEAD",
	}
	for _, want := range promptChecks {
		if !strings.Contains(agent.SystemPrompt, want) {
			t.Errorf("reviewer prompt missing %q", want)
		}
	}

	allowChecks := []string{
		"gh pr view *",
		"gh pr diff *",
		"git fetch *",
		"git merge-base *",
	}
	for _, want := range allowChecks {
		if !stringSliceContains(agent.Shell.Allow, want) {
			t.Errorf("reviewer shell.allow = %#v, missing %q", agent.Shell.Allow, want)
		}
	}

	branchScript := agent.Shell.Scripts["review-branch-main"]
	for _, want := range []string{"git fetch origin main --prune", "git merge-base HEAD origin/main", "git diff --no-color ${base}...HEAD"} {
		if !strings.Contains(branchScript, want) {
			t.Errorf("review-branch-main script = %q, missing %q", branchScript, want)
		}
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
	if agent.Spawn.MaxParallel != 5 {
		t.Errorf("developer spawn.max_parallel = %d, want 5", agent.Spawn.MaxParallel)
	}
	if agent.Spawn.MaxDepth != 1 {
		t.Errorf("developer spawn.max_depth = %d, want 1", agent.Spawn.MaxDepth)
	}
	if agent.Spawn.DefaultTimeout != 600 {
		t.Errorf("developer spawn.timeout = %d, want 600", agent.Spawn.DefaultTimeout)
	}
	if agent.Spawn.AgentModels["codebase"] != "fast" {
		t.Errorf("developer spawn.agent_models[codebase] = %q, want fast", agent.Spawn.AgentModels["codebase"])
	}

	if !strings.Contains(agent.SystemPrompt, "`spawn_agent`") {
		t.Errorf("developer prompt should name the actual spawn_agent tool")
	}
	if strings.Contains(agent.SystemPrompt, "`run_agent`") {
		t.Errorf("developer prompt should not refer to nonexistent run_agent tool")
	}
	if !strings.Contains(agent.SystemPrompt, "Only spawn `reviewer` when the user explicitly asks") {
		t.Errorf("developer prompt should gate reviewer spawning on explicit user request")
	}
	if !strings.Contains(agent.SystemPrompt, "do not run `reviewer` proactively") {
		t.Errorf("developer prompt should prohibit proactive reviewer spawning")
	}
	if !strings.Contains(agent.SystemPrompt, "Parallel `codebase` discovery is okay and encouraged") {
		t.Errorf("developer prompt should encourage parallel codebase discovery")
	}
}

func TestCodebaseBuiltinDoesNotForceFastModel(t *testing.T) {
	agent, err := getBuiltinAgent("codebase")
	if err != nil {
		t.Fatalf("getBuiltinAgent(codebase): %v", err)
	}
	if agent.Model != "" {
		t.Errorf("codebase model = %q, want empty so direct @codebase uses the normal active model", agent.Model)
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
		"widget-builder": true,
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
