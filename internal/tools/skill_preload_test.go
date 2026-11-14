package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	codemodetool "github.com/term-llm/term-llm/internal/codemode/tool"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/skills"
)

func preloadTestSetup(t *testing.T, user bool, manifest string, files map[string]string) (*skills.Setup, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	base := root
	if user {
		base = home
	}
	dir := filepath.Join(base, ".skills", "probe")
	files["SKILL.md"] = manifest
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	setup, err := skills.NewSetupWithOptions(&config.SkillsConfig{
		Enabled: true, AutoInvoke: false, IncludeProjectSkills: true,
	}, skills.SetupOptions{ProjectDir: root, PreloadNames: []string{"probe", "probe"}, PromptMetadataSuppressed: true})
	if err != nil || setup == nil {
		t.Fatalf("setup = %v, %v", setup, err)
	}
	return setup, dir
}

func preloadTestRuntime(t *testing.T, cfg *config.Config, enabled []string) (*engine.Engine, *ToolManager) {
	t.Helper()
	manager, err := NewToolManager(&ToolConfig{Enabled: enabled}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.ApprovalMgr.Close)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	t.Cleanup(eng.CloseCodeMode)
	manager.Registry.RegisterWithEngine(eng)
	return eng, manager
}

const preloadScriptManifest = `---
name: probe
description: Explicit capability preload
user-invocable: false
disable-model-invocation: true
context: fork
allowed-tools: []
user_installed: true
tools:
  - name: probe_script
    description: A harmless fixture
    script: scripts/probe.sh
    user_installed: true
---
Use the fixture; keep $ARGUMENTS[0] literal.
`

func TestPreloadScriptToolsUseNormalScopeWithoutChangingParentPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	for _, user := range []bool{false, true} {
		name := "project"
		if user {
			name = "user"
		}
		t.Run(name, func(t *testing.T) {
			setup, dir := preloadTestSetup(t, user, preloadScriptManifest, map[string]string{
				"scripts/probe.sh": "#!/bin/sh\nprintf ok > \"$TERM_LLM_AGENT_DIR/ran\"\necho fixture-result\n",
			})
			eng, manager := preloadTestRuntime(t, &config.Config{}, []string{ReadFileToolName, GrepToolName})
			eng.SetAllowedToolsFilter([]string{ReadFileToolName, "probe_script"})
			if err := PreloadSkillCapabilities(eng, manager, setup); err != nil {
				t.Fatal(err)
			}
			if !eng.IsToolAllowed(ReadFileToolName) || eng.IsToolAllowed(GrepToolName) {
				t.Fatal("preload narrowed or widened the parent's existing policy")
			}
			if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
				t.Fatalf("preload executed a script: %v", err)
			}
			tool, ok := eng.Tools().Get("probe_script")
			if !ok {
				t.Fatal("explicit preload did not register declared tool on engine")
			}
			if registered, ok := manager.Registry.Get("probe_script"); !ok || registered != tool {
				t.Fatal("engine did not use the normal registry tool")
			}
			instructions, err := setup.PreloadedInstructions()
			if err != nil || !strings.Contains(instructions, "$ARGUMENTS[0]") || strings.Contains(instructions, "not available") {
				t.Fatalf("preloaded instructions = %q, %v", instructions, err)
			}
			// Reading instructions again must not register/replace tools.
			if _, err := setup.PreloadedInstructions(); err != nil {
				t.Fatal(err)
			}
			if again, _ := eng.Tools().Get("probe_script"); again != tool {
				t.Fatal("prompt read re-registered capabilities")
			}
			output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(dir, "ran"))
			if user {
				if output.IsError || statErr != nil || !strings.Contains(output.Content, "fixture-result") {
					t.Fatalf("user script unavailable: %+v, %v", output, statErr)
				}
			} else if !output.IsError || !strings.Contains(output.Content, "skills.allow_project_script_tools") || !os.IsNotExist(statErr) {
				t.Fatalf("project preload bypassed scope gate: %+v, %v", output, statErr)
			}
		})
	}
}

const preloadModuleManifest = `---
name: probe
description: Module preload
allowed-tools: []
code-mode:
  module: index.mjs
  files: [index.mjs]
  requires:
    tools: [read_file]
---
Module instructions with $ARGUMENTS[0].
`

func TestPreloadCodeModuleAdmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		user         bool
		optIn        bool
		yolo         bool
		noCode       bool
		denyCode     bool
		denyRead     bool
		wantError    string
		wantAdmitted bool
	}{
		{name: "installed", user: true, wantAdmitted: true},
		{name: "project denied", wantError: "skills.allow_project_code_modules"},
		{name: "project opt-in", optIn: true, wantAdmitted: true},
		{name: "project yolo", yolo: true, wantAdmitted: true},
		{name: "code unavailable", noCode: true},
		{name: "code not permitted", user: true, denyCode: true},
		{name: "dependency not granted", user: true, denyRead: true, wantError: "requires unavailable tool read_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, _ := preloadTestSetup(t, tc.user, preloadModuleManifest, map[string]string{"index.mjs": "export const answer = 42;"})
			initial, err := setup.PreloadedInstructions()
			if err != nil || !strings.Contains(initial, "not available in this prompt-only context") || strings.Contains(initial, "module admitted") {
				t.Fatalf("prompt-only rendering = %q, %v", initial, err)
			}
			enabled := []string{ReadFileToolName, GrepToolName}
			if !tc.noCode {
				enabled = append(enabled, CodeExecToolName)
			}
			cfg := &config.Config{Skills: config.SkillsConfig{AllowProjectCodeModules: tc.optIn}}
			eng, manager := preloadTestRuntime(t, cfg, enabled)
			if tc.yolo {
				manager.ApprovalMgr.SetApprovalMode(ModeYolo)
			}
			allowed := []string{}
			if !tc.denyCode {
				allowed = append(allowed, CodeExecToolName)
			}
			if !tc.denyRead {
				allowed = append(allowed, ReadFileToolName)
			}
			eng.SetAllowedToolsFilter(allowed)
			err = PreloadSkillCapabilities(eng, manager, setup)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("admission error = %v, want %q", err, tc.wantError)
				}
				if refreshed := setup.RefreshPreloadedInstructions(initial); refreshed != "" {
					t.Fatalf("rejected instructions retained: %q", refreshed)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				instructions, err := setup.PreloadedInstructions()
				if err != nil || strings.Contains(instructions, "Code mode module admitted") != tc.wantAdmitted {
					t.Fatalf("admission instructions = %q, %v", instructions, err)
				}
				if !tc.wantAdmitted && !strings.Contains(instructions, "not available here because spawn_script is not enabled") {
					t.Fatalf("unavailable module note differs from activation: %q", instructions)
				}
				if tc.wantAdmitted {
					assertPreloadedModuleImport(t, eng, "42")
				}
			}
			if eng.IsToolAllowed(GrepToolName) || eng.IsToolAllowed(ReadFileToolName) == tc.denyRead || eng.IsToolAllowed(CodeExecToolName) == tc.denyCode {
				t.Fatal("module admission changed parent permissions")
			}
		})
	}
}

func assertPreloadedModuleImport(t *testing.T, eng *engine.Engine, want string) {
	t.Helper()
	result, err := eng.ExecCodeMode(context.Background(), `const m = await import('skill:probe'); text(m.answer);`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for attempts := 0; attempts < 10; attempts++ {
		for _, part := range result.Parts {
			output.WriteString(part.Text)
		}
		if result.Status == "completed" {
			if output.String() != want {
				t.Fatalf("module output = %q", output.String())
			}
			return
		}
		if result.Status == "failed" || result.Status == "cancelled" {
			t.Fatalf("preloaded module import failed: %+v", result)
		}
		result, err = eng.WaitCodeMode(context.Background(), result.CellID, 2*time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("module import did not finish")
}

func TestPreloadPromptOnlyCapabilitiesAreUnavailable(t *testing.T) {
	setup, _ := preloadTestSetup(t, true, strings.Replace(preloadScriptManifest, "---\nUse", "code-mode:\n  module: index.mjs\n  files: [index.mjs]\n---\nUse", 1), map[string]string{"index.mjs": "throw new Error('must not execute during preload');"})
	if err := PreloadSkillCapabilities(nil, nil, setup); err != nil {
		t.Fatal(err)
	}
	instructions, err := setup.PreloadedInstructions()
	if err != nil || !strings.Contains(instructions, "Declared script tools are not available") || !strings.Contains(instructions, "code mode module (skill:probe), which is not available") || strings.Contains(instructions, "module admitted") {
		t.Fatalf("prompt-only capabilities incorrectly claimed: %q, %v", instructions, err)
	}
}

func TestPreloadReappliesCapabilitiesForContextRollbackAndNewEngine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	manifest := strings.Replace(preloadScriptManifest, "---\nUse", "code-mode:\n  module: index.mjs\n  files: [index.mjs]\n---\nUse", 1)
	first, firstDir := preloadTestSetup(t, false, manifest, map[string]string{
		"scripts/probe.sh": "#!/bin/sh\necho first\n", "index.mjs": "export const answer = 42;",
	})
	cfg := &config.Config{Skills: config.SkillsConfig{AllowProjectCodeModules: true, AllowProjectScriptTools: true}}
	eng, manager := preloadTestRuntime(t, cfg, []string{CodeExecToolName})
	if err := PreloadSkillCapabilities(eng, manager, first); err != nil {
		t.Fatal(err)
	}
	firstPrompt, _ := first.PreloadedInstructions()
	second, _ := preloadTestSetup(t, false, manifest, map[string]string{
		"scripts/probe.sh": "#!/bin/sh\necho second\n", "index.mjs": "export const answer = 7;",
	})
	if err := PreloadSkillCapabilities(eng, manager, second); err != nil {
		t.Fatal(err)
	}
	assertPreloadedModuleImport(t, eng, "7")
	assertPreloadedScriptOutput(t, eng, "second")
	// The prior context is restored on the SAME engine, with cached resolution.
	if err := os.Remove(filepath.Join(firstDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := PreloadSkillCapabilities(eng, manager, first); err != nil {
		t.Fatal(err)
	}
	assertPreloadedModuleImport(t, eng, "42")
	assertPreloadedScriptOutput(t, eng, "first")
	otherEngine, otherManager := preloadTestRuntime(t, cfg, []string{CodeExecToolName})
	if err := PreloadSkillCapabilities(otherEngine, otherManager, first); err != nil {
		t.Fatal(err)
	}
	assertPreloadedModuleImport(t, otherEngine, "42")
	assertPreloadedScriptOutput(t, otherEngine, "first")
	// Re-admission checks current trust and replaces, rather than accumulating,
	// errors. Previously admitted prompt text must disappear on rejection.
	cfg.Skills.AllowProjectCodeModules = false
	if err := PreloadSkillCapabilities(eng, manager, first); err == nil {
		t.Fatal("context restoration ignored revoked module trust")
	}
	if refreshed := first.RefreshPreloadedInstructions(firstPrompt); refreshed != "" {
		t.Fatalf("revoked module's prompt retained: %q", refreshed)
	}
	cfg.Skills.AllowProjectCodeModules = true
	if err := PreloadSkillCapabilities(eng, manager, first); err != nil {
		t.Fatalf("successful re-admission retained old error: %v", err)
	}
	if instructions, err := first.PreloadedInstructions(); err != nil || !strings.Contains(instructions, "Code mode module admitted") {
		t.Fatalf("latest admission not rendered: %q, %v", instructions, err)
	}
}

func assertPreloadedScriptOutput(t *testing.T, eng *engine.Engine, want string) {
	t.Helper()
	tool, ok := eng.Tools().Get("probe_script")
	if !ok {
		t.Fatal("script tool not available on engine")
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || output.IsError || !strings.Contains(output.Content, "\n"+want+"\n") {
		t.Fatalf("script output = %+v, %v; want %q", output, err, want)
	}
}

func TestPreloadDoesNotExecuteModuleTopLevel(t *testing.T) {
	setup, _ := preloadTestSetup(t, true, preloadModuleManifest, map[string]string{
		"index.mjs": "throw new Error('module top-level must not run during admission');",
	})
	eng, manager := preloadTestRuntime(t, &config.Config{}, []string{CodeExecToolName, ReadFileToolName})
	if err := PreloadSkillCapabilities(eng, manager, setup); err != nil {
		t.Fatalf("preload evaluated module top-level: %v", err)
	}
}

func TestPreloadScriptWithoutToolManagerReportsUnavailable(t *testing.T) {
	setup, _ := preloadTestSetup(t, true, preloadScriptManifest, map[string]string{})
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	if err := PreloadSkillCapabilities(eng, nil, setup); err != nil {
		t.Fatal(err)
	}
	instructions, err := setup.PreloadedInstructions()
	if err != nil || !strings.Contains(instructions, "Declared script tools are not available here") {
		t.Fatalf("unavailable tools not reported: %q, %v", instructions, err)
	}
	if _, ok := eng.Tools().Get("probe_script"); ok {
		t.Fatal("script registered without manager")
	}
}

func TestPreloadModuleWithoutToolManagerDistinguishesUnavailableFromUntrusted(t *testing.T) {
	for _, emptyManager := range []bool{false, true} {
		for _, mode := range []string{"absent", "exec denied", "legacy denied", "exec enabled", "legacy enabled"} {
			name := mode
			if emptyManager {
				name += "/empty manager"
			}
			t.Run(name, func(t *testing.T) {
				setup, _ := preloadTestSetup(t, true, preloadModuleManifest, map[string]string{
					"index.mjs": "throw new Error('must not execute');",
				})
				eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
				if strings.HasPrefix(mode, "exec") {
					eng.RegisterTool(codemodetool.NewExec())
				} else if strings.HasPrefix(mode, "legacy") {
					eng.RegisterTool(codemodetool.New())
				}
				if strings.HasSuffix(mode, "denied") {
					eng.SetAllowedToolsFilter([]string{ReadFileToolName})
				}
				var manager *ToolManager
				if emptyManager {
					manager = &ToolManager{}
				}
				err := PreloadSkillCapabilities(eng, manager, setup)
				instructions, renderErr := setup.PreloadedInstructions()
				if strings.HasSuffix(mode, "enabled") {
					if err == nil || !strings.Contains(err.Error(), "require an engine and local tool manager") || renderErr == nil || instructions != "" {
						t.Fatalf("callable module without trust manager did not fail closed: %q, %v, %v", instructions, err, renderErr)
					}
				} else if err != nil || renderErr != nil || !strings.Contains(instructions, "Module instructions with $ARGUMENTS[0].") || !strings.Contains(instructions, "not available here because spawn_script is not enabled") || strings.Contains(instructions, "Code mode module admitted") {
					t.Fatalf("unavailable module dropped instructions or claimed admission: %q, %v, %v", instructions, err, renderErr)
				}
			})
		}
	}
}
