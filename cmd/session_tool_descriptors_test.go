package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestAgentLegacyToolDisableUsesDescriptor(t *testing.T) {
	agent := &agents.Agent{Tools: agents.ToolsConfig{Disabled: []string{tools.ShowImageToolName}}}
	_, enabled, err := ResolveSessionPromptTools(nil, agent, CLIFlags{}, "", t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(enabled, ",")
	if slices.Contains(names, tools.ShowMediaToolName) {
		t.Fatalf("legacy disable left show_media enabled: %v", names)
	}
	if !slices.Contains(names, tools.ReadFileToolName) {
		t.Fatalf("legacy disable removed unrelated tools: %v", names)
	}
}
