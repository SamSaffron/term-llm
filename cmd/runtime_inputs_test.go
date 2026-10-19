package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

func serveRuntimeFixtureOptions() ServeOptions {
	return ServeOptions{
		Host: "127.0.0.1", Port: 8080, AuthMode: "bearer", BasePath: "/ui",
		SessionTTL: 30 * time.Minute, SessionMax: 1000, TelegramCarryoverChars: 4000,
		JobsWorkers: 4, SidebarSessions: "all", ResponseTimeout: 30 * time.Minute,
		MaxTurns: 200, HubConnect: "direct",
	}
}

func runtimeInputsTestConfig() *config.Config {
	return &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
}

func TestSessionSettingsImageRecorderWritesMemoryOverrideOnly(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "override", "memory.db")
	defaultPath := filepath.Join(dir, "default", "memory.db")
	envPath := filepath.Join(dir, "env", "memory.db")
	outputDir := t.TempDir()
	t.Setenv("TERM_LLM_MEMORY_DB", envPath)

	cfg := &config.Config{
		Image: config.ImageConfig{Provider: "debug", OutputDir: outputDir},
		Tools: config.ToolsConfig{ImageProvider: "debug"},
	}
	settings := SessionSettings{
		Provider: "debug", Model: "debug", Tools: tools.ImageGenerateToolName,
		WriteDirs: []string{outputDir}, AgentName: "jarvis", SessionID: "session-123",
		MemoryPaths: appmemory.Paths{DBPath: overridePath, DefaultDBPath: defaultPath},
	}
	mgr, err := settings.SetupToolManager(cfg, engine.NewEngine(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if mgr == nil {
		t.Fatal("SetupToolManager returned nil")
	}
	defer mgr.ApprovalMgr.Close()
	tool, ok := mgr.Registry.Get(tools.ImageGenerateToolName)
	if !ok {
		t.Fatal("image_generate tool not registered")
	}
	showImage := false
	args, err := json.Marshal(tools.ImageGenerateArgs{Prompt: "runtime path guard", ShowImage: &showImage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("override memory DB was not written: %v", err)
	}
	store, err := memorydb.NewStore(memorydb.Config{Path: overridePath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.ListImages(context.Background(), memorydb.ImageListOptions{Agent: "jarvis", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "session-123" || rows[0].Prompt != "runtime path guard" {
		t.Fatalf("recorded images = %#v", rows)
	}
	for _, untouched := range []string{defaultPath, envPath} {
		if _, err := os.Stat(untouched); !os.IsNotExist(err) {
			t.Fatalf("non-selected memory DB %q was touched: %v", untouched, err)
		}
	}
}

func TestRunnerRestoreSettingsKeepsFreshMemoryPaths(t *testing.T) {
	fresh := appmemory.Paths{DBPath: filepath.Join(t.TempDir(), "fresh.db"), DefaultDBPath: filepath.Join(t.TempDir(), "default.db")}
	stale := appmemory.Paths{DBPath: filepath.Join(t.TempDir(), "stale.db"), DefaultDBPath: filepath.Join(t.TempDir(), "old-default.db")}
	runner := newCmdRunner(runtimeInputsTestConfig(), cmdRunnerOptions{
		MemoryPaths: fresh,
		RestoreSettings: &SessionSettings{
			MemoryPaths: stale,
		},
	}).(*cmdRunner)
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform: runpkg.PlatformWeb, ProviderInstance: mockprovider.NewMockProvider("mock"),
		Cwd: t.TempDir(), DeferSession: true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if env.settings.MemoryPaths != fresh {
		t.Fatalf("restored MemoryPaths = %#v, want fresh %#v", env.settings.MemoryPaths, fresh)
	}
}

func TestRunnerSessionStoreOverridesOnlyPersistenceFallback(t *testing.T) {
	t.Run("explicit path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "explicit", "sessions.db")
		cfg := runtimeInputsTestConfig()
		cfg.Sessions.Enabled = true
		runner := newCmdRunner(cfg, cmdRunnerOptions{
			SessionStore: appruntime.SessionStoreOverrides{DBPath: path}, ErrWriter: io.Discard,
		}).(*cmdRunner)
		env, err := runner.prepare(context.Background(), runpkg.Request{
			Platform: runpkg.PlatformJob, ProviderInstance: mockprovider.NewMockProvider("mock"),
			Cwd: t.TempDir(), Persist: true, DeferSession: true,
		}, eventSinkFunc(nil))
		if err != nil {
			t.Fatal(err)
		}
		env.Close()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("explicit session DB was not opened: %v", err)
		}
	})

	t.Run("no session", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "disabled", "sessions.db")
		cfg := runtimeInputsTestConfig()
		cfg.Sessions.Enabled = true
		runner := newCmdRunner(cfg, cmdRunnerOptions{
			SessionStore: appruntime.SessionStoreOverrides{NoSession: true, DBPath: path}, ErrWriter: io.Discard,
		}).(*cmdRunner)
		env, err := runner.prepare(context.Background(), runpkg.Request{
			Platform: runpkg.PlatformJob, ProviderInstance: mockprovider.NewMockProvider("mock"),
			Cwd: t.TempDir(), Persist: true, DeferSession: true,
		}, eventSinkFunc(nil))
		if err != nil {
			t.Fatal(err)
		}
		defer env.Close()
		if env.store != nil {
			t.Fatal("NoSession fallback created a store")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("disabled session DB was touched: %v", err)
		}
	})

	t.Run("injected store wins", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unused", "sessions.db")
		store := newServeRuntimeTestStore()
		runner := newCmdRunner(runtimeInputsTestConfig(), cmdRunnerOptions{
			Store: store, SessionStore: appruntime.SessionStoreOverrides{DBPath: path}, ErrWriter: io.Discard,
		}).(*cmdRunner)
		env, err := runner.prepare(context.Background(), runpkg.Request{
			Platform: runpkg.PlatformJob, ProviderInstance: mockprovider.NewMockProvider("mock"),
			Cwd: t.TempDir(), Persist: true, DeferSession: true,
		}, eventSinkFunc(nil))
		if err != nil {
			t.Fatal(err)
		}
		defer env.Close()
		if env.store != store {
			t.Fatal("runner replaced its injected store")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("fallback DB was touched despite injected store: %v", err)
		}
	})
}

func TestServeRuntimeRunnerDefaultsCarryRuntimeInputs(t *testing.T) {
	sessionStore := appruntime.SessionStoreOverrides{NoSession: true, DBPath: "session.db"}
	paths := appmemory.Paths{DBPath: "memory.db", DefaultDBPath: "default.db"}
	got := serveRuntimeRunnerDefaults(serveAgentRuntimeOptions{
		flags: serveRuntimeFixtureOptions(), sessionStore: sessionStore, memoryPaths: paths,
	}, serveRuntimeRequest{}, tools.ModePrompt)
	if got.SessionStore != sessionStore || got.MemoryPaths != paths {
		t.Fatalf("runtime inputs = %#v / %#v, want %#v / %#v", got.SessionStore, got.MemoryPaths, sessionStore, paths)
	}
}

func TestServeSkillChildRunnerCarriesServerRuntimeInputs(t *testing.T) {
	sessionStore := appruntime.SessionStoreOverrides{NoSession: true, DBPath: "session.db"}
	paths := appmemory.Paths{DBPath: "memory.db", DefaultDBPath: "default.db"}
	srv := &serveServer{cfgRef: &config.Config{}, sessionStore: sessionStore, memoryPaths: paths}
	child, err := srv.serveSkillChildRunner(context.Background(), "parent", &serveRuntime{providerKey: "debug", defaultModel: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	runner, ok := child.(*SpawnAgentRunner)
	if !ok {
		t.Fatalf("child runner type = %T", child)
	}
	if runner.sessionStore != sessionStore || runner.memoryPaths != paths {
		t.Fatalf("skill runtime inputs = %#v / %#v, want %#v / %#v", runner.sessionStore, runner.memoryPaths, sessionStore, paths)
	}
}

func TestResolveJobLLMSettingsCarriesMemoryPaths(t *testing.T) {
	paths := appmemory.Paths{DBPath: "job-memory.db", DefaultDBPath: "default.db"}
	settings, _, err := resolveJobLLMSettings(&config.Config{}, nil, jobsV2LLMConfig{Instructions: "run"}, jobLLMServeDefaults{MemoryPaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if settings.MemoryPaths != paths {
		t.Fatalf("job MemoryPaths = %#v, want %#v", settings.MemoryPaths, paths)
	}
}

func TestRunnerResolveSettingsCarriesMemoryPaths(t *testing.T) {
	paths := appmemory.Paths{DBPath: "memory.db", DefaultDBPath: "default.db"}
	runner := newCmdRunner(runtimeInputsTestConfig(), cmdRunnerOptions{MemoryPaths: paths}).(*cmdRunner)
	settings, err := runner.resolveSettings(runtimeInputsTestConfig(), nil, runpkg.Request{Platform: runpkg.PlatformConsole}, "")
	if err != nil {
		t.Fatal(err)
	}
	if settings.MemoryPaths != paths {
		t.Fatalf("MemoryPaths = %#v, want %#v", settings.MemoryPaths, paths)
	}
}
