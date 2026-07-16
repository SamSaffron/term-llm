package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestCmdRunnerResolveSettingsUsesRequestWorkingDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for project-instruction discovery")
	}
	workingDir := t.TempDir()
	if err := exec.Command("git", "init", "-q", workingDir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	const marker = "instructions-from-current-worktree"
	if err := os.WriteFile(filepath.Join(workingDir, "AGENTS.md"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	runner := newCmdRunner(&config.Config{}, cmdRunnerOptions{}).(*cmdRunner)
	settings, err := runner.resolveSettings(&config.Config{}, &agents.Agent{
		Name:         "reviewer",
		SystemPrompt: "review system prompt",
		AgentsMd:     "true",
	}, runpkg.Request{Platform: runpkg.PlatformConsole, Cwd: workingDir}, "")
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	if !strings.Contains(settings.SystemPrompt, marker) {
		t.Fatalf("SystemPrompt does not contain request-directory instructions %q: %q", marker, settings.SystemPrompt)
	}
	if settings.BaseDir != workingDir || settings.ShellWorkingDir != workingDir {
		t.Fatalf("BaseDir/ShellWorkingDir = %q/%q, want %q", settings.BaseDir, settings.ShellWorkingDir, workingDir)
	}
	if !sessionTestStringSliceContains(settings.ReadDirs, workingDir) || !sessionTestStringSliceContains(settings.WriteDirs, workingDir) {
		t.Fatalf("ReadDirs/WriteDirs = %#v/%#v, want request directory %q", settings.ReadDirs, settings.WriteDirs, workingDir)
	}
}

func TestCmdRunnerPrepareUsesRequestWorkingDirForSkills(t *testing.T) {
	workingDir := t.TempDir()
	skillDir := filepath.Join(workingDir, ".skills", "worktree-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: worktree-skill\ndescription: Skill from current worktree\n---\nInstructions.\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
		Skills: config.SkillsConfig{
			Enabled:              true,
			IncludeProjectSkills: true,
			MetadataBudgetTokens: 1000,
			MaxVisibleSkills:     10,
		},
	}
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		ProviderInstance: llm.NewMockProvider("mock"),
		Cwd:              workingDir,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()
	if !strings.Contains(env.settings.SystemPrompt, "worktree-skill") {
		t.Fatalf("SystemPrompt does not contain request-directory skill: %q", env.settings.SystemPrompt)
	}
}

func TestCmdRunnerPreparePropagatesWorkingDirToLLMRequest(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
	provider := llm.NewMockProvider("mock")
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	workingDir := t.TempDir()

	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		ProviderInstance: provider,
		Cwd:              workingDir,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()

	if env.llmReq.WorkingDir != workingDir {
		t.Fatalf("llm request WorkingDir = %q, want %q", env.llmReq.WorkingDir, workingDir)
	}
}

func TestCmdRunnerEnsureRunSessionUsesConfiguredBaseDir(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	runner := newCmdRunner(&config.Config{}, cmdRunnerOptions{}).(*cmdRunner)
	store := newServeRuntimeTestStore()
	workingDir := t.TempDir()

	sess := runner.ensureRunSession(
		context.Background(),
		store,
		runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "working-dir-session"},
		provider,
		"mock",
		"mock-model",
		"",
		SessionSettings{BaseDir: workingDir},
	)
	if sess == nil {
		t.Fatal("ensureRunSession returned nil")
	}
	if sess.CWD != workingDir {
		t.Fatalf("session CWD = %q, want %q", sess.CWD, workingDir)
	}
}

func TestCmdRunnerPrepareUsesBorrowedEngineProvider(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
	provider := llm.NewMockProvider("mock")
	engine := newEngine(provider, cfg)
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)

	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		Engine:           engine,
		ProviderInstance: provider,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()

	if env.engine != engine {
		t.Fatal("prepare did not reuse borrowed engine")
	}
	if env.provider != provider {
		t.Fatal("prepare did not reuse borrowed provider")
	}
	if env.runtime == nil || !env.runtime.skipProviderCleanup {
		t.Fatal("borrowed provider should skip runtime provider cleanup")
	}
}
