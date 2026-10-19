package cmd

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/session"
)

type servePlatformFactory func(string, *config.Config) (serve.Platform, error)
type servePlatformDelay func(context.Context, time.Duration) bool

func composeServePlatformReady(ready, observer func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ready()
			if observer != nil {
				observer()
			}
		})
	}
}

type servePlatformCompositionOptions struct {
	cfg                 *config.Config
	flags               ServeOptions
	resolved            SessionSettings
	debugRaw            bool
	forceExternalSearch bool
	agentName           string
	platformMessages    agents.PlatformMessagesConfig
	store               session.Store
	agentOwner          *runtime.AgentHostOwner
	approval            resolvedApprovalMode
	approvalErrWriter   io.Writer
	sessionStore        runtime.SessionStoreOverrides
	memoryPaths         appmemory.Paths
	newRunner           func(*config.Config, cmdRunnerOptions) runpkg.Runner
	newRuntime          func(context.Context) (*serveRuntime, error)
}

func composeServePlatformSettings(opts servePlatformCompositionOptions) serve.Settings {
	runnerOptions := cmdRunnerOptions{
		AgentOwner: opts.agentOwner, Provider: opts.flags.Provider, Tools: opts.flags.Tools,
		ReadDirs: append([]string(nil), opts.flags.ReadDirs...), WriteDirs: append([]string(nil), opts.flags.WriteDirs...),
		ShellAllow: append([]string(nil), opts.flags.ShellAllow...), MCP: opts.flags.MCP,
		SystemMessage: opts.flags.SystemMessage, MaxTurns: opts.flags.MaxTurns,
		Search: opts.flags.Search, NoSearch: opts.flags.NoSearch,
		NativeSearch: opts.flags.NativeSearch, NoNativeSearch: opts.flags.NoNativeSearch,
		ApprovalMode: opts.approval.Mode, ApprovalModeSet: true, ApprovalSource: opts.approval.Source,
		ApprovalHeadless: true, ApprovalDiagnostics: opts.flags.Verbose,
		Debug: opts.flags.Debug, DebugRaw: opts.debugRaw, ErrWriter: opts.approvalErrWriter,
		Store: opts.store, SessionStore: opts.sessionStore, MemoryPaths: opts.memoryPaths,
	}
	newRunner := opts.newRunner
	if newRunner == nil {
		newRunner = newCmdRunner
	}
	return serve.Settings{
		SystemPrompt: opts.resolved.SystemPrompt, IdleTimeout: opts.flags.SessionTTL,
		TelegramCarryoverChars: opts.flags.TelegramCarryoverChars, MaxTurns: opts.resolved.MaxTurns,
		Debug: opts.flags.Debug, DebugRaw: opts.debugRaw, Search: opts.resolved.Search,
		ForceExternalSearch: opts.forceExternalSearch, Tools: opts.resolved.Tools, MCP: opts.resolved.MCP,
		Agent: opts.agentName, TimeGrounding: opts.resolved.TimeGrounding,
		PlatformMessages: opts.platformMessages, Store: opts.store,
		Runner:     newRunner(opts.cfg, runnerOptions),
		NewSession: composeServePlatformSessionFactory(opts.newRuntime),
	}
}

func composeServePlatformSessionFactory(factory func(context.Context) (*serveRuntime, error)) func(context.Context) (*serve.SessionRuntime, error) {
	return func(ctx context.Context) (*serve.SessionRuntime, error) {
		runtime, err := factory(ctx)
		if err != nil {
			return nil, err
		}
		return &serve.SessionRuntime{
			Engine: runtime.engine, Provider: runtime.provider,
			ProviderName: runtime.provider.Name(), ModelName: runtime.defaultModel,
			Cleanup: runtime.Close,
		}, nil
	}
}

func composeServePlatforms(names []string, cfg *config.Config, factory servePlatformFactory) ([]serve.Platform, error) {
	if factory == nil {
		factory = defaultServePlatformFactory
	}
	platforms := make([]serve.Platform, 0, len(names))
	for _, name := range names {
		switch name {
		case "web", "api", "jobs":
			// These surfaces are handled by the HTTP serveServer.
		default:
			platform, err := factory(name, cfg)
			if err != nil {
				return nil, err
			}
			platforms = append(platforms, platform)
		}
	}
	return platforms, nil
}

func defaultServePlatformFactory(name string, cfg *config.Config) (serve.Platform, error) {
	if name == "telegram" {
		return serve.NewTelegramPlatform(cfg.Serve.Telegram), nil
	}
	return nil, fmt.Errorf("unknown platform: %s", name)
}
