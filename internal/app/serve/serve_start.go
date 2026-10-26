package serve

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/agents"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/app/reload"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	bootstrap "github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	guardian "github.com/samsaffron/term-llm/internal/app/runtime/guardian"
	"github.com/samsaffron/term-llm/internal/app/serve/hubnode"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/restart"
	platform "github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
)

func runServeWith(parentCtx context.Context, deps serveDeps, opts *Options, args []string) error {
	defer closeServeListener(deps.Listener)()
	startup, err := resolveServeStartup(deps.Flags, opts)
	if err != nil {
		return err
	}
	startupDir := startup.startupDir
	sidebarSessions := startup.sidebarSessions
	authMode := startup.authMode
	passkeyEndpoint := startup.passkeyEndpoint
	requireAuth := startup.requireAuth
	token, tokenSource := startup.token, startup.tokenSource
	hubRegistrationToken := startup.hubRegistrationToken

	ctx, stop := signal.NotifyContextWithParent(parentCtx)
	defer stop()

	cfg, err := bootstrap.LoadConfigWithSetup()
	if err != nil {
		return err
	}
	runtime.ApplyServeFileTrackingOverride(cfg, opts.EnableFileTracking)
	if deps.ResolveApproval == nil {
		return fmt.Errorf("serve approval resolver is required")
	}
	resolvedApproval, err := deps.ResolveApproval(cfg)
	if err != nil {
		return err
	}
	resolvedYolo := resolvedApproval.Mode == tools.ModeYolo
	opts.Approval = resolvedApproval.Mode.String()
	opts.Yolo = resolvedYolo
	opts.Auto = resolvedApproval.Mode == tools.ModeAuto

	platformNames, err := resolvePlatforms(args, cfg.Serve.Platforms)
	if err != nil {
		return err
	}
	hasJobs := platformContains(platformNames, "jobs")
	hasWeb := platformContains(platformNames, "web")
	hasAPI := platformContains(platformNames, "api")
	hasTelegram := platformContains(platformNames, "telegram")
	var browserAuth *browserPasskeyHandler
	var bootstrapDisplay string
	if authMode == "passkey" {
		if !hasWeb || len(platformNames) != 1 {
			return fmt.Errorf("--auth passkey supports serve web only (no additional platforms)")
		}
		if err := validateWebPasskeyConfigPath(deps.Flags.BasePath, passkeyEndpoint, cfg.Serve.BasePath); err != nil {
			return err
		}
		var closePasskeys func() error
		browserAuth, bootstrapDisplay, closePasskeys, err = openWebPasskeys(deps.Stderr, passkeyEndpoint, opts)
		if err != nil {
			return err
		}
		defer func() {
			if err := closePasskeys(); err != nil {
				log.Printf("passkey state close: %v", err)
			}
		}()
	}

	// Auto-generate VAPID keys for web push if not already configured.
	if hasWeb && !appnotify.WebPushConfigured(cfg) {
		privKey, pubKey, genErr := webpush.GenerateVAPIDKeys()
		if genErr != nil {
			return fmt.Errorf("generate VAPID keys: %w", genErr)
		}
		wpCfg := config.WebPushConfig{
			VAPIDPublicKey:  pubKey,
			VAPIDPrivateKey: privKey,
			Subject:         cfg.Serve.WebPush.Subject,
		}
		if err := config.SetServeWebPushConfig(wpCfg); err != nil {
			return fmt.Errorf("persist VAPID keys: %w (web push requires stable keys across restarts)", err)
		}
		cfg.Serve.WebPush = wpCfg
		log.Println("generated VAPID keys for web push (saved to config)")
	}

	// Apply config fallback for base-path if not set via flag
	if authMode != "passkey" && !deps.Flags.BasePath && cfg.Serve.BasePath != "" {
		opts.BasePath = cfg.Serve.BasePath
	}
	opts.BasePath, err = normalizeBasePath(opts.BasePath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.PublicURL) == "" {
		opts.PublicURL = strings.TrimSpace(os.Getenv("TERM_LLM_SERVE_PUBLIC_URL"))
	}
	opts.PublicURL, err = normalizeServePublicURL(opts.PublicURL)
	if err != nil {
		return fmt.Errorf("invalid serve --public-url: %w", err)
	}

	resolvedTitle := strings.TrimSpace(opts.Title)
	if !deps.Flags.Title {
		resolvedTitle = strings.TrimSpace(cfg.Serve.Title)
	}
	locationSharingDisabled := opts.DisableLocationSharing
	if !deps.Flags.DisableLocationSharing {
		locationSharingDisabled = cfg.Serve.DisableLocationSharing
	}

	responseTimeout, err := resolveServeResponseTimeout(deps.Flags.ResponseTimeout, opts.ResponseTimeout, cfg.Serve.ResponseTimeout)
	if err != nil {
		return err
	}

	var agent *agents.Agent
	if hasWeb || hasAPI || hasTelegram {
		agent, err = runtime.LoadAgent(opts.Agent, cfg)
		if err != nil {
			return err
		}
	}

	agentProvider := ""
	agentModel := ""
	if agent != nil {
		agentProvider = agent.Provider
		agentModel = agent.Model
	}
	if err := bootstrap.ApplyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, opts.Provider, agentProvider, agentModel); err != nil {
		return err
	}

	promptProvider, promptModel := activeLLMFlags(cfg)
	settings, err := ResolveSettings(cfg, agent, CLIFlags{
		Provider:       opts.Provider,
		ActiveProvider: promptProvider,
		ActiveModel:    promptModel,
		Tools:          opts.Tools,
		ReadDirs:       opts.ReadDirs,
		WriteDirs:      opts.WriteDirs,
		ShellAllow:     opts.ShellAllow,
		MCP:            opts.MCP,
		SystemMessage:  opts.SystemMessage,
		MaxTurns:       opts.MaxTurns,
		MaxTurnsSet:    deps.Flags.MaxTurns,
		Search:         opts.Search,
		NoSearch:       opts.NoSearch,
		Platform:       singleServeTemplatePlatform(platformNames),
		MemoryPaths:    deps.MemoryPaths,
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 50)
	if err != nil {
		return err
	}

	// Setup skills once for this serve process. The <available_skills> XML is
	// injected here into settings.SystemPrompt so that both the web serveRuntime
	// and the Telegram serveSettings pick it up correctly. Per-session engine
	// registration (activate_skill tool) still happens inside the factory via
	// newServeEngineWithTools.
	agentSkills := ""
	if agent != nil {
		agentSkills = agent.Skills
	}
	effectiveSkillsConfig := runtime.ApplySkillsFlag(&cfg.Skills, agentSkills)
	baseSystemPrompt := settings.SystemPrompt
	skillsSetup := SetupSkills(effectiveSkillsConfig, "", "", deps.Stderr)
	settings.SystemPrompt = InjectSkillsMetadata(baseSystemPrompt, skillsSetup)

	agentName, availableAgentNames, agentPlatformMsgs, err := resolveServeAgentMetadata(cfg, agent, hasWeb)
	if err != nil {
		return err
	}

	store, storeCleanup := runtime.InitSessionStoreWith(cfg, deps.Stderr, deps.SessionStore)
	defer storeCleanup()
	store = withServeSessionLogging(store)

	projectsEnabled, bootstrapProjectID, err := initializeServeProjectRuntime(ctx, deps.Flags, opts, cfg, store, startupDir, hasWeb, deps.Stderr)
	if err != nil {
		return err
	}

	forceExternalSearch := bootstrap.ResolveForceExternalSearch(cfg, opts.NativeSearch, opts.NoNativeSearch)
	mediaPublisher, err := newServeMediaPublisher()
	if err != nil {
		return fmt.Errorf("initialize media publisher: %w", err)
	}

	toolMap, err := ParseToolMap(opts.ToolMap)
	if err != nil {
		return err
	}

	modelName := bootstrap.ActiveModel(cfg)
	if err := guardian.PreflightHeadlessApproval(cfg, resolvedApproval); err != nil {
		return err
	}
	var approvalErrWriter io.Writer = io.Discard
	if opts.Debug || opts.Verbose {
		approvalErrWriter = deps.Stderr
	}
	// The runner below is built before the server; share one owner pointer.
	agentOwner := &runtime.AgentHostOwner{}
	var s *serveServer
	serverStarted := false
	collaborationController := &serveCollaborativeShellController{manager: func() (*serveShellManager, error) {
		if s == nil {
			return nil, errServeShellClosed
		}
		return s.shellManager()
	}}
	runtimeOptions := serveAgentRuntimeOptions{
		flags: *opts, toolsSet: deps.Flags.Tools, systemMessageSet: deps.Flags.System, debugRaw: deps.DebugRaw,
		cfg: cfg, store: store, sessionStore: deps.SessionStore, memoryPaths: deps.MemoryPaths, approval: resolvedApproval, approvalErrWriter: approvalErrWriter,
		toolMap: toolMap, mediaPublisher: mediaPublisher, collaborationController: collaborationController,
		hasWeb: hasWeb,
	}
	runtimeOptions.jobsServerURL, runtimeOptions.jobsServerToken = serveJobsToolClientConfig(hasJobs, hasWeb, hasAPI, opts.Host, opts.Port, opts.BasePath, token)
	agentRuntimeFactory := newServeAgentRuntimeFactory(runtimeOptions, func() *serveServer { return s })

	runtimeFactory := func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		return agentRuntimeFactory(ctx, request)
	}
	factory := func(ctx context.Context) (*serveRuntime, error) {
		return runtimeFactory(ctx, serveRuntimeRequest{})
	}
	sessionMgr := newServeSessionManager(opts.SessionTTL, opts.SessionMax, factory)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessionMgr.CloseContext(shutdownCtx)
	}()

	if hasJobs && strings.TrimSpace(opts.Agent) != "" {
		fmt.Fprintln(deps.Stderr, "warning: --agent is ignored for --platform jobs; set llm runner_config.agent_name per job definition")
	}

	// Build the serve.Settings used by non-web platforms. Resolved settings feed
	// the platform while raw flags remain the defaults for its generic runner.
	serveSettings := composeServePlatformSettings(servePlatformCompositionOptions{
		cfg: cfg, flags: *opts, resolved: settings, debugRaw: deps.DebugRaw,
		forceExternalSearch: forceExternalSearch, agentName: agentName,
		platformMessages: agentPlatformMsgs, store: store, agentOwner: agentOwner,
		approval: resolvedApproval, approvalErrWriter: approvalErrWriter,
		sessionStore: deps.SessionStore, memoryPaths: deps.MemoryPaths,
		newRunner: deps.RunnerFactory, newRuntime: factory,
	})

	// Instantiate non-web platforms.
	platforms, err := composeServePlatforms(platformNames, cfg, deps.PlatformFactory)
	if err != nil {
		return err
	}

	// Run setup wizard for platforms that need it (or --setup flag).
	for _, p := range platforms {
		if opts.Setup || p.NeedsSetup() {
			if err := p.RunSetup(); err != nil {
				return fmt.Errorf("setup %s: %w", p.Name(), err)
			}
		}
	}

	hasHTTP := hasWeb || hasAPI || hasJobs

	var hubRegistration *hubnode.Registration
	if hasHTTP {
		var jobsV2 *jobsV2Manager
		serveUI := hasWeb
		// This is the only serveServer construction path. Process reload re-execs
		// the serve command, so provider resolution is validated here again before
		// the replacement server can accept live sessions.
		liveClassifier, liveDecisionStore, liveClassifyErr := prepareLiveClassify(cfg)
		if liveClassifyErr != nil {
			return liveClassifyErr
		}
		defer closeLiveDecisionStore(liveDecisionStore)

		widgetsMgr, stopWidgets, widgetsErr := setupServeWidgets(ctx, cfg, hasWeb, opts)
		if widgetsErr != nil {
			return widgetsErr
		}
		defer stopWidgets()

		s = &serveServer{
			agentOwner:  agentOwner,
			browserAuth: browserAuth,
			listener:    deps.Listener,
			cfg: serveServerConfig{
				host:                    opts.Host,
				port:                    opts.Port,
				requireAuth:             requireAuth,
				token:                   token,
				ui:                      serveUI,
				api:                     hasAPI,
				suppressServerTools:     opts.FilterServerTools,
				verbose:                 opts.Verbose,
				debug:                   opts.Debug,
				debugRaw:                deps.DebugRaw,
				basePath:                opts.BasePath,
				publicURL:               opts.PublicURL,
				uiTitle:                 resolvedTitle,
				locationSharingDisabled: locationSharingDisabled,
				sidebarSessions:         append([]string(nil), sidebarSessions...),
				agentName:               agentName,
				agentNames:              append([]string(nil), availableAgentNames...),
				corsOrigins:             append([]string(nil), opts.CORSOrigins...),
				filesDir:                resolveFilesDir(opts.FilesDir, cfg),
				writeDirs:               resolveServeWriteDirs(opts.WriteDirs, cfg),
				responseTimeout:         responseTimeout,
				hubURL:                  strings.TrimSpace(opts.HubURL),
				hubNodeID:               strings.TrimSpace(opts.HubNodeID),
				hubNodeName:             strings.TrimSpace(opts.HubNodeName),
			},
			sessionMgr:          sessionMgr,
			jobsV2:              jobsV2,
			cfgRef:              cfg,
			store:               store,
			sessionStore:        deps.SessionStore,
			memoryPaths:         deps.MemoryPaths,
			liveClassifier:      liveClassifier,
			liveDecisionStore:   liveDecisionStore,
			mediaPublisher:      mediaPublisher,
			approvalDefault:     resolvedApproval.Mode,
			projectsEnabled:     projectsEnabled,
			bootstrapProjectID:  bootstrapProjectID,
			startupDir:          startupDir,
			baseSystemPrompt:    baseSystemPrompt,
			skillsSetup:         skillsSetup,
			skillsConfig:        effectiveSkillsConfig,
			runtimeFactory:      runtimeFactory,
			agentRuntimeFactory: agentRuntimeFactory,
			widgetsMgr:          widgetsMgr,
			shells: newServeShellManager(opts.SessionTTL, func(sessionID string) bool {
				if store == nil {
					return false
				}
				sess, getErr := store.Get(context.Background(), sessionID)
				return getErr == nil && sess != nil
			}),
		}
		if hasWeb {
			storeID, storeErr := session.SessionInputStoreIdentity(store)
			if attentionStore, ok := session.AsAttentionStore(store); ok {
				storeID, storeErr = attentionStore.StoreInstanceID(ctx)
			}
			if storeErr != nil {
				storeID = ""
			}
			s.linkedDevices, err = linkeddevices.NewServer(agentName, storeID, opts.BasePath, requireAuth || browserAuth != nil, resolvedTitle, fmt.Sprintf("can see every conversation this server shows; messages drive tools under approval mode %s", resolvedApproval.Mode), linkeddevices.ServerOptionsFromConfig(cfg))
			if err != nil {
				return fmt.Errorf("initialize linked devices: %w", err)
			}
		}
		if hasWeb {
			s.extensions, err = newServeExtensions(opts.ExtensionsDir, opts.ExtensionIDs, deps.Flags.Extensions, opts.DisableExtensions)
			if err != nil {
				return fmt.Errorf("extensions: %w", err)
			}
		}
		if hasJobs {
			jobsV2, err = newServeJobsV2Manager(cfg, opts.JobsWorkers, resolvedApproval, s.notifyJobsV2RunDone, deps.SessionStore, deps.MemoryPaths, *opts, deps.DebugRaw)
			if err != nil {
				return fmt.Errorf("initialize jobs v2 manager: %w", err)
			}
			s.jobsV2 = jobsV2
		}
		sessionMgr.onEvict = func(rt *serveRuntime) {
			for _, rid := range rt.getResponseIDs() {
				s.responseToSession.Delete(rid)
			}
		}

		reverseHubURL := strings.TrimSpace(opts.HubURL)
		reverseHubNodeID := strings.TrimSpace(opts.HubNodeID)
		if opts.HubRegister {
			if opts.HubConnect != "reverse" {
				return fmt.Errorf("--hub-register requires --hub-connect reverse")
			}
			if reverseHubURL == "" || reverseHubNodeID == "" || token == "" || hubRegistrationToken == "" {
				return fmt.Errorf("--hub-register requires --hub-url, --hub-node-id, --hub-registration-token, and a bearer --token")
			}
		}
		if opts.HubConnect == "reverse" && (reverseHubURL == "" || reverseHubNodeID == "" || token == "") {
			return fmt.Errorf("--hub-connect reverse requires --hub-url, --hub-node-id, and a bearer --token")
		}

		// Register after dependency cleanup so a failed startup stops HTTP first.
		defer func() {
			if !serverStarted || s == nil {
				return
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.Stop(shutdownCtx)
		}()

		if err := s.Start(); err != nil {
			return err
		}
		serverStarted = true

		if opts.HubRegister {
			var err error
			hubRegistration, err = hubnode.Start(ctx, nil, reverseHubURL, hubRegistrationToken, hubnode.RegisterRequest{
				ID:         reverseHubNodeID,
				Name:       strings.TrimSpace(opts.HubNodeName),
				Connection: "reverse",
				BasePath:   opts.BasePath,
				Token:      token,
			}, nil)
			if err != nil {
				return err
			}
			if hubRegistration.Registered() {
				fmt.Fprintf(deps.Stderr, "hub registration: registered %s with %s\n", reverseHubNodeID, reverseHubURL)
			}
		}

		if opts.HubConnect == "reverse" {
			localBase := localHubConnectBase(opts.Host, opts.Port)
			go runHubReverseConnector(ctx, reverseHubURL, reverseHubNodeID, token, localBase, opts.BasePath, newHubReverseLocalClient())
			fmt.Fprintf(deps.Stderr, "hub reverse: connecting %s to %s\n", reverseHubNodeID, reverseHubURL)
		}

		fmt.Fprintf(deps.Stderr, "term-llm serve listening on http://%s\n", net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
		if browserAuth == nil {
			fmt.Fprintf(deps.Stderr, "auth: %s\n", authSummary(requireAuth))
		}
		if authMode == "bearer" {
			switch tokenSource {
			case tokenSourceGenerated:
				fmt.Fprintf(deps.Stderr, "token: %s (auto-generated; export TERM_LLM_SERVE_TOKEN to persist)\n", token)
			case tokenSourceEnv:
				fmt.Fprintln(deps.Stderr, "token: configured via $TERM_LLM_SERVE_TOKEN (redacted)")
			default:
				fmt.Fprintln(deps.Stderr, "token: configured via --token (redacted)")
			}
		}
		fmt.Fprintf(deps.Stderr, "ui: %v\n", s.cfg.ui)
		if warning := plainHTTPConnectionWarning(s.cfg.ui, opts.Host, opts.PublicURL); warning != "" {
			fmt.Fprintln(deps.Stderr, warning)
		}
		if hasJobs {
			fmt.Fprintf(deps.Stderr, "jobs workers: %d\n", opts.JobsWorkers)
		}
		if modelName != "" {
			fmt.Fprintf(deps.Stderr, "model: %s\n", modelName)
		}
		fmt.Fprintf(deps.Stderr, "response timeout: %s\n", humanDuration(responseTimeout))
		if browserAuth != nil {
			printWebPasskeyStatus(deps.Stderr, browserAuth, bootstrapDisplay)
		}
	}

	// Start non-web platforms concurrently.
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
	}()
	for _, p := range platforms {
		platformCtx, startingDone, err := restart.Default.Root(ctx)
		if err != nil {
			return err
		}
		settings := serveSettings
		settings.Ready = composeServePlatformReady(startingDone, deps.PlatformReady)
		wg.Add(1)
		go func(p platform.Platform) {
			defer wg.Done()
			defer settings.Ready()
			runPlatformSupervisor(platformCtx, cfg, settings, p, servePlatformInitialRetryDelay, deps.PlatformDelay)
		}(p)
	}

	stopReload, reloadErr := reload.Bind(ctx, "TERM_LLM_SERVE_RELOAD_TOKEN="+token)
	if reloadErr != nil {
		return reloadErr
	}
	defer stopReload()
	shutdownServe(ctx, s, &wg, hubRegistration)
	serverStarted = false

	return nil
}
