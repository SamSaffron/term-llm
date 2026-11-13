package tools

import (
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func cleanTestPath(path string) string { return filepath.Clean(path) }

func TestValidateCustomToolNameReservesEngineAndClientNames(t *testing.T) {
	reserved := []string{
		"web_search",
		"read_url",
		"tool_search",
		"suggest_commands",
		"webmcp__ping",
		"shell",
		"Bad-Name",
	}
	for _, name := range reserved {
		if err := validateCustomToolName(name); err == nil {
			t.Errorf("reserved name %q accepted", name)
		}
	}

	for _, name := range []string{"check_build", "webmcp", "webmcp_tool", "my_tool"} {
		if err := validateCustomToolName(name); err != nil {
			t.Errorf("ordinary name %q rejected: %v", name, err)
		}
	}
}

func TestRegisterCustomToolsRejectsReservedNames(t *testing.T) {
	for _, name := range []string{"web_search", "read_url", "tool_search", "suggest_commands", "webmcp__ping"} {
		t.Run(name, func(t *testing.T) {
			registry := &LocalToolRegistry{tools: make(map[string]core.Tool)}
			err := registry.RegisterCustomTools([]agents.CustomToolDef{{
				Name:        name,
				Description: "reserved",
				Script:      "reserved.sh",
			}}, t.TempDir())
			if err == nil {
				t.Fatalf("RegisterCustomTools(%q) accepted a reserved name", name)
			}
			if _, ok := registry.tools[name]; ok {
				t.Fatalf("reserved tool %q was installed", name)
			}
		})
	}
}

func TestCustomScriptToolProvenance(t *testing.T) {
	dir := t.TempDir()
	def := agents.CustomToolDef{Name: "check_build", Description: "build", Script: "build.sh"}
	tool := newCustomScriptTool(def, dir, DefaultOutputLimits())

	origin := tool.ToolOrigin()
	if origin.Class != core.ToolOriginCustomAgent {
		t.Fatalf("origin class = %q", origin.Class)
	}
	if origin.ID == "" || origin.ID != cleanTestPath(dir) {
		t.Fatalf("origin id = %q, want %q", origin.ID, cleanTestPath(dir))
	}
	firstBinding := tool.ToolBindingID()
	if firstBinding == "" {
		t.Fatal("empty binding id")
	}
	if got := newCustomScriptTool(def, dir, DefaultOutputLimits()).ToolBindingID(); got != firstBinding {
		t.Fatalf("same definition changed binding id: %q vs %q", got, firstBinding)
	}
	changed := def
	changed.Script = "other.sh"
	if got := newCustomScriptTool(changed, dir, DefaultOutputLimits()).ToolBindingID(); got == firstBinding {
		t.Fatal("script replacement reused binding id")
	}

	skillTool := newCustomScriptTool(def, dir, DefaultOutputLimits()).SetToolOrigin(core.ToolOrigin{Class: core.ToolOriginSkill})
	skillOrigin := skillTool.ToolOrigin()
	if skillOrigin.Class != core.ToolOriginSkill || skillOrigin.ID != "" {
		t.Fatalf("skill origin = %#v, want class-level identity", skillOrigin)
	}
	if skillTool.ToolBindingID() == firstBinding {
		t.Fatal("custom and skill provenance shared a binding id")
	}
	otherSkill := newCustomScriptTool(def, t.TempDir(), DefaultOutputLimits()).SetToolOrigin(core.ToolOrigin{Class: core.ToolOriginSkill})
	if otherSkill.ToolOrigin() != skillOrigin {
		t.Fatalf("skill origins differ across source dirs: %#v vs %#v", otherSkill.ToolOrigin(), skillOrigin)
	}
	if otherSkill.ToolBindingID() == skillTool.ToolBindingID() {
		t.Fatal("different skill source dirs shared a binding id")
	}
}
