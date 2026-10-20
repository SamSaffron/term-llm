package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestApplyChildSkillRuntimeRegistersToolsBeforeRestricting(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.ReadFileToolName, tools.ShellToolName}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := toolMgr.SetBaseDir(root); err != nil {
		t.Fatal(err)
	}
	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	toolMgr.SetupEngine(engine)

	skill := &runpkg.SkillRunMetadata{
		Name:                "review",
		SourcePath:          root,
		AllowedToolsPresent: true,
		AllowedTools:        []string{tools.ReadFileToolName, "review_helper"},
		ToolDefs: []skills.SkillToolDef{{
			Name:        "review_helper",
			Description: "Review helper",
			Script:      "helper.sh",
		}},
	}
	if err := applyChildSkillRuntime(engine, toolMgr, skill); err != nil {
		t.Fatalf("applyChildSkillRuntime() error = %v", err)
	}
	if _, ok := engine.Tools().Get("review_helper"); !ok {
		t.Fatal("skill-declared tool was not registered on child engine")
	}
	if !engine.IsToolAllowed(tools.ReadFileToolName) || !engine.IsToolAllowed("review_helper") {
		t.Fatal("allowed child tools were not available")
	}
	if engine.IsToolAllowed(tools.ShellToolName) {
		t.Fatal("skill allowlist broadened to a disallowed child tool")
	}

	specs := filterEngineAllowedToolSpecs(engine.Tools().AllSpecs(), engine)
	for _, spec := range specs {
		if spec.Name == tools.ShellToolName {
			t.Fatal("disallowed tool remained visible in child request specs")
		}
	}
}

func TestApplyChildSkillRuntimeExplicitEmptyBlocksAllTools(t *testing.T) {
	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	engine.RegisterTool(tools.NewReadFileTool(nil, tools.OutputLimits{}))
	if err := applyChildSkillRuntime(engine, nil, &runpkg.SkillRunMetadata{Name: "no-tools", AllowedToolsPresent: true}); err != nil {
		t.Fatal(err)
	}
	if engine.IsToolAllowed(tools.ReadFileToolName) {
		t.Fatal("explicit empty isolated skill allowlist should block all tools")
	}
}

func TestAppendChildSkillSystemContextIncludesBaseAndResources(t *testing.T) {
	got := appendChildSkillSystemContext("agent prompt", &runpkg.SkillRunMetadata{
		Name:       "review",
		Source:     "project",
		SourcePath: "/repo/.skills/review",
		Resources:  []string{"references/checklist.md"},
	})
	for _, want := range []string{"agent prompt", "<isolated_skill_run>", "review", "/repo/.skills/review", "references/checklist.md", "without requesting parent conversation context"} {
		if !strings.Contains(got, want) {
			t.Fatalf("child system context missing %q: %q", want, got)
		}
	}
}
