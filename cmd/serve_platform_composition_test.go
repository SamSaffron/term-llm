package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	platform "github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/tools"
)

type compositionFakeRunner struct{}

func (*compositionFakeRunner) Run(context.Context, runpkg.Request, runpkg.EventSink) (runpkg.Result, error) {
	return runpkg.Result{}, nil
}

type compositionCleanupProvider struct {
	name     string
	cleanups atomic.Int32
}

func (p *compositionCleanupProvider) Name() string                  { return p.name }
func (*compositionCleanupProvider) Credential() string              { return "test" }
func (*compositionCleanupProvider) Capabilities() core.Capabilities { return core.Capabilities{} }
func (*compositionCleanupProvider) Stream(context.Context, core.Request) (core.Stream, error) {
	return nil, errors.New("not used")
}
func (p *compositionCleanupProvider) CleanupMCP() { p.cleanups.Add(1) }

func TestComposeServePlatformSettingsPreservesResolvedAndRawInputs(t *testing.T) {
	store := newServeRuntimeTestStore()
	owner := &runtime.AgentHostOwner{}
	fakeRunner := &compositionFakeRunner{}
	var runnerOptions cmdRunnerOptions
	resolved := SessionSettings{
		SystemPrompt: "resolved prompt", Tools: "resolved_tools", MCP: "resolved-mcp",
		MaxTurns: 41, Search: true, TimeGrounding: true,
	}
	flags := serveRuntimeFixtureOptions()
	flags.Provider, flags.Tools, flags.MCP = "raw-provider", "raw_tools", "raw-mcp"
	flags.SystemMessage, flags.MaxTurns, flags.Search = "raw prompt", 7, false
	flags.NoSearch, flags.Debug, flags.TelegramCarryoverChars = true, true, 1234
	settings := composeServePlatformSettings(servePlatformCompositionOptions{
		cfg: &config.Config{}, flags: flags, resolved: resolved, debugRaw: true,
		forceExternalSearch: true, agentName: "resolved-agent",
		platformMessages: agents.PlatformMessagesConfig{Telegram: "telegram rules"},
		store:            store, agentOwner: owner,
		approval:          resolvedApprovalMode{Mode: tools.ModeAuto, Source: approvalModeSourceCLI},
		approvalErrWriter: io.Discard,
		newRunner: func(_ *config.Config, opts cmdRunnerOptions) runpkg.Runner {
			runnerOptions = opts
			return fakeRunner
		},
		newRuntime: func(context.Context) (*serveRuntime, error) { return nil, errors.New("unused") },
	})

	if settings.SystemPrompt != resolved.SystemPrompt || settings.Tools != resolved.Tools || settings.MCP != resolved.MCP ||
		settings.MaxTurns != resolved.MaxTurns || settings.Search != resolved.Search || settings.TimeGrounding != resolved.TimeGrounding {
		t.Fatalf("platform resolved settings = %#v, resolved = %#v", settings, resolved)
	}
	if settings.Agent != "resolved-agent" || settings.Debug != flags.Debug || !settings.DebugRaw ||
		settings.IdleTimeout != flags.SessionTTL || settings.TelegramCarryoverChars != flags.TelegramCarryoverChars ||
		!settings.ForceExternalSearch || settings.PlatformMessages.Telegram != "telegram rules" {
		t.Fatalf("platform option/dependency settings = %#v", settings)
	}
	if settings.Store != store || settings.Runner != fakeRunner {
		t.Fatalf("platform identities: store=%p/%p runner=%p/%p", settings.Store, store, settings.Runner, fakeRunner)
	}
	if runnerOptions.Search != flags.Search || runnerOptions.NoSearch != flags.NoSearch || runnerOptions.Tools != flags.Tools ||
		runnerOptions.MCP != flags.MCP || runnerOptions.SystemMessage != flags.SystemMessage || runnerOptions.MaxTurns != flags.MaxTurns {
		t.Fatalf("runner raw defaults = %#v, flags = %#v", runnerOptions, flags)
	}
	if runnerOptions.Store != store || runnerOptions.AgentOwner != owner || !runnerOptions.Debug || !runnerOptions.DebugRaw {
		t.Fatalf("runner identities/options = %#v", runnerOptions)
	}
}

func TestComposeServePlatformSessionFactoryMapsAndPreservesCleanup(t *testing.T) {
	provider := &compositionCleanupProvider{name: "composition-provider"}
	eng := engine.NewEngine(provider, nil)
	factory := composeServePlatformSessionFactory(func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{engine: eng, provider: provider, defaultModel: "composition-model"}, nil
	})
	sessionRuntime, err := factory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sessionRuntime.Engine != eng || sessionRuntime.Provider != provider || sessionRuntime.ProviderName != provider.Name() || sessionRuntime.ModelName != "composition-model" {
		t.Fatalf("session runtime = %#v", sessionRuntime)
	}
	sessionRuntime.Cleanup()
	if got := provider.cleanups.Load(); got != 1 {
		t.Fatalf("successful runtime cleanups = %d, want 1", got)
	}

	wantErr := errors.New("factory failed")
	var failureCalls atomic.Int32
	failure := composeServePlatformSessionFactory(func(context.Context) (*serveRuntime, error) {
		failureCalls.Add(1)
		return nil, wantErr
	})
	if got, err := failure(context.Background()); got != nil || !errors.Is(err, wantErr) {
		t.Fatalf("failure = (%#v, %v), want nil, %v", got, err, wantErr)
	}
	if got := failureCalls.Load(); got != 1 {
		t.Fatalf("failed factory calls = %d, want 1", got)
	}
}

type compositionRetryPlatform struct {
	runs atomic.Int32
}

func (*compositionRetryPlatform) Name() string     { return "composition-retry" }
func (*compositionRetryPlatform) NeedsSetup() bool { return false }
func (*compositionRetryPlatform) RunSetup() error  { return nil }
func (p *compositionRetryPlatform) Run(_ context.Context, _ *config.Config, settings platform.Settings) error {
	p.runs.Add(1)
	settings.Ready()
	settings.Ready()
	return errors.New("retry")
}

func TestRunPlatformSupervisorInjectedBackoffReadyAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testPlatform := &compositionRetryPlatform{}
	var ready atomic.Int32
	var delays []time.Duration
	runPlatformSupervisor(ctx, &config.Config{}, platform.Settings{Ready: func() { ready.Add(1) }}, testPlatform, time.Second,
		func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			if len(delays) == 3 {
				cancel()
				return false
			}
			return true
		})
	if got := testPlatform.runs.Load(); got != 3 {
		t.Fatalf("platform runs = %d, want 3", got)
	}
	if got := ready.Load(); got != 9 {
		t.Fatalf("supervisor Ready forwarding calls = %d, want baseline 9", got)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
	if ctx.Err() == nil {
		t.Fatal("injected delay did not drive cancellation")
	}
}

type compositionRootResult struct {
	settings platform.Settings
	runtime  *platform.SessionRuntime
	jobs     core.ToolOutput
	err      error
}

type compositionRootPlatform struct {
	result chan<- compositionRootResult
	cwd    string
}

func (*compositionRootPlatform) Name() string     { return "telegram" }
func (*compositionRootPlatform) NeedsSetup() bool { return false }
func (*compositionRootPlatform) RunSetup() error  { return nil }
func (p *compositionRootPlatform) Run(ctx context.Context, _ *config.Config, settings platform.Settings) error {
	settings.Ready()
	settings.Ready()
	runtime, err := settings.NewSession(ctx)
	result := compositionRootResult{settings: settings, runtime: runtime, err: err}
	if err == nil {
		for _, name := range []string{tools.QueueAgentToolName, tools.WaitForJobsToolName, tools.LiveSettingsToolName} {
			if _, ok := runtime.Engine.Tools().Get(name); !ok {
				result.err = errors.New("production runtime omitted tool " + name)
				break
			}
		}
	}
	if result.err == nil {
		queue, _ := runtime.Engine.Tools().Get(tools.QueueAgentToolName)
		payload, _ := json.Marshal(map[string]any{"agent_name": "developer", "prompt": "composition probe", "cwd": p.cwd})
		result.jobs, result.err = queue.Execute(ctx, payload)
	}
	p.result <- result
	<-ctx.Done()
	return ctx.Err()
}

func TestRunServeWithProductionPlatformComposition(t *testing.T) {
	root := t.TempDir()
	for key, path := range map[string]string{
		"HOME": filepath.Join(root, "home"), "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
	} {
		t.Setenv(key, path)
	}
	t.Setenv("TERM_LLM_OFFLINE", "1")
	t.Setenv("TERM_LLM_JOBS_SERVER", "http://127.0.0.1:1/unwired")
	t.Setenv("TERM_LLM_JOBS_TOKEN", "wrong-unwired-token")
	configDir := filepath.Join(root, "config", "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf(`default_provider: debug
providers:
  debug:
    model: debug
skills:
  enabled: false
sessions:
  enabled: true
  path: %q
serve:
  projects:
    enabled: false
  web_push:
    vapid_public_key: fixture-public
    vapid_private_key: fixture-private
`, filepath.Join(root, "sessions.db"))
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	opts := serveRuntimeFixtureOptions()
	opts.Host, opts.Port, opts.Token = "127.0.0.1", port, "composition-token"
	opts.AuthMode, opts.BasePath = "bearer", "/ui"
	opts.DisableWidgets, opts.DisableExtensions, opts.NoProjects = true, true, true
	opts.Tools = "queue_agent,wait_for_jobs,live_settings"
	opts.Search, opts.NoSearch, opts.Debug = false, true, true

	results := make(chan compositionRootResult, 1)
	var runnerOptions cmdRunnerOptions
	var readyCalls atomic.Int32
	deps := serveDeps{Invocation: appserve.Invocation{
		Flags: serveFlagState{NoProjects: true, Tools: true}, Stdout: io.Discard, Stderr: io.Discard,
		ResolveApproval: func(*config.Config) (resolvedApprovalMode, error) {
			return resolvedApprovalMode{Mode: tools.ModeYolo, Source: approvalModeSourceCLI}, nil
		},
	}, Listener: listener,
		PlatformReady: func() { readyCalls.Add(1) },
		PlatformFactory: func(name string, _ *config.Config) (platform.Platform, error) {
			if name != "telegram" {
				return nil, errors.New("unexpected platform " + name)
			}
			return &compositionRootPlatform{result: results, cwd: root}, nil
		},
		RunnerFactory: func(_ *config.Config, captured cmdRunnerOptions) runpkg.Runner {
			runnerOptions = captured
			return &compositionFakeRunner{}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServeWith(ctx, deps, &opts, []string{"web", "telegram", "jobs"}) }()

	var result compositionRootResult
	select {
	case result = <-results:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("production platform composition did not run")
	}
	if result.err != nil || result.jobs.IsError {
		cancel()
		t.Fatalf("production jobs/live runtime wiring: output=%+v err=%v", result.jobs, result.err)
	}
	if result.runtime == nil || result.runtime.Engine == nil || result.runtime.Provider == nil || result.runtime.ProviderName == "" || result.runtime.ModelName != "debug" {
		cancel()
		t.Fatalf("production session runtime = %#v", result.runtime)
	}
	if result.settings.Store == nil || runnerOptions.Store != result.settings.Store || runnerOptions.AgentOwner == nil {
		cancel()
		t.Fatalf("production identities: settings store=%#v runner store=%#v owner=%p", result.settings.Store, runnerOptions.Store, runnerOptions.AgentOwner)
	}
	if result.settings.Search || runnerOptions.Search || !runnerOptions.NoSearch {
		cancel()
		t.Fatalf("production search sources: platform=%t runner=%t/%t", result.settings.Search, runnerOptions.Search, runnerOptions.NoSearch)
	}
	result.runtime.Cleanup()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServeWith shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServeWith did not stop")
	}
	if got := readyCalls.Load(); got != 1 {
		t.Fatalf("production outer Ready calls = %d, want 1", got)
	}
}

var _ runpkg.Runner = (*compositionFakeRunner)(nil)
var _ platform.Platform = (*compositionRootPlatform)(nil)
