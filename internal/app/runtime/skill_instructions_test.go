package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestInstructionOnlySkillPreloads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	dir := filepath.Join(root, "config", "term-llm", "skills", "helper")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: helper\ndescription: Example\n---\nUnique helper instructions."), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.SkillsConfig{Enabled: true, AutoInvoke: true, AlwaysEnabled: []string{"helper"}}
	for _, flag := range []string{"", "all", "*", "none", "helper", "helper,+"} {
		t.Run(flag, func(t *testing.T) {
			var warnings bytes.Buffer
			got := PreloadSkillInstructions(cfg, flag, "Base instructions", &warnings)
			wantBody := strings.HasPrefix(flag, "helper")
			if has := strings.Contains(got, "Unique helper instructions."); has != wantBody {
				t.Fatalf("body present=%v, want %v: %s", has, wantBody, got)
			}
			if strings.Contains(got, "<available_skills>") || strings.Contains(got, "search_skills") {
				t.Fatalf("instruction-only runtime advertises discovery: %s", got)
			}
			if warnings.Len() != 0 {
				t.Fatalf("unexpected warnings: %s", &warnings)
			}
			if again := PreloadSkillInstructions(cfg, flag, got, &warnings); again != got {
				t.Fatalf("repeated preload duplicated instructions: %s", again)
			}
		})
	}
}

func TestInstructionOnlyDefaultsDoNotInitializeSkills(t *testing.T) {
	// An explicit invalid ecosystem path must not matter without a named preload.
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "nonexistent"))
	for _, flag := range []string{"", "all", "none", "+"} {
		var warnings bytes.Buffer
		cfg := &config.SkillsConfig{Enabled: true, AutoInvoke: true, AlwaysEnabled: []string{"missing"}}
		if got := PreloadSkillInstructions(cfg, flag, "Base", &warnings); got != "Base" || warnings.Len() != 0 {
			t.Fatalf("flag %q initialized metadata-only skills: got=%q warnings=%s", flag, got, &warnings)
		}
	}
}
