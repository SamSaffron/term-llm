package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
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
