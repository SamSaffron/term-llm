package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/skills"
	"github.com/term-llm/term-llm/internal/tools"
)

func TestRuntimeSkillContextRefreshesAdmissionNotes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	dir := filepath.Join(root, ".skills", "helper")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: helper\ndescription: Example\n---\nOriginal body."), 0644); err != nil {
		t.Fatal(err)
	}
	setup, err := skills.NewSetupWithOptions(&config.SkillsConfig{Enabled: true, IncludeProjectSkills: true}, skills.SetupOptions{ProjectDir: root, PreloadNames: []string{"helper"}})
	if err != nil || setup == nil {
		t.Fatalf("setup=%v err=%v", setup, err)
	}
	initial, err := setup.PreloadedInstructions()
	if err != nil {
		t.Fatal(err)
	}
	context := RuntimeSystemContext{Skills: setup, SystemPrompt: "Base\n\n" + initial}
	context.ApplySkills = func(_ *engine.Engine, _ *tools.ToolManager) {
		if _, err := setup.Preload(func(_ *skills.Activation) (string, error) { return "Admitted capabilities.", nil }); err != nil {
			t.Fatal(err)
		}
	}
	context.applySkills(nil, nil)
	if strings.Contains(context.SystemPrompt, "Original body.") || !strings.Contains(context.SystemPrompt, "Admitted capabilities.") {
		t.Fatalf("stale prompt after applying context: %s", context.SystemPrompt)
	}
}
