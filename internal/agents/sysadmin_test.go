package agents

import (
	"strings"
	"testing"
)

func TestTemplateHostVariablesLazy(t *testing.T) {
	ctx := NewTemplateContextForTemplate("hello {{!host_facts}}")
	if ctx.HostFacts != "" || ctx.HostNotes != "" {
		t.Fatalf("host values computed unexpectedly")
	}
}
func TestTemplateHostVariablesExpand(t *testing.T) {
	ctx := TemplateContext{HostFacts: "FACTS", HostNotes: "NOTES"}
	got := ExpandTemplate("{{host_facts}}\n{{host_notes}} {{unknown}}", ctx)
	if got != "FACTS\nNOTES {{unknown}}" {
		t.Fatalf("got %q", got)
	}
}
func TestBuiltinSysadmin(t *testing.T) {
	a, err := getBuiltinAgent("sysadmin")
	if err != nil {
		t.Fatal(err)
	}
	if a.Workspace != "none" || a.AgentsMd != "false" || a.MaxTurns != 300 || !a.Search {
		t.Fatalf("config=%+v", a)
	}
	if !strings.Contains(a.SystemPrompt, "{{host_facts}}") || !strings.Contains(a.SystemPrompt, "{{host_notes}}") || !strings.Contains(a.SystemPrompt, "Always use absolute paths") {
		t.Fatal("prompt missing host variables or guidance")
	}
	for _, tool := range a.Tools.Enabled {
		if tool == "spawn_agent" {
			t.Fatal("spawn_agent must not be enabled")
		}
	}
}
