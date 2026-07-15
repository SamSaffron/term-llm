package cmd

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

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
