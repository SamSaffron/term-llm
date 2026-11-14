package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/skills"
)

func TestCodeModeSearchSkillsBindsConfiguredCatalogueWithoutActivating(t *testing.T) {
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := skills.NewRegistry(skills.RegistryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager.Registry.RegisterSkillTool(catalogue)
	manager.Registry.mu.Lock()
	manager.Registry.tools[SearchSkillsToolName] = NewSearchSkillsTool(catalogue)
	manager.Registry.mu.Unlock()

	authority := captureBindingAuthority(t, manager.Registry, "skills-session", []string{SearchSkillsToolName})
	defer authority.Close()
	bound, ok := authority.Tool(SearchSkillsToolName)
	if !ok {
		t.Fatal("search_skills was not bound")
	}
	search, ok := bound.(*SearchSkillsTool)
	if !ok || search.registry != catalogue {
		t.Fatalf("search_skills did not preserve the configured catalogue: %T", bound)
	}
	// Search is not activation: activate_skill remains direct-only and absent.
	if _, ok := authority.Tool(ActivateSkillToolName); ok {
		t.Fatal("search binding implicitly admitted activate_skill")
	}

	// A locally registered search tool keeps its own catalogue rather than the
	// activation tool's fallback.
	other, err := skills.NewRegistry(skills.RegistryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager.Registry.mu.Lock()
	manager.Registry.tools[SearchSkillsToolName] = NewSearchSkillsTool(other)
	manager.Registry.mu.Unlock()
	authority2 := captureBindingAuthority(t, manager.Registry, "skills-session-2", []string{SearchSkillsToolName})
	defer authority2.Close()
	search2 := bindingTool(t, authority2, SearchSkillsToolName).(*SearchSkillsTool)
	if search2.registry != other {
		t.Fatal("locally registered search tool catalogue was not preserved")
	}
}

func TestCodeModeRunAgentScriptFreezesSourceConfig(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	for dir, value := range map[string]string{dirA: "script-a", dirB: "script-b"} {
		script := "#!/bin/sh\necho " + value + "\n"
		if err := os.WriteFile(filepath.Join(dir, "probe.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{RunAgentScriptToolName}
	cfg.AgentDir = dirA
	cfg.BaseDir = dirA
	cfg.Workspace = "none"
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	authority := captureBindingAuthority(t, manager.Registry, "script-source", []string{RunAgentScriptToolName})
	defer authority.Close()
	bound := bindingTool(t, authority, RunAgentScriptToolName).(*RunAgentScriptTool)
	if bound == manager.Registry.tools[RunAgentScriptToolName] {
		t.Fatal("run_agent_script reused the mutable live instance")
	}

	// Retarget both the live config and the live tool after admission: the cell
	// keeps the source directory and working directory it captured.
	cfg.AgentDir = dirB
	manager.Registry.mu.Lock()
	manager.Registry.tools[RunAgentScriptToolName] = NewRunAgentScriptTool(&cfg, manager.Registry.limits)
	manager.Registry.mu.Unlock()
	ctx := core.ContextWithSessionID(context.Background(), "script-source")
	out := lifecycleCall(t, bound, ctx, `{"script":"probe.sh"}`)
	if out.IsError || !strings.Contains(out.Content, "script-a") || strings.Contains(out.Content, "script-b") {
		t.Fatalf("script source was not frozen: %+v", out)
	}
}

func TestCodeModeAdmittedNameWithoutLocalToolStaysLive(t *testing.T) {
	// A remote or host-registered tool with a builtin service name must not be
	// replaced by a synthesized runner-less local tool.
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{ReadFileToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	authority := captureBindingAuthority(t, manager.Registry, "collision", []string{SpawnAgentToolName, UpdatePlanToolName, ReadFileToolName})
	defer authority.Close()
	for _, name := range []string{SpawnAgentToolName, UpdatePlanToolName} {
		if _, ok := authority.Tool(name); ok {
			t.Fatalf("authority synthesized a local %s without a registered local service", name)
		}
	}
	if _, ok := authority.Tool(ReadFileToolName); !ok {
		t.Fatal("ordinary local tool was not bound")
	}
}

func TestCodeModeAgentToolsDenyUnstartableWorkWithoutRunner(t *testing.T) {
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{SpawnAgentToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// spawn_agent is registered but the host has not wired a runner. wait/list
	// remain usable for the shared child registry; start/resume deny per tool.
	authority := captureBindingAuthority(t, manager.Registry, "runnerless", []string{SpawnAgentToolName, WaitAgentToolName, ContinueAgentToolName, CancelAgentToolName, ListAgentsToolName})
	defer authority.Close()
	for _, name := range []string{SpawnAgentToolName, ContinueAgentToolName} {
		tool, ok := authority.Tool(name)
		if !ok {
			t.Fatalf("%s missing instead of denied", name)
		}
		out := lifecycleCall(t, tool, context.Background(), `{}`)
		if !out.IsError || !strings.Contains(out.Content, codeModeBindingUnavailableCode) {
			t.Fatalf("%s did not deny unstartable work: %#v", name, out)
		}
	}
	for _, name := range []string{WaitAgentToolName, CancelAgentToolName, ListAgentsToolName} {
		if _, ok := authority.Tool(name); !ok {
			t.Fatalf("%s should observe the shared child registry", name)
		}
	}
}
