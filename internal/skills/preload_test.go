package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestPreloadedInstructionsBypassDiscoveryFilters(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	dir := filepath.Join(root, ".skills", "manual")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: manual\ndescription: Manual skill\ndisable-model-invocation: true\nuser-invocable: false\ncontext: fork\n---\nFull manual instructions. Keep $ARGUMENTS and $ARGUMENTS[0]."), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "guide.md"), []byte("Resource"), 0644); err != nil {
		t.Fatal(err)
	}
	setup, err := NewSetupWithOptions(&config.SkillsConfig{
		Enabled: true, AutoInvoke: false, IncludeProjectSkills: true,
		AlwaysEnabled: []string{"pinned-only"}, NeverAuto: []string{"manual"},
		MetadataBudgetTokens: 1, MaxVisibleSkills: 1,
	}, SetupOptions{ProjectDir: root, PromptMetadataSuppressed: true, PreloadNames: []string{"manual", "manual", "missing"}})
	if err != nil || setup == nil {
		t.Fatalf("NewSetupWithOptions() = %v, %v", setup, err)
	}
	got, err := setup.PreloadedInstructions()
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing skill error = %v", err)
	}
	for _, want := range []string{"Full manual instructions.", "$ARGUMENTS and $ARGUMENTS[0]", dir, "guide.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preload missing %q: %s", want, got)
		}
	}
	if strings.Count(got, "# Skill: manual") != 1 {
		t.Fatalf("duplicate preload: %s", got)
	}
	if setup.Registry.ModelInvocationEnabled() {
		t.Fatal("preloading must not enable model invocation")
	}
	// Startup instructions remain stable even if files change later.
	if err := os.Remove(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	again, againErr := setup.PreloadedInstructions()
	if again != got || againErr.Error() != err.Error() {
		t.Fatal("preload result was not cached")
	}
}

func TestAlwaysEnabledDoesNotPreload(t *testing.T) {
	registry := testInvocationRegistry(t, map[string]string{
		"pinned": "---\nname: pinned\ndescription: Catalog only\n---\nDo not preload me.",
	})
	setup := &Setup{Registry: registry, alwaysEnabled: []string{"pinned"}}
	got, err := setup.PreloadedInstructions()
	if got != "" || err != nil {
		t.Fatalf("config always_enabled preloaded instructions: %q, %v", got, err)
	}
}

func TestPreloadCanAdmitAfterPromptOnlyRendering(t *testing.T) {
	registry := testInvocationRegistry(t, map[string]string{
		"module": "---\nname: module\ndescription: Optional module\ncode-mode:\n  module: index.mjs\n  files: [index.mjs]\n---\nLiteral $ARGUMENTS[0].",
	})
	setup := &Setup{Registry: registry, preloadNames: []string{"module", "module"}}
	promptOnly, err := setup.PreloadedInstructions()
	if err != nil || !strings.Contains(promptOnly, "not available in this prompt-only context") {
		t.Fatalf("prompt-only result = %q, %v", promptOnly, err)
	}
	// Resolution is cached, including the original body and source scope.
	skill, err := registry.Get("module")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(skill.SourcePath, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	admit := func(activation *Activation) (string, error) {
		calls++
		if activation.Prompt != "Literal $ARGUMENTS[0]." || activation.Skill.UserInstalled != skill.UserInstalled {
			t.Fatalf("resolved preload changed: %+v", activation)
		}
		return RenderActivationInstructions(activation) + "\nCapabilities admitted.", nil
	}
	admitted, err := setup.Preload(admit)
	if err != nil || !strings.Contains(admitted, "Capabilities admitted.") || strings.Contains(admitted, "prompt-only") {
		t.Fatalf("admitted result = %q, %v", admitted, err)
	}
	if _, err := setup.PreloadedInstructions(); err != nil || calls != 1 {
		t.Fatalf("prompt read admitted capabilities again: calls=%d err=%v", calls, err)
	}
	if _, err := setup.Preload(admit); err != nil || calls != 2 {
		t.Fatalf("explicit host admission was skipped: calls=%d err=%v", calls, err)
	}
	original := "Prefix\n" + promptOnly + "\nSuffix"
	refreshed := setup.RefreshPreloadedInstructions(original)
	if refreshed != "Prefix\n"+admitted+"\nSuffix" || setup.RefreshPreloadedInstructions(refreshed) != refreshed {
		t.Fatalf("refresh is not replacement/idempotent: %q", refreshed)
	}
}
