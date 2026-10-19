package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func withServeSessionLogging(store session.Store) session.Store {
	if store == nil {
		return nil
	}
	if logged, ok := store.(*session.LoggingStore); ok {
		store = logged.Store
	}
	return session.NewLoggingStore(store, func(format string, args ...any) {
		log.Printf("[serve] "+format, args...)
	})
}

func resolveServeAgentMetadata(cfg *config.Config, agent *agents.Agent, hasWeb bool) (string, []string, agents.PlatformMessagesConfig, error) {
	if agent != nil {
		return agent.Name, nil, agent.PlatformMessages, nil
	}
	if hasWeb {
		names, err := ListAgentNames(cfg)
		if err != nil {
			return "", nil, agents.PlatformMessagesConfig{}, fmt.Errorf("list web agents: %w", err)
		}
		return "", names, agents.PlatformMessagesConfig{}, nil
	}
	return "", nil, agents.PlatformMessagesConfig{}, nil
}

type serveAgentRuntimeOptions struct {
	flags                   ServeOptions
	toolsSet                bool
	systemMessageSet        bool
	debugRaw                bool
	cfg                     *config.Config
	store                   session.Store
	sessionStore            runtime.SessionStoreOverrides
	memoryPaths             appmemory.Paths
	approval                resolvedApprovalMode
	approvalErrWriter       io.Writer
	toolMap                 map[string]string
	mediaPublisher          *serveMediaPublisher
	collaborationController *serveCollaborativeShellController
	// childRuns makes delegated runs readable and steerable. It is nil outside
	// serve, which is what keeps the CLI spawn path unchanged.
	childRuns       *childRunRegistry
	hasWeb          bool
	jobsServerURL   string
	jobsServerToken string
	agentOwner      *runtime.AgentHostOwner
}

func newServeAgentRuntimeFactory(opts serveAgentRuntimeOptions, server func() *serveServer) func(context.Context, serveRuntimeRequest) (*serveRuntime, error) {
	return func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		// runServe creates this factory before the HTTP server. Resolve its
		// registry at invocation, not construction, or every child inherits nil.
		requestOpts := opts
		requestOpts.childRuns = server().ensureChildRuns()
		requestOpts.agentOwner = server().agentOwner
		return newServeAgentRuntime(ctx, request, requestOpts)
	}
}

func newServeAgentRuntime(ctx context.Context, request serveRuntimeRequest, opts serveAgentRuntimeOptions) (*serveRuntime, error) {
	flags := &opts.flags
	runtimeAgent := strings.TrimSpace(flags.Agent)
	if runtimeAgent == "" {
		runtimeAgent = strings.TrimSpace(request.Agent)
	}
	approvalMode := opts.approval.Mode
	if request.approvalMode != nil {
		approvalMode = *request.approvalMode
	}
	runner := &cmdRunner{baseCfg: opts.cfg, defaults: serveRuntimeRunnerDefaults(opts, request, approvalMode)}
	env, err := runner.prepare(ctx, runpkg.Request{
		// Live session switching rejects child sessions in resolveLiveSwitchTarget;
		// this factory therefore only prepares top-level runtimes. Subagents use
		// the child runner path, which supplies Depth and RemainingDepth.
		SessionID: request.SessionID, Cwd: request.RuntimeDir, Platform: runpkg.PlatformWeb,
		AgentName: runtimeAgent, Provider: strings.TrimSpace(request.Provider), Model: strings.TrimSpace(request.Model), DeferSession: true,
	}, nil)
	if err != nil {
		return nil, err
	}
	runtime := env.runtime
	runtime.toolMap = opts.toolMap
	runtime.platform = "web"
	runtime.sideProviderFactory = func(providerKey, model string) (core.Provider, error) {
		if err := runtime.modelPolicy.CheckWithConfig(opts.cfg, runtime.agentName, providerKey, model); err != nil {
			return nil, err
		}
		return llm.NewProviderByName(opts.cfg, providerKey, model)
	}
	runtime.configureSideQuestionContext()
	if err := validateServeRuntimeToolMap(runtime, opts.toolMap); err != nil {
		runtime.Close()
		return nil, err
	}
	configureServeRuntimeTools(runtime, opts)
	runtime.Touch()
	return runtime, nil
}

func serveRuntimeRunnerDefaults(opts serveAgentRuntimeOptions, request serveRuntimeRequest, approvalMode tools.ApprovalMode) cmdRunnerOptions {
	flags := &opts.flags
	// Leave WireSpawn unset: cmdRunner binds child persistence to this store and session.
	return cmdRunnerOptions{
		Inputs: request.Inputs, RestoreSettings: request.settings, RestoreAgentSkills: request.agentSkills,
		ToolsSet: opts.toolsSet, SystemMessageSet: opts.systemMessageSet,
		Provider: flags.Provider, Tools: flags.Tools, ReadDirs: append([]string(nil), flags.ReadDirs...), WriteDirs: append([]string(nil), flags.WriteDirs...), ShellAllow: append([]string(nil), flags.ShellAllow...),
		MCP: flags.MCP, SystemMessage: flags.SystemMessage, MaxTurns: flags.MaxTurns, Search: flags.Search, NoSearch: flags.NoSearch,
		NativeSearch: flags.NativeSearch, NoNativeSearch: flags.NoNativeSearch, ApprovalMode: approvalMode, ApprovalModeSet: true,
		ApprovalSource: opts.approval.Source, ApprovalHeadless: true, ApprovalPrepare: true, ApprovalDiagnostics: flags.Verbose,
		Debug: flags.Debug, DebugRaw: opts.debugRaw, ErrWriter: opts.approvalErrWriter, Store: opts.store,
		SessionStore: opts.sessionStore, MemoryPaths: opts.memoryPaths,
		ChildRunObserver: childRunObserverOrNil(opts.childRuns), AgentOwner: opts.agentOwner,
	}
}

// childRunObserverOrNil avoids handing the runner a non-nil interface wrapping
// a nil registry, which would make every "is a host watching?" check true.
func childRunObserverOrNil(registry *childRunRegistry) childRunObserver {
	if registry == nil {
		return nil
	}
	return registry
}

func serveJobsToolURL(host string, port int, basePath string, mountedUnderBasePath bool) string {
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	baseURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	if mountedUnderBasePath {
		baseURL += basePath
	}
	return baseURL
}

func serveJobsToolClientConfig(hasJobs, hasWeb, hasAPI bool, host string, port int, basePath, token string) (string, string) {
	if !hasJobs {
		return "", ""
	}
	return serveJobsToolURL(host, port, basePath, hasWeb || hasAPI), token
}

func validateServeRuntimeToolMap(runtime *serveRuntime, toolMap map[string]string) error {
	for clientName, serverName := range toolMap {
		if _, ok := runtime.engine.Tools().Get(serverName); ok {
			continue
		}
		names := make([]string, 0)
		for _, spec := range runtime.engine.Tools().AllSpecs() {
			names = append(names, spec.Name)
		}
		return fmt.Errorf("--tool-map %s:%s: server tool %q not found (registered tools: %v)", clientName, serverName, serverName, names)
	}
	return nil
}

func configureServeRuntimeTools(runtime *serveRuntime, opts serveAgentRuntimeOptions) {
	flags := &opts.flags
	if runtime.toolMgr == nil {
		return
	}
	runtime.toolMgr.ApprovalMgr.GuardianEventFunc = runtime.emitGuardianReview
	if spawn := runtime.toolMgr.GetSpawnAgentTool(); spawn != nil {
		spawn.SetEventCallback(runtime.recordSubagentStats)
	}

	imageBaseURL := ""
	if opts.hasWeb {
		imageBaseURL = strings.TrimRight(flags.BasePath, "/") + "/images/"
	}
	runtime.toolMgr.Registry.SetServeMode(true, imageBaseURL)
	runtime.toolMgr.Registry.SetMediaPublisher(opts.mediaPublisher)
	if opts.jobsServerURL != "" {
		runtime.toolMgr.Registry.SetJobsServer(opts.jobsServerURL, opts.jobsServerToken)
	}
	if opts.hasWeb {
		runtime.toolMgr.Registry.SetCollaborativeShellController(opts.collaborationController, tools.ShellRoutingControllerRequired)
	} else {
		runtime.toolMgr.Registry.SetCollaborativeShellController(nil, tools.ShellRoutingLocalOnly)
	}
	if opts.approval.Mode != tools.ModeYolo {
		runtime.toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
		runtime.toolMgr.ApprovalMgr.DebugApproval = flags.Debug
		runtime.toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite, isShell bool, workDir string) (tools.ApprovalResult, error) {
			return runtime.awaitApproval(path, isWrite, isShell, workDir)
		}
		runtime.toolMgr.ApprovalMgr.SharedShellPromptUIFunc = runtime.awaitSharedShellApproval
	}
	if opts.hasWeb {
		runtime.toolMgr.ApprovalMgr.WorkspacePromptFunc = runtime.awaitWorkspaceApproval
	}
}
