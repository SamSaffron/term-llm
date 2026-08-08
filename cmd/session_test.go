package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeSettingsDoNotGrantDaemonWorkingDirectory(t *testing.T) {
	settings, err := ResolveSettings(&config.Config{}, nil, CLIFlags{Tools: tools.ReadFileToolName, Platform: "web"}, "", "", "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if settings.PrimaryWorkspace != "" {
		t.Fatalf("serve PrimaryWorkspace = %q, want empty", settings.PrimaryWorkspace)
	}
	manager, err := settings.SetupToolManager(&config.Config{}, llm.NewEngine(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if capabilities := manager.ApprovalMgr.WorkspaceCapabilities(); len(capabilities) != 0 {
		t.Fatalf("serve inherited daemon workspace: %#v", capabilities)
	}
}

func TestPrimaryWorkspaceConfirmsOnlyActiveWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "sibling")
	for _, dir := range []string{workspace, sibling} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	workspaceFile := filepath.Join(workspace, "inside.txt")
	siblingFile := filepath.Join(sibling, "outside.txt")
	for _, path := range []string{workspaceFile, siblingFile} {
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	runtimeDir := workspace
	workspaceLink := filepath.Join(root, "workspace-link")
	if err := os.Symlink(workspace, workspaceLink); err == nil {
		runtimeDir = workspaceLink
	}

	settings, err := ResolveSettingsInDir(&config.Config{}, nil, CLIFlags{
		Tools: tools.ReadFileToolName + "," + tools.WriteFileToolName,
	}, "", "", "", 0, 20, runtimeDir)
	if err != nil {
		t.Fatalf("ResolveSettingsInDir() error = %v", err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("canonicalize workspace: %v", err)
	}
	if settings.BaseDir != canonicalWorkspace || settings.ShellWorkingDir != canonicalWorkspace {
		t.Fatalf("BaseDir/ShellWorkingDir = %q/%q, want canonical workspace %q", settings.BaseDir, settings.ShellWorkingDir, canonicalWorkspace)
	}
	if len(settings.ReadDirs) != 0 || len(settings.WriteDirs) != 0 {
		t.Fatalf("ReadDirs/WriteDirs = %#v/%#v, primary authority must not mutate static allowlists", settings.ReadDirs, settings.WriteDirs)
	}
	settings.PrimaryWorkspace = canonicalWorkspace
	if len(settings.ShellAllow) != 0 {
		t.Fatalf("ShellAllow = %#v, workspace binding must not approve shell commands", settings.ShellAllow)
	}

	toolMgr, err := settings.SetupToolManager(&config.Config{}, llm.NewEngine(nil, nil))
	if err != nil {
		t.Fatalf("SetupToolManager() error = %v", err)
	}
	toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
	workspacePrompts := 0
	toolMgr.ApprovalMgr.WorkspacePromptFunc = func(path string) (tools.WorkspaceApprovalResult, error) {
		workspacePrompts++
		if path != canonicalWorkspace {
			t.Fatalf("workspace prompt = %q, want %q", path, canonicalWorkspace)
		}
		return tools.WorkspaceApprovalResult{Approved: true}, nil
	}
	prompts := 0
	toolMgr.ApprovalMgr.PromptUIFunc = func(string, bool, bool, string) (tools.ApprovalResult, error) {
		prompts++
		return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, nil
	}

	for _, check := range []struct {
		name       string
		path       string
		tool       string
		isWrite    bool
		want       tools.ConfirmOutcome
		wantPrompt bool
	}{
		{name: "workspace read", path: workspaceFile, tool: tools.ReadFileToolName, want: tools.ProceedAlways},
		{name: "workspace write", path: filepath.Join(workspace, "new.txt"), tool: tools.WriteFileToolName, isWrite: true, want: tools.ProceedAlways},
		{name: "sibling read", path: siblingFile, tool: tools.ReadFileToolName, want: tools.Cancel, wantPrompt: true},
		{name: "sibling write", path: filepath.Join(sibling, "new.txt"), tool: tools.WriteFileToolName, isWrite: true, want: tools.Cancel, wantPrompt: true},
	} {
		t.Run(check.name, func(t *testing.T) {
			before := prompts
			outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(check.tool, check.path, check.path, check.isWrite)
			if err != nil {
				t.Fatalf("CheckPathApproval() error = %v", err)
			}
			if outcome != check.want {
				t.Fatalf("CheckPathApproval() = %v, want %v", outcome, check.want)
			}
			if got := prompts > before; got != check.wantPrompt {
				t.Fatalf("prompted = %v, want %v", got, check.wantPrompt)
			}
		})
	}
	if workspacePrompts != 1 {
		t.Fatalf("workspace prompts = %d, want 1", workspacePrompts)
	}
}

func TestResolveSettingsInDirKeepsExplicitDirsAdditiveWithoutWorkspaceDuplicates(t *testing.T) {
	workspace := t.TempDir()
	extraRead := t.TempDir()
	extraWrite := t.TempDir()
	extraReadFile := filepath.Join(extraRead, "fixture.txt")
	if err := os.WriteFile(extraReadFile, []byte("fixture"), 0o644); err != nil {
		t.Fatalf("write explicit read fixture: %v", err)
	}

	settings, err := ResolveSettingsInDir(&config.Config{}, nil, CLIFlags{
		Tools:     tools.ReadFileToolName + "," + tools.WriteFileToolName,
		ReadDirs:  []string{extraRead, workspace},
		WriteDirs: []string{extraWrite, workspace},
	}, "", "", "", 0, 20, workspace)
	if err != nil {
		t.Fatalf("ResolveSettingsInDir() error = %v", err)
	}
	if countString(settings.ReadDirs, workspace) != 1 || !sessionTestStringSliceContains(settings.ReadDirs, extraRead) {
		t.Fatalf("ReadDirs = %#v, want explicit dir plus one workspace grant", settings.ReadDirs)
	}
	if countString(settings.WriteDirs, workspace) != 1 || !sessionTestStringSliceContains(settings.WriteDirs, extraWrite) {
		t.Fatalf("WriteDirs = %#v, want explicit dir plus one workspace grant", settings.WriteDirs)
	}

	toolMgr, err := settings.SetupToolManager(&config.Config{}, llm.NewEngine(nil, nil))
	if err != nil {
		t.Fatalf("SetupToolManager() error = %v", err)
	}
	toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
	toolMgr.ApprovalMgr.PromptUIFunc = func(string, bool, bool, string) (tools.ApprovalResult, error) {
		t.Fatal("explicit directory access prompted for approval")
		return tools.ApprovalResult{}, nil
	}
	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, extraReadFile, extraReadFile, false); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("explicit read approval = %v, %v; want deterministic approval", outcome, err)
	}
	extraWriteFile := filepath.Join(extraWrite, "new.txt")
	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.WriteFileToolName, extraWriteFile, extraWriteFile, true); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("explicit write approval = %v, %v; want deterministic approval", outcome, err)
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func TestResolveSettingsEnablesPlanGuidanceOnlyForBuiltinDeveloper(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name  string
		agent *agents.Agent
		want  bool
	}{
		{name: "builtin developer", agent: &agents.Agent{Name: "developer", Source: agents.SourceBuiltin}, want: true},
		{name: "custom developer", agent: &agents.Agent{Name: "developer", Source: agents.SourceUser}, want: false},
		{name: "other builtin", agent: &agents.Agent{Name: "codebase", Source: agents.SourceBuiltin}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := ResolveSettingsInDir(cfg, tc.agent, CLIFlags{}, "", "", "", 0, 50, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if settings.PlanGuidance != tc.want {
				t.Fatalf("PlanGuidance = %v, want %v", settings.PlanGuidance, tc.want)
			}
		})
	}
}

func TestResolveSettings_ConfigSystemPromptExpandsIncludeThenTemplate(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(tmp, "inc.md"), []byte("Year={{year}}"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "Start {{file:inc.md}} End", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	if strings.Contains(settings.SystemPrompt, "{{year}}") {
		t.Fatalf("SystemPrompt still has template token: %q", settings.SystemPrompt)
	}
	if !strings.Contains(settings.SystemPrompt, "Year="+time.Now().Format("2006")) {
		t.Fatalf("SystemPrompt did not include expanded year: %q", settings.SystemPrompt)
	}
}

func TestResolveSettings_ConfigSystemPromptComputesContextAfterIncludes(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "inc.md"), []byte("Repo={{ git_repo }} Branch={{ git_branch }}"), 0644); err != nil {
		t.Fatal(err)
	}

	restore := agents.SetGitOutputRunnerForTest(func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		return []byte("feature/include\n/tmp/include-repo\n"), nil
	})
	defer restore()

	settings, err := ResolveSettings(&config.Config{}, nil, CLIFlags{}, "", "", "Start {{file:inc.md}} End", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	if want := "Start Repo=include-repo Branch=feature/include End"; settings.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, want)
	}
}

func TestResolveSettings_AgentAgentsMdWithoutSystemPromptUsesProjectInstructions(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))
	projectDir := filepath.Join(tmp, "project")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("project-only instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	settings, err := ResolveSettings(&config.Config{}, &agents.Agent{Name: "target", AgentsMd: "true"}, CLIFlags{}, "", "", "config fallback", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "project-only instructions" {
		t.Fatalf("SystemPrompt = %q, want AGENTS.md instructions only", settings.SystemPrompt)
	}
}

func TestResolveSettings_AgentAutoAgentsMdWithoutProjectInstructionsFallsBackToConfig(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))
	projectDir := filepath.Join(tmp, "project")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	agent := &agents.Agent{
		Name:  "target",
		Tools: agents.ToolsConfig{Enabled: []string{"edit_file"}},
	}
	if !agent.ShouldLoadProjectInstructions() {
		t.Fatal("test agent should request project instructions via auto agents_md")
	}

	settings, err := ResolveSettings(&config.Config{}, agent, CLIFlags{}, "", "", "config fallback", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "config fallback" {
		t.Fatalf("SystemPrompt = %q, want config fallback when no project instructions exist", settings.SystemPrompt)
	}
}

func TestResolveSettings_AgentSpawnConfigAppliesToToolManager(t *testing.T) {
	agent := &agents.Agent{
		Name: "spawn-limited",
		Tools: agents.ToolsConfig{
			Enabled: []string{tools.SpawnAgentToolName},
		},
		Spawn: agents.SpawnConfig{
			AllowedAgents:  []string{"codebase", "reviewer"},
			MaxParallel:    2,
			MaxDepth:       1,
			DefaultTimeout: 600,
		},
	}

	settings, err := ResolveSettings(&config.Config{}, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	if settings.Spawn.MaxParallel != 2 {
		t.Fatalf("settings.Spawn.MaxParallel = %d, want 2", settings.Spawn.MaxParallel)
	}
	if settings.Spawn.MaxDepth != 1 {
		t.Fatalf("settings.Spawn.MaxDepth = %d, want 1", settings.Spawn.MaxDepth)
	}
	if settings.Spawn.DefaultTimeout != 600 {
		t.Fatalf("settings.Spawn.DefaultTimeout = %d, want 600", settings.Spawn.DefaultTimeout)
	}
	if !sessionTestStringSliceContains(settings.Spawn.AllowedAgents, "codebase") || !sessionTestStringSliceContains(settings.Spawn.AllowedAgents, "reviewer") {
		t.Fatalf("settings.Spawn.AllowedAgents = %#v, want codebase and reviewer", settings.Spawn.AllowedAgents)
	}

	engine := llm.NewEngine(nil, nil)
	toolMgr, err := settings.SetupToolManager(&config.Config{}, engine)
	if err != nil {
		t.Fatalf("SetupToolManager() error = %v", err)
	}
	spawnTool := toolMgr.GetSpawnAgentTool()
	if spawnTool == nil {
		t.Fatal("spawn_agent tool was not enabled")
	}

	out, err := spawnTool.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"do work"}`))
	if err != nil {
		t.Fatalf("spawn_agent Execute() error = %v", err)
	}
	if !strings.Contains(out.Content, "not in the allowed list") {
		t.Fatalf("spawn_agent output = %q, want allowed-list denial", out.Content)
	}
}

func sessionTestStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestResolveSettings_AgentSystemPromptIncludeUsesAgentDir(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))
	other := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "parts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "parts", "p.md"), []byte("from agent dir"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}

	agent := &agents.Agent{
		Name:         "test-agent",
		Source:       agents.SourceUser,
		SourcePath:   agentDir,
		SystemPrompt: "X {{file:parts/p.md}} Y",
	}

	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	if settings.SystemPrompt != "X from agent dir Y" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "X from agent dir Y")
	}
}

func TestAgentPromptTemplateContextAndBaseDir_BuiltinWithoutExtractedResourcesSkipsExtraction(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	agent := &agents.Agent{
		Name:         "developer",
		Source:       agents.SourceBuiltin,
		SystemPrompt: "Today is {{date}}.",
	}

	templateCtx, baseDir, err := agentPromptTemplateContextAndBaseDir(agent, nil)
	if err != nil {
		t.Fatalf("agentPromptTemplateContextAndBaseDir() error = %v", err)
	}
	if templateCtx.ResourceDir != "" {
		t.Fatalf("templateCtx.ResourceDir = %q, want empty", templateCtx.ResourceDir)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if baseDir != cwd {
		t.Fatalf("baseDir = %q, want %q", baseDir, cwd)
	}

	resourceRoot, err := agents.GetBuiltinResourceDir()
	if err != nil {
		t.Fatalf("GetBuiltinResourceDir() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(resourceRoot, agent.Name)); !os.IsNotExist(err) {
		t.Fatalf("expected builtin resource extraction to be skipped, stat err = %v", err)
	}
}

func TestResolveSettings_BuiltinAgentResourceDirStillExtractsResources(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	agent := &agents.Agent{
		Name:         "artist",
		Source:       agents.SourceBuiltin,
		SystemPrompt: "Read {{ resource_dir }}/styles.md",
	}

	settings, err := ResolveSettings(&config.Config{}, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	resourceRoot, err := agents.GetBuiltinResourceDir()
	if err != nil {
		t.Fatalf("GetBuiltinResourceDir() error = %v", err)
	}
	expectedPath := filepath.Join(resourceRoot, "artist", "styles.md")
	if !strings.HasPrefix(settings.SystemPrompt, "Read "+expectedPath) {
		t.Fatalf("SystemPrompt = %q, want prefix %q", settings.SystemPrompt, "Read "+expectedPath)
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected extracted resource at %q: %v", expectedPath, err)
	}
}

func TestResolveSettings_BuiltinAgentFileIncludeStillExtractsResources(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	agent := &agents.Agent{
		Name:         "artist",
		Source:       agents.SourceBuiltin,
		SystemPrompt: "X {{ file:styles.md }} Y",
	}

	settings, err := ResolveSettings(&config.Config{}, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if !strings.Contains(settings.SystemPrompt, "# Art Styles & Techniques Reference") {
		t.Fatalf("SystemPrompt = %q, want embedded styles.md content", settings.SystemPrompt)
	}
}

func TestResolveSettings_AppendsInsightsToSystemPrompt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	t.Setenv("TERM_LLM_MEMORY_DB", dbPath)
	oldMemoryDBPath := memoryDBPath
	memoryDBPath = defaultMemoryDBPath
	t.Cleanup(func() { memoryDBPath = oldMemoryDBPath })

	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	if err := store.CreateInsight(context.Background(), &memorydb.Insight{
		Agent:          "jarvis",
		Content:        "Avoid filler.",
		CompactContent: "Avoid filler.",
		Category:       "communication-style",
		Confidence:     0.9,
	}); err != nil {
		t.Fatalf("CreateInsight() error = %v", err)
	}

	agent := &agents.Agent{
		Name:         "jarvis",
		SystemPrompt: "Base prompt",
		Memory: agents.MemoryConfig{
			InsightsExpansion: true,
			InsightsMaxTokens: 500,
		},
	}

	settings, err := ResolveSettings(&config.Config{}, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if !strings.Contains(settings.SystemPrompt, "Base prompt") {
		t.Fatalf("SystemPrompt missing base prompt: %q", settings.SystemPrompt)
	}
	if !strings.Contains(settings.SystemPrompt, "<insights>") {
		t.Fatalf("SystemPrompt missing insights block: %q", settings.SystemPrompt)
	}
	if !strings.Contains(settings.SystemPrompt, "Avoid filler.") {
		t.Fatalf("SystemPrompt missing insight content: %q", settings.SystemPrompt)
	}
}

func TestInjectSkillsMetadata_KeepsInsightsAtTail(t *testing.T) {
	instructions := "Base prompt\n\n<insights>\nBehavioral guidelines from past sessions:\n1. Avoid filler.\n</insights>"
	skillsSetup := &skills.Setup{XML: "<available_skills>demo</available_skills>"}

	got := InjectSkillsMetadata(instructions, skillsSetup)
	want := "Base prompt\n\n<available_skills>demo</available_skills>\n\n<insights>\nBehavioral guidelines from past sessions:\n1. Avoid filler.\n</insights>"
	if got != want {
		t.Fatalf("InjectSkillsMetadata() = %q, want %q", got, want)
	}
}

func TestInjectSkillsMetadata_DoesNotDuplicateExistingMetadata(t *testing.T) {
	instructions := "Base prompt\n\n<available_skills>existing</available_skills>"
	skillsSetup := &skills.Setup{XML: "<available_skills>new</available_skills>"}

	got := InjectSkillsMetadata(instructions, skillsSetup)
	if got != instructions {
		t.Fatalf("InjectSkillsMetadata() = %q, want unchanged instructions", got)
	}
}

func TestInjectSkillsMetadata_SkipsLazyDiscoveryWhenAgentsProvidesSkills(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".skills", "demo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".skills", "demo", "SKILL.md"), []byte(`---
name: demo
description: Demo skill
---
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("<available_skills>managed elsewhere</available_skills>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	setup := SetupSkills(&config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      8,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}, "", "", io.Discard)
	if setup == nil {
		t.Fatal("SetupSkills() = nil, want non-nil")
	}
	if suppressed, known := setup.PromptMetadataSuppressed(); !known || !suppressed {
		t.Fatalf("PromptMetadataSuppressed() = (%v, %v), want (true, true)", suppressed, known)
	}

	got := InjectSkillsMetadata("Base prompt", setup)
	if got != "Base prompt" {
		t.Fatalf("InjectSkillsMetadata() = %q, want unchanged instructions", got)
	}
	if setup.XML != "" {
		t.Fatalf("setup.XML = %q, want empty because AGENTS.md short-circuits metadata discovery", setup.XML)
	}
	if len(setup.Skills) != 0 {
		t.Fatalf("len(setup.Skills) = %d, want 0 because metadata discovery should be skipped", len(setup.Skills))
	}
	if setup.TotalSkills != 0 {
		t.Fatalf("setup.TotalSkills = %d, want 0 because metadata discovery should be skipped", setup.TotalSkills)
	}
}

func BenchmarkSetupSkillsWithAgentsMdSkillMarkup(b *testing.B) {
	origWD, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := b.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("<available_skills>managed elsewhere</available_skills>"), 0644); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		skillDir := filepath.Join(tmp, ".skills", fmt.Sprintf("skill-%03d", i))
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			b.Fatal(err)
		}
		body := fmt.Sprintf(`---
name: skill-%03d
description: Skill number %03d
---

# Skill %03d
`, i, i, i)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0644); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.Chdir(tmp); err != nil {
		b.Fatal(err)
	}
	b.Setenv("HOME", tmp)
	b.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	b.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	skillsCfg := &config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      50,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		setup := SetupSkills(skillsCfg, "", "", io.Discard)
		if setup == nil {
			b.Fatal("SetupSkills() = nil, want non-nil")
		}
		if got := InjectSkillsMetadata("Base prompt", setup); got != "Base prompt" {
			b.Fatalf("InjectSkillsMetadata() = %q, want unchanged prompt", got)
		}
	}
}

func TestResolveSettings_MissingIncludeIsLeftUnchanged(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "{{file:missing.md}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "{{file:missing.md}}" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "{{file:missing.md}}")
	}
}

func TestResolveSettings_ExpandsEnvToken(t *testing.T) {
	t.Setenv("TERM_LLM_SYSTEM_PROMPT_TEST", "container-name")

	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "node={{env:TERM_LLM_SYSTEM_PROMPT_TEST}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "node=container-name" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "node=container-name")
	}
}

func TestResolveSettings_ExpandsPlatformTokenWhenProvided(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{Platform: "chat"}, "", "", "surface={{platform}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "surface=chat" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "surface=chat")
	}
}

func TestResolveSettings_ExpandsLLMTokensWhenProvided(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "chatgpt", "gpt-5.4-medium", "surface={{provider}}/{{model}}/{{provider_model}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "surface=chatgpt/gpt-5.4-medium/chatgpt:gpt-5.4-medium" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "surface=chatgpt/gpt-5.4-medium/chatgpt:gpt-5.4-medium")
	}
}

func TestResolveSettings_LeavesPlatformTokenWhenPlatformUnavailable(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "surface={{platform}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "surface={{platform}}" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "surface={{platform}}")
	}
}

func TestResolveSettings_LeavesLLMTokensWhenUnavailable(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "surface={{provider}}/{{model}}/{{provider_model}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != "surface={{provider}}/{{model}}/{{provider_model}}" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "surface={{provider}}/{{model}}/{{provider_model}}")
	}
}

func TestResolveSettings_AgentToolsAppliedWhenCLIToolsUnset(t *testing.T) {
	cfg := &config.Config{}
	agent := &agents.Agent{
		Tools: agents.ToolsConfig{
			Enabled: []string{tools.ReadFileToolName, tools.ShellToolName},
		},
	}

	settings, err := ResolveSettings(cfg, agent, CLIFlags{}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.Tools != tools.ReadFileToolName+","+tools.ShellToolName {
		t.Fatalf("Tools = %q, want %q", settings.Tools, tools.ReadFileToolName+","+tools.ShellToolName)
	}
}

func TestResolveSettings_CLIToolsOverrideAgentTools(t *testing.T) {
	cfg := &config.Config{}
	agent := &agents.Agent{
		Tools: agents.ToolsConfig{
			Enabled: []string{tools.ReadFileToolName, tools.ShellToolName},
		},
	}

	settings, err := ResolveSettings(cfg, agent, CLIFlags{Tools: tools.GrepToolName}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.Tools != tools.GrepToolName {
		t.Fatalf("Tools = %q, want %q", settings.Tools, tools.GrepToolName)
	}
}

func TestSkillsEnabled_RegistersActivateSkillAndInjectsSkillList(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	// Isolate user-level skill paths so this test only sees the local fixture.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	skillDir := filepath.Join(tmp, ".skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillMD := `---
name: test-skill
description: "Skill fixture for startup-mode tests"
---

# Test Skill

Fixture content.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name               string
		agent              *agents.Agent
		cli                CLIFlags
		configInstructions string
	}{
		{
			name: "starts with agent",
			agent: &agents.Agent{
				Name:         "reviewer",
				SystemPrompt: "agent instructions",
			},
			cli:                CLIFlags{},
			configInstructions: "",
		},
		{
			name:               "starts with tools",
			agent:              nil,
			cli:                CLIFlags{Tools: tools.ReadFileToolName},
			configInstructions: "tool instructions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Skills: config.SkillsConfig{
					Enabled:               true,
					AutoInvoke:            true,
					MetadataBudgetTokens:  8000,
					MaxVisibleSkills:      8,
					IncludeProjectSkills:  true,
					IncludeEcosystemPaths: false,
				},
			}

			settings, err := ResolveSettings(cfg, tc.agent, tc.cli, "", "", tc.configInstructions, 0, 20)
			if err != nil {
				t.Fatalf("ResolveSettings() error = %v", err)
			}

			engine := newEngine(llm.NewMockProvider("mock"), cfg)
			toolMgr, err := settings.SetupToolManager(cfg, engine)
			if err != nil {
				t.Fatalf("SetupToolManager() error = %v", err)
			}

			skillsSetup := SetupSkills(&cfg.Skills, "", "", io.Discard)
			if skillsSetup == nil {
				t.Fatal("SetupSkills() = nil, want non-nil")
			}

			RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

			if _, ok := engine.Tools().Get(tools.ActivateSkillToolName); !ok {
				t.Fatalf("expected %q tool to be registered", tools.ActivateSkillToolName)
			}
			if _, ok := engine.Tools().Get(tools.SearchSkillsToolName); !ok {
				t.Fatalf("expected %q tool to be registered", tools.SearchSkillsToolName)
			}

			instructions := InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)
			if !strings.Contains(instructions, "<available_skills>") {
				t.Fatalf("instructions missing <available_skills>: %q", instructions)
			}
			if !strings.Contains(instructions, "<name>test-skill</name>") {
				t.Fatalf("instructions missing test skill entry: %q", instructions)
			}
		})
	}
}

func TestRegisterSkillToolWithEngine_AllowsSkillDeclaredToolsInAllowedToolsFilter(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	// Isolate user-level skill paths so this test only sees the local fixture.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	skillDir := filepath.Join(tmp, ".skills", "test-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "echo.sh"), []byte("#!/bin/sh\necho ok\n"), 0755); err != nil {
		t.Fatal(err)
	}

	skillMD := `---
name: test-skill
description: "Skill fixture with a bundled tool"
allowed-tools:
  - test_echo
tools:
  - name: test_echo
    description: "Echo fixture"
    script: scripts/echo.sh
---

# Test Skill
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Skills: config.SkillsConfig{
			Enabled:               true,
			AutoInvoke:            true,
			MetadataBudgetTokens:  8000,
			MaxVisibleSkills:      8,
			IncludeProjectSkills:  true,
			IncludeEcosystemPaths: false,
		},
	}

	settings, err := ResolveSettings(cfg, nil, CLIFlags{Tools: tools.ReadFileToolName}, "", "", "tool instructions", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}

	engine := newEngine(llm.NewMockProvider("mock"), cfg)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager() error = %v", err)
	}
	if toolMgr == nil {
		t.Fatal("SetupToolManager() = nil, want non-nil")
	}

	skillsSetup := SetupSkills(&cfg.Skills, "", "", io.Discard)
	if skillsSetup == nil {
		t.Fatal("SetupSkills() = nil, want non-nil")
	}

	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	activateTool, ok := engine.Tools().Get(tools.ActivateSkillToolName)
	if !ok {
		t.Fatalf("expected %q tool to be registered", tools.ActivateSkillToolName)
	}

	if _, err := activateTool.Execute(context.Background(), json.RawMessage(`{"name":"test-skill"}`)); err != nil {
		t.Fatalf("activate_skill Execute() error = %v", err)
	}

	if _, ok := engine.Tools().Get("test_echo"); !ok {
		t.Fatal("expected skill-declared tool to be registered with engine")
	}
	if !engine.IsToolAllowed("test_echo") {
		t.Fatal("expected skill-declared tool to remain allowed after activation")
	}
	if engine.IsToolAllowed(tools.ReadFileToolName) {
		t.Fatalf("expected %q to be disallowed by skill allowlist", tools.ReadFileToolName)
	}
}

func TestRegisterSkillToolWithEngineDoesNotWidenActiveToolPolicy(t *testing.T) {
	setup, _ := serveSkillTestSetup(t)
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	engine.RegisterTool(tools.NewReadFileTool(nil, tools.OutputLimits{}))
	engine.RegisterTool(tools.NewGrepTool(nil, tools.OutputLimits{}))
	RegisterSkillToolWithEngine(engine, nil, setup)
	engine.SetAllowedToolsFilter([]string{tools.ReadFileToolName, tools.ActivateSkillToolName})

	activateTool, ok := engine.Tools().Get(tools.ActivateSkillToolName)
	if !ok {
		t.Fatalf("expected %q tool to be registered", tools.ActivateSkillToolName)
	}
	if _, err := activateTool.Execute(context.Background(), json.RawMessage(`{"name":"compact"}`)); err != nil {
		t.Fatalf("activate unrestricted skill: %v", err)
	}
	if !engine.IsToolAllowed(tools.ReadFileToolName) || engine.IsToolAllowed(tools.GrepToolName) {
		t.Fatalf("omitted allowed-tools changed active policy: read=%v grep=%v", engine.IsToolAllowed(tools.ReadFileToolName), engine.IsToolAllowed(tools.GrepToolName))
	}

	if _, err := activateTool.Execute(context.Background(), json.RawMessage(`{"name":"grep-only"}`)); err != nil {
		t.Fatalf("activate restricted skill: %v", err)
	}
	if engine.IsToolAllowed(tools.ReadFileToolName) || engine.IsToolAllowed(tools.GrepToolName) {
		t.Fatalf("skill restriction widened or failed to use baseline policy: read=%v grep=%v", engine.IsToolAllowed(tools.ReadFileToolName), engine.IsToolAllowed(tools.GrepToolName))
	}
	if _, err := activateTool.Execute(context.Background(), json.RawMessage(`{"name":"read-only"}`)); err != nil {
		t.Fatalf("activate second restricted skill: %v", err)
	}
	if !engine.IsToolAllowed(tools.ReadFileToolName) || engine.IsToolAllowed(tools.GrepToolName) {
		t.Fatalf("second restriction did not reapply against baseline policy: read=%v grep=%v", engine.IsToolAllowed(tools.ReadFileToolName), engine.IsToolAllowed(tools.GrepToolName))
	}
	if _, err := activateTool.Execute(context.Background(), json.RawMessage(`{"name":"compact"}`)); err != nil {
		t.Fatalf("restore baseline with unrestricted skill: %v", err)
	}
	if !engine.IsToolAllowed(tools.ReadFileToolName) || engine.IsToolAllowed(tools.GrepToolName) {
		t.Fatalf("unrestricted skill did not restore baseline policy: read=%v grep=%v", engine.IsToolAllowed(tools.ReadFileToolName), engine.IsToolAllowed(tools.GrepToolName))
	}
}
