package cmd

import (
	"context"
	"encoding/json"
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

func TestResolveSettings_AgentSystemPromptIncludeUsesAgentDir(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
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
