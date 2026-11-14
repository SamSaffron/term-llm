package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appruntime "github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	runpkg "github.com/term-llm/term-llm/internal/run"
	"github.com/term-llm/term-llm/internal/session"
)

func servePreloadDirectories(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	agentsDir := filepath.Join(root, "agents")
	for name, selection := range map[string]string{"named": "git", "other-agent": "other", "disabled": "none", "default-agent": ""} {
		writeAgentCatalogTestAgent(t, agentsDir, name, fmt.Sprintf("name: %s\nskills: %q\ntools:\n  enabled: [read_file]\n", name, selection))
	}
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		for _, name := range []string{"git", "other"} {
			writeServePreloadManifest(t, dir, name, "", filepath.Base(dir)+" "+name+" instructions.")
		}
	}
	cfg := &config.Config{
		DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "debug"}},
		Skills: config.SkillsConfig{Enabled: true, AutoInvoke: true, IncludeProjectSkills: true, MetadataBudgetTokens: 8000, MaxVisibleSkills: 10},
	}
	cfg.Agents.SearchPaths = []string{agentsDir}
	return cfg, first, second
}

func writeServePreloadManifest(t *testing.T, dir, name, extra, body string) {
	t.Helper()
	path := filepath.Join(dir, ".skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: Selection fixture\n%s---\n%s\n", name, extra, body)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestServeAgentNamedPreloadsAtRuntimeStartup(t *testing.T) {
	cfg, first, second := servePreloadDirectories(t)
	// The startup setup is intentionally catalog-only; runtime setup must retain
	// the --agent selection rather than reconstructing it from AlwaysEnabled.
	catalogCfg := appruntime.ApplySkillsFlag(&cfg.Skills, "git")
	catalog := SetupSkillsInDir(catalogCfg, "", "", io.Discard, first)
	if body, err := catalog.PreloadedInstructions(); body != "" || err != nil {
		t.Fatalf("shared catalog owns preload instructions: %q, %v", body, err)
	}
	rt, err := newServeAgentRuntime(context.Background(), serveRuntimeRequest{RuntimeDir: first}, serveAgentRuntimeOptions{
		cfg: cfg, flags: Options{Agent: "named"}, approvalErrWriter: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.agentSkills != "git" || !strings.Contains(rt.systemPrompt, "first git instructions.") {
		t.Fatalf("agent named preload missing at startup: selection=%q prompt=%q", rt.agentSkills, rt.systemPrompt)
	}
	srv := &serveServer{cfgRef: cfg, skillsConfig: catalogCfg, skillsSetup: catalog}
	srv.configureRuntimeSkillsForDir(rt, second)
	if !strings.Contains(rt.systemPrompt, "second git instructions.") || strings.Contains(rt.systemPrompt, "first git instructions.") {
		t.Fatalf("startup selection lost on first workspace rebind: %q", rt.systemPrompt)
	}
}

func TestServeRuntimeSkillSelectionSurvivesDirectoryRebind(t *testing.T) {
	for _, tc := range []struct {
		name, agent, override, selection, body string
		catalog                                bool
	}{
		{name: "CLI named overrides agent", agent: "named", override: "other", selection: "other", body: "other"},
		{name: "CLI named enables disabled agent", agent: "disabled", override: "git", selection: "git", body: "git"},
		{name: "CLI none overrides agent", agent: "named", override: "none", selection: "none"},
		{name: "CLI all overrides named", agent: "named", override: "all", selection: "all", catalog: true},
		{name: "CLI all enables disabled agent", agent: "disabled", override: "all", selection: "all", catalog: true},
		{name: "agent other", agent: "other-agent", selection: "other", body: "other"},
		{name: "agent none", agent: "disabled", selection: "none"},
		{name: "agent default uses global", agent: "default-agent", catalog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, first, second := servePreloadDirectories(t)
			env, err := NewRunner(cfg, RunnerOptions{}).Prepare(context.Background(), runpkg.Request{
				AgentName: tc.agent, Skills: tc.override, Cwd: first, Platform: runpkg.PlatformWeb,
				DeferSession: true, ProviderInstance: mockprovider.NewMockProvider("debug"),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			if env.runtime.agentSkills != tc.selection {
				t.Fatalf("retained selection = %q, want %q", env.runtime.agentSkills, tc.selection)
			}
			// A server default must not leak its disabled auto-invoke or named
			// selection into another agent/runtime during rebinding.
			srv := &serveServer{cfgRef: cfg, skillsConfig: appruntime.ApplySkillsFlag(&cfg.Skills, "git")}
			env.runtime.mu.Lock()
			srv.configureRuntimeSkillsForDirLocked(env.runtime, second)
			env.runtime.mu.Unlock()
			prompt := env.runtime.systemPrompt
			for _, name := range []string{"git", "other"} {
				if strings.Contains(prompt, "second "+name+" instructions.") != (name == tc.body) {
					t.Fatalf("wrong selection after rebind: %q", prompt)
				}
			}
			if strings.Contains(prompt, "first git instructions.") || strings.Contains(prompt, "first other instructions.") || strings.Contains(prompt, "<available_skills>") != tc.catalog {
				t.Fatalf("stale/incorrect metadata after rebind: %q", prompt)
			}
			if body, err := env.PreloadedSkillInstructions(); err != nil || (tc.body != "" && !strings.Contains(body, "second "+tc.body+" instructions.")) {
				t.Fatalf("prepared accessor is stale: %q, %v", body, err)
			}
			setup := env.runtime.skillsSetup
			writeServePreloadManifest(t, second, "git", "", "changed after binding")
			srv.configureRuntimeSkillsForDir(env.runtime, second)
			if env.runtime.skillsSetup != setup || env.runtime.systemPrompt != prompt {
				t.Fatal("same-directory prompt refresh rescanned skills")
			}
		})
	}
}

func TestServeRuntimePreloadAdmissionIsNotSharedAcrossEngines(t *testing.T) {
	cfg, dir, _ := servePreloadDirectories(t)
	cfg.Skills.AllowProjectCodeModules = true
	writeServePreloadManifest(t, dir, "git", "code-mode:\n  module: index.mjs\n  files: [index.mjs]\n", "Runtime-local module instructions.")
	if err := os.WriteFile(filepath.Join(dir, ".skills", "git", "index.mjs"), []byte("throw new Error('not during preload');"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{cfgRef: cfg}
	catalog := srv.skillsForServeSession(&session.Session{CWD: dir})
	var runtimes []*serveRuntime
	for _, toolsFlag := range []string{"spawn_script,read_file", "read_file"} {
		eng, manager, err := newServeEngineWithTools(cfg, SessionSettings{Tools: toolsFlag, BaseDir: dir}, mockprovider.NewMockProvider("debug"), "debug", "debug", false, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		rt := &serveRuntime{engine: eng, toolMgr: manager, baseSystemPrompt: "Base prompt", serveRuntimeSkills: serveRuntimeSkills{agentSkills: "git"}}
		t.Cleanup(rt.Close)
		srv.configureRuntimeSkillsForDir(rt, dir)
		runtimes = append(runtimes, rt)
	}
	first, second := runtimes[0], runtimes[1]
	if first.skillsSetup == second.skillsSetup || first.skillsSetup == catalog || second.skillsSetup == catalog {
		t.Fatal("runtime admission renderings share a setup with another engine/catalog")
	}
	if !strings.Contains(first.systemPrompt, "Code mode module admitted") || strings.Contains(first.systemPrompt, "not available") {
		t.Fatalf("first runtime lost admitted module note: %q", first.systemPrompt)
	}
	if !strings.Contains(second.systemPrompt, "not available here because spawn_script is not enabled") || strings.Contains(second.systemPrompt, "Code mode module admitted") {
		t.Fatalf("second runtime inherited another engine's admission: %q", second.systemPrompt)
	}
	if current := InjectSkillsMetadata("Base prompt", first.skillsSetup); current != first.systemPrompt {
		t.Fatalf("second engine mutated first engine's rendering: %q", current)
	}
	if body, err := catalog.PreloadedInstructions(); err != nil || body != "" {
		t.Fatalf("runtime admission mutated shared catalog: %q, %v", body, err)
	}
}
