package serve

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestExplicitSkillsPreloadInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, flag, agent, agentsMarkup string
		configured                      []string
		wantBody, wantCatalog           bool
	}{
		{name: "named", flag: "helper", wantBody: true},
		{name: "multiple", flag: "helper,other", wantBody: true},
		{name: "plus", flag: "helper,+", wantBody: true, wantCatalog: true},
		{name: "all", flag: "all", wantCatalog: true},
		{name: "all preserves configured metadata only", flag: "all", configured: []string{"helper"}, wantCatalog: true},
		{name: "none", flag: "none", configured: []string{"helper"}},
		{name: "agent", agent: "helper", wantBody: true},
		{name: "flag overrides agent", flag: "none", agent: "helper"},
		{name: "configured metadata only", configured: []string{"helper"}, wantCatalog: true},
		{name: "agents catalog", flag: "helper", agentsMarkup: "<available_skills>managed</available_skills>", wantBody: true, wantCatalog: true},
		{name: "agents activation reference", flag: "helper", agentsMarkup: "Use activate_skill to load skills", wantBody: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			for _, name := range []string{"helper", "other"} {
				dir := filepath.Join(project, ".skills", name)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				manifest := "---\nname: " + name + "\ndescription: Test skill\n---\nUnique instructions for " + name + "."
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.agentsMarkup != "" {
				if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte(tc.agentsMarkup), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.SkillsConfig{
				Enabled: true, AutoInvoke: true, IncludeProjectSkills: true,
				AlwaysEnabled: tc.configured, MetadataBudgetTokens: 8000, MaxVisibleSkills: 8,
			}
			setup := SetupSkillsInDir(cfg, tc.flag, tc.agent, io.Discard, project)
			base := "Base instructions\n" + tc.agentsMarkup + "\n\n<insights>\nKeep this last\n</insights>"
			got := InjectSkillsMetadata(base, setup)
			if has := strings.Contains(got, "Unique instructions for helper."); has != tc.wantBody {
				t.Fatalf("helper body present = %v, want %v: %s", has, tc.wantBody, got)
			}
			if has := strings.Contains(got, "<available_skills>"); has != tc.wantCatalog {
				t.Fatalf("catalog present = %v, want %v: %s", has, tc.wantCatalog, got)
			}
			if has := strings.Contains(got, "Unique instructions for other."); has != (tc.name == "multiple") {
				t.Fatalf("unexpected other skill body: %s", got)
			}
			if !strings.HasSuffix(got, "<insights>\nKeep this last\n</insights>") {
				t.Fatalf("insights no longer at tail: %s", got)
			}
			if again := InjectSkillsMetadata(got, setup); again != got {
				t.Fatalf("repeated injection duplicated instructions: %s", again)
			}
		})
	}
}

func TestPreparedExecPreloadsReuseSkillSetup(t *testing.T) {
	for _, flag := range []string{"", "helper", "helper,+"} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			dir := filepath.Join(root, ".skills", "helper")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(path, []byte("---\nname: helper\ndescription: Example\n---\nUnique helper body."), 0644); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{
				DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}},
				Skills: config.SkillsConfig{Enabled: true, AutoInvoke: true, IncludeProjectSkills: true, AlwaysEnabled: []string{"helper"}},
			}
			env, err := NewRunner(cfg, RunnerOptions{}).Prepare(context.Background(), runpkg.Request{
				Platform: runpkg.PlatformExec, Cwd: root, Skills: flag, DeferSession: true,
				ProviderInstance: mockprovider.NewMockProvider("mock"),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			// The accessor must reuse the prepared/admitted selection, not rescan.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			got, err := env.PreloadedSkillInstructions()
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(got, "Unique helper body."); has != (flag != "") {
				t.Fatalf("flag %q returned wrong bodies: %s", flag, got)
			}
			if strings.Contains(got, "<available_skills>") {
				t.Fatalf("exec instruction accessor returned discovery catalog: %s", got)
			}
		})
	}
}
