package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
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

func TestResolveSettings_SystemPromptPlatformAcrossSourcesAndIncludeOrder(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(tmp, "inc.md"), []byte("from-config={{platform}}"), 0644); err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(tmp, "agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "inc.md"), []byte("from-agent={{platform}}"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name               string
		platform           string
		agent              *agents.Agent
		cli                CLIFlags
		configInstructions string
		want               string
	}{
		{
			name:               "config instructions",
			platform:           agents.TemplatePlatformChat,
			configInstructions: "A {{file:inc.md}} B",
			want:               "A from-config=chat B",
		},
		{
			name:     "agent system prompt",
			platform: agents.TemplatePlatformWeb,
			agent: &agents.Agent{
				Name:         "agent",
				Source:       agents.SourceUser,
				SourcePath:   agentDir,
				SystemPrompt: "A {{file:inc.md}} B",
			},
			configInstructions: "ignored",
			want:               "A from-agent=web B",
		},
		{
			name:               "cli system message",
			platform:           agents.TemplatePlatformJobs,
			cli:                CLIFlags{SystemMessage: "A {{file:inc.md}} B"},
			configInstructions: "ignored",
			want:               "A from-config=jobs B",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			settings, err := ResolveSettingsWithPlatform(cfg, tt.agent, tt.cli, "", "", tt.configInstructions, 0, 20, tt.platform)
			if err != nil {
				t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
			}
			if settings.SystemPrompt != tt.want {
				t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, tt.want)
			}
			if strings.Contains(settings.SystemPrompt, "{{platform}}") {
				t.Fatalf("SystemPrompt still has {{platform}} token: %q", settings.SystemPrompt)
			}
		})
	}
}

func TestResolveSettings_PlatformDefaultsToUnknown(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettings(cfg, nil, CLIFlags{}, "", "", "{{platform}}", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if settings.SystemPrompt != agents.TemplatePlatformUnknown {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, agents.TemplatePlatformUnknown)
	}
}

func TestResolveSettings_UnknownTokensRemainUnchangedWhenPlatformExpands(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettingsWithPlatform(cfg, nil, CLIFlags{SystemMessage: "{{platform}} {{unknown_token}}"}, "", "", "", 0, 20, agents.TemplatePlatformChat)
	if err != nil {
		t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
	}
	if settings.SystemPrompt != "chat {{unknown_token}}" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "chat {{unknown_token}}")
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
					MaxActive:             8,
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

			skillsSetup := SetupSkills(&cfg.Skills, "", io.Discard)
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
