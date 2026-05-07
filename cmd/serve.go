package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/widgets"
	"github.com/spf13/cobra"
)

var (
	serveHost                   string
	servePort                   int
	serveToken                  string
	serveAllowNoAuth            bool
	serveAuthMode               string
	serveBasePath               string
	serveCORSOrigins            []string
	serveSessionTTL             time.Duration
	serveSessionMax             int
	serveDebug                  bool
	serveSearch                 bool
	serveProvider               string
	serveMCP                    string
	serveNativeSearch           bool
	serveNoNativeSearch         bool
	serveMaxTurns               int
	serveTools                  string
	serveReadDirs               []string
	serveWriteDirs              []string
	serveShellAllow             []string
	serveSystemMessage          string
	serveAgent                  string
	serveYolo                   bool
	serveTelegramCarryoverChars int
	serveJobsWorkers            int
	serveSetup                  bool
	serveVerbose                bool
	serveFilterServerTools      bool
	serveSidebarSessions        string
	serveToolMap                []string
	serveFilesDir               string
	serveEnableWidgets          bool
	serveWidgetsDir             string
	serveResponseTimeout        time.Duration
)

var serveCmd = &cobra.Command{
	Use:   "serve <platform> [platform...]",
	Short: "Run the agent as a server (web, api, jobs, Telegram, or any combination)",
	Long: `Run term-llm as a server on one or more platforms simultaneously.

Available platforms:
  web        HTTP server with chat UI
  api        HTTP server with API endpoints only (no UI)
  jobs       HTTP server with async job runner
  telegram   Telegram bot

Platforms are specified as positional arguments. If none are given, the
serve.platforms list from config.yaml is used.

Examples:
  term-llm serve web             # web server with UI enabled
  term-llm serve api             # API only (no chat UI)
  term-llm serve telegram        # Telegram bot only
  term-llm serve telegram web    # both platforms
  term-llm serve web --base-path /chat

All HTTP routes are mounted under --base-path (default /ui):
  POST {base}/v1/responses
  POST {base}/v1/chat/completions
  POST {base}/v1/messages
  POST {base}/v1/transcribe
  GET  {base}/v1/models
  GET  {base}/healthz
  GET  {base}/                       (web UI)
  GET  {base}/images/:file

Jobs endpoints (also under base-path):
  POST   {base}/v2/jobs
  GET    {base}/v2/jobs
  GET    {base}/v2/jobs/:id
  PATCH  {base}/v2/jobs/:id
  DELETE {base}/v2/jobs/:id
  POST   {base}/v2/jobs/:id/trigger
  POST   {base}/v2/jobs/:id/pause
  POST   {base}/v2/jobs/:id/resume
  GET    {base}/v2/runs
  GET    {base}/v2/runs/:id
  GET    {base}/v2/runs/:id/events
  POST   {base}/v2/runs/:id/cancel

Use --setup to configure credentials for the selected platforms.`,
	ValidArgsFunction: servePlatformCompletion,
	RunE:              runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)

	serveCmd.Flags().StringVar(&serveHost, "host", "127.0.0.1", "Bind host")
	serveCmd.Flags().IntVar(&servePort, "port", 8080, "Bind port")
	serveCmd.Flags().StringVar(&serveToken, "token", "", "Bearer token for API auth (auto-generated if omitted)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "no-auth", false, "Disable auth (only allowed on loopback host)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "allow-no-auth", false, "Disable auth (alias for --no-auth)")
	_ = serveCmd.Flags().MarkHidden("allow-no-auth")
	serveCmd.Flags().StringVar(&serveAuthMode, "auth", "bearer", "Auth mode: bearer or none")
	serveCmd.Flags().StringVar(&serveBasePath, "base-path", "/ui", "URL prefix the UI uses for session URLs (e.g. /chat)")
	serveCmd.Flags().StringArrayVar(&serveCORSOrigins, "cors-origin", nil, "Allowed CORS origin (repeatable, or '*' for all)")
	serveCmd.Flags().DurationVar(&serveSessionTTL, "session-ttl", 30*time.Minute, "Stateful session idle TTL")
	serveCmd.Flags().IntVar(&serveSessionMax, "session-max", 1000, "Max stateful sessions in memory")

	serveCmd.Flags().BoolVar(&serveSetup, "setup", false, "Re-run setup wizard for selected platforms")
	serveCmd.Flags().IntVar(&serveTelegramCarryoverChars, "telegram-carryover-chars", 4000, "Characters of previous Telegram session context to carry into replacement sessions (0 disables)")
	serveCmd.Flags().IntVar(&serveJobsWorkers, "jobs-workers", 4, "Number of concurrent job workers for --platform jobs")
	serveCmd.Flags().StringVar(&serveSidebarSessions, "sidebar-sessions", "all", "Default web sidebar session categories: all or a comma-separated list like chat,web,ask,plan,exec")
	serveCmd.Flags().BoolVar(&serveVerbose, "verbose", false, "Log API request/response summaries to stderr")
	serveCmd.Flags().StringArrayVar(&serveToolMap, "tool-map", nil, "Map client tool name to server tool (repeatable, format ClientName:ServerName)")
	serveCmd.Flags().BoolVar(&serveFilterServerTools, "suppress-server-tool-calls", false, "Hide server-executed tool calls from API responses (use when proxying to external clients)")
	serveCmd.Flags().StringVar(&serveFilesDir, "files-dir", "", "Directory for serving arbitrary files (videos, PDFs, etc) at {base}/files/")
	serveCmd.Flags().BoolVar(&serveEnableWidgets, "enable-widgets", false, "Enable local widget apps proxied under {base}/widgets/<mount>/")
	serveCmd.Flags().StringVar(&serveWidgetsDir, "widgets-dir", "", "Directory containing widget sub-directories (default: ~/.config/term-llm/widgets)")
	serveCmd.Flags().DurationVar(&serveResponseTimeout, "response-timeout", defaultServeRequestTimeout, "Maximum duration for API/web response runs before timing out")

	AddCommonFlags(serveCmd,
		CommonCoreFlags|CommonSearch|CommonNativeSearch|CommonMaxTurns|CommonAgent,
		CommonFlagBindings{
			Provider:        &serveProvider,
			Debug:           &serveDebug,
			Search:          &serveSearch,
			NativeSearch:    &serveNativeSearch,
			NoNativeSearch:  &serveNoNativeSearch,
			MCP:             &serveMCP,
			MaxTurns:        &serveMaxTurns,
			MaxTurnsDefault: 200,
			Tools:           &serveTools,
			ReadDirs:        &serveReadDirs,
			WriteDirs:       &serveWriteDirs,
			ShellAllow:      &serveShellAllow,
			SystemMessage:   &serveSystemMessage,
			Agent:           &serveAgent,
			Yolo:            &serveYolo,
		})
}

func servePlatformCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	platforms := []string{
		"web\tHTTP server with chat UI",
		"api\tHTTP server with API endpoints only (no UI)",
		"jobs\tAsync job runner with HTTP management API",
		"telegram\tTelegram bot",
	}

	// Filter out already-selected platforms
	selected := make(map[string]bool, len(args))
	for _, a := range args {
		selected[a] = true
	}
	var completions []string
	for _, p := range platforms {
		name := strings.SplitN(p, "\t", 2)[0]
		if !selected[name] && strings.HasPrefix(name, toComplete) {
			completions = append(completions, p)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func runServe(cmd *cobra.Command, args []string) error {
	if servePort <= 0 || servePort > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", servePort)
	}
	if serveSessionTTL <= 0 {
		return fmt.Errorf("invalid --session-ttl %s (must be > 0)", serveSessionTTL)
	}
	if serveSessionMax <= 0 {
		return fmt.Errorf("invalid --session-max %d (must be > 0)", serveSessionMax)
	}
	if serveTelegramCarryoverChars < 0 {
		return fmt.Errorf("invalid --telegram-carryover-chars %d (must be >= 0)", serveTelegramCarryoverChars)
	}
	if serveJobsWorkers <= 0 {
		return fmt.Errorf("invalid --jobs-workers %d (must be > 0)", serveJobsWorkers)
	}
	if cmd.Flags().Changed("response-timeout") && serveResponseTimeout <= 0 {
		return fmt.Errorf("invalid --response-timeout %s (must be > 0)", serveResponseTimeout)
	}
	sidebarSessions, err := parseSidebarSessionCategories(serveSidebarSessions, true)
	if err != nil {
		return err
	}

	authMode, err := resolveServeAuthMode(cmd.Flags().Changed("auth"), serveAuthMode, cmd.Flags().Changed("no-auth") || cmd.Flags().Changed("allow-no-auth"), serveAllowNoAuth)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if !requireAuth && !isLoopbackHost(serveHost) {
		return fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", serveHost)
	}

	token := strings.TrimSpace(serveToken)
	if requireAuth && token == "" {
		generated, err := generateServeToken()
		if err != nil {
			return fmt.Errorf("generate auth token: %w", err)
		}
		token = generated
	}

	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	platformNames, err := resolvePlatforms(args, cfg.Serve.Platforms)
	if err != nil {
		return err
	}
	hasJobs := platformContains(platformNames, "jobs")
	hasWeb := platformContains(platformNames, "web")
	hasAPI := platformContains(platformNames, "api")
	hasTelegram := platformContains(platformNames, "telegram")

	// Auto-generate VAPID keys for web push if not already configured.
	if hasWeb && (cfg.Serve.WebPush.VAPIDPublicKey == "" || cfg.Serve.WebPush.VAPIDPrivateKey == "") {
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
	if !cmd.Flags().Changed("base-path") && cfg.Serve.BasePath != "" {
		serveBasePath = cfg.Serve.BasePath
	}
	serveBasePath, err = normalizeBasePath(serveBasePath)
	if err != nil {
		return err
	}

	responseTimeout, err := resolveServeResponseTimeout(cmd.Flags().Changed("response-timeout"), serveResponseTimeout, cfg.Serve.ResponseTimeout)
	if err != nil {
		return err
	}

	var agent *agents.Agent
	if hasWeb || hasAPI || hasTelegram {
		agent, err = LoadAgent(serveAgent, cfg)
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
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, serveProvider, agentProvider, agentModel); err != nil {
		return err
	}

	settings, err := ResolveSettings(cfg, agent, CLIFlags{
		Provider:      serveProvider,
		Tools:         serveTools,
		ReadDirs:      serveReadDirs,
		WriteDirs:     serveWriteDirs,
		ShellAllow:    serveShellAllow,
		MCP:           serveMCP,
		SystemMessage: serveSystemMessage,
		MaxTurns:      serveMaxTurns,
		MaxTurnsSet:   cmd.Flags().Changed("max-turns"),
		Search:        serveSearch,
		Platform:      singleServeTemplatePlatform(platformNames),
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 20)
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
	skillsSetup := SetupSkills(&cfg.Skills, "", agentSkills, cmd.ErrOrStderr())
	settings.SystemPrompt = InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)

	agentName := ""
	var agentPlatformMsgs agents.PlatformMessagesConfig
	if agent != nil {
		agentName = agent.Name
		agentPlatformMsgs = agent.PlatformMessages
	}

	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	defer storeCleanup()
	if store != nil {
		store = session.NewLoggingStore(store, func(format string, args ...any) {
			log.Printf("[serve] "+format, args...)
		})
	}

	forceExternalSearch := resolveForceExternalSearch(cfg, serveNativeSearch, serveNoNativeSearch)

	// Parse --tool-map entries ("ClientName:ServerName")
	var toolMap map[string]string
	for _, entry := range serveToolMap {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid --tool-map %q (expected ClientName:ServerName)", entry)
		}
		if toolMap == nil {
			toolMap = make(map[string]string)
		}
		toolMap[parts[0]] = parts[1]
	}

	modelName := activeModel(cfg)
	runtimeFactory := func(ctx context.Context, providerName string, providerModel string) (*serveRuntime, error) {
		var provider llm.Provider
		var err error
		provKey := cfg.DefaultProvider
		rtModelName := modelName

		if providerName != "" && (providerName != cfg.DefaultProvider || providerModel != "") {
			provider, err = llm.NewProviderByName(cfg, providerName, providerModel)
			provKey = providerName
			if providerModel != "" {
				rtModelName = providerModel
			} else {
				// Use provider's configured model or first curated model
				if pc, ok := cfg.Providers[providerName]; ok && pc.Model != "" {
					rtModelName = pc.Model
				} else if models := llm.ResolveProviderModelIDs(providerName); len(models) > 0 {
					rtModelName = models[0]
				}
			}
		} else {
			provider, err = llm.NewProvider(cfg)
			if providerModel != "" {
				rtModelName = providerModel
			}
		}
		if err != nil {
			return nil, err
		}

		engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, provKey, rtModelName, serveYolo, WireSpawnAgentRunner, skillsSetup)
		if err != nil {
			return nil, err
		}

		var mcpManager *mcp.Manager
		if settings.MCP != "" {
			mcpOpts := &MCPOptions{Provider: provider, Model: rtModelName, YoloMode: serveYolo}
			mgr, err := enableMCPServersWithFeedback(ctx, settings.MCP, engine, io.Discard, mcpOpts)
			if err != nil {
				return nil, err
			}
			mcpManager = mgr
		}

		// Validate --tool-map targets exist as registered server tools.
		// This runs after MCP registration so mapped MCP tools are visible.
		for clientName, serverName := range toolMap {
			if _, ok := engine.Tools().Get(serverName); !ok {
				names := make([]string, 0)
				for _, spec := range engine.Tools().AllSpecs() {
					names = append(names, spec.Name)
				}
				return nil, fmt.Errorf("--tool-map %s:%s: server tool %q not found (registered tools: %v)", clientName, serverName, serverName, names)
			}
		}

		runtime := &serveRuntime{
			provider:            provider,
			providerKey:         provKey,
			engine:              engine,
			toolMgr:             toolMgr,
			mcpManager:          mcpManager,
			systemPrompt:        settings.SystemPrompt,
			search:              settings.Search,
			forceExternalSearch: forceExternalSearch,
			maxTurns:            settings.MaxTurns,
			toolMap:             toolMap,
			debug:               serveDebug,
			debugRaw:            debugRaw,
			autoCompact:         cfg.AutoCompact,
			defaultModel:        rtModelName,
			store:               store,
			toolsSetting:        settings.Tools,
			mcpSetting:          settings.MCP,
			agentName:           agentName,
			platform:            "web",
			platformMessages:    agentPlatformMsgs,
		}
		if toolMgr != nil {
			imageBaseURL := ""
			if hasWeb {
				imageBaseURL = strings.TrimRight(serveBasePath, "/") + "/images/"
			}
			runtime.toolMgr.Registry.SetServeMode(true, imageBaseURL)
			if !serveYolo {
				runtime.toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
				runtime.toolMgr.ApprovalMgr.DebugApproval = serveDebug
				runtime.toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
					return runtime.awaitApproval(path, isWrite, isShell, workDir)
				}
			}
		}
		runtime.Touch()
		return runtime, nil
	}

	factory := func(ctx context.Context) (*serveRuntime, error) {
		return runtimeFactory(ctx, "", "")
	}
	sessionMgr := newServeSessionManager(serveSessionTTL, serveSessionMax, factory)
	defer sessionMgr.Close()

	if hasJobs && strings.TrimSpace(serveAgent) != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: --agent is ignored for --platform jobs; set llm runner_config.agent_name per job definition")
	}

	// Build the serve.Settings used by non-web platforms.
	serveSettings := serve.Settings{
		SystemPrompt:           settings.SystemPrompt,
		IdleTimeout:            serveSessionTTL,
		TelegramCarryoverChars: serveTelegramCarryoverChars,
		MaxTurns:               settings.MaxTurns,
		Debug:                  serveDebug,
		DebugRaw:               debugRaw,
		Search:                 settings.Search,
		ForceExternalSearch:    forceExternalSearch,
		Tools:                  settings.Tools,
		MCP:                    settings.MCP,
		Agent:                  agentName,
		PlatformMessages:       agentPlatformMsgs,
		Store:                  store,
		NewSession: func(ctx context.Context) (*serve.SessionRuntime, error) {
			rt, err := factory(ctx)
			if err != nil {
				return nil, err
			}
			return &serve.SessionRuntime{
				Engine:       rt.engine,
				ProviderName: rt.provider.Name(),
				ModelName:    rt.defaultModel,
				Cleanup:      rt.Close,
			}, nil
		},
	}

	// Instantiate non-web platforms.
	var platforms []serve.Platform
	for _, name := range platformNames {
		switch name {
		case "web":
			// Handled by the existing serveServer below.
		case "api":
			// Handled by the HTTP serveServer below (no UI).
		case "jobs":
			// Handled by the HTTP serveServer below.
		case "telegram":
			platforms = append(platforms, serve.NewTelegramPlatform(cfg.Serve.Telegram))
		default:
			return fmt.Errorf("unknown platform: %s", name)
		}
	}

	// Run setup wizard for platforms that need it (or --setup flag).
	for _, p := range platforms {
		if serveSetup || p.NeedsSetup() {
			if err := p.RunSetup(); err != nil {
				return fmt.Errorf("setup %s: %w", p.Name(), err)
			}
		}
	}

	hasHTTP := hasWeb || hasAPI || hasJobs

	var s *serveServer
	if hasHTTP {
		var jobsV2 *jobsV2Manager
		if hasJobs {
			jobsV2, err = newServeJobsV2Manager(cfg, serveJobsWorkers)
			if err != nil {
				return fmt.Errorf("initialize jobs v2 manager: %w", err)
			}
		}
		serveUI := hasWeb

		var widgetsMgr *widgets.Manager
		if serveEnableWidgets && (hasWeb || hasAPI) {
			wDir, wErr := resolveWidgetsDir(serveWidgetsDir, cfg)
			if wErr != nil {
				return wErr
			}
			widgetsMgr = widgets.NewManager(wDir, serveBasePath)
			log.Printf("widgets enabled, dir: %s", wDir)
		}

		s = &serveServer{
			cfg: serveServerConfig{
				host:                serveHost,
				port:                servePort,
				requireAuth:         requireAuth,
				token:               token,
				ui:                  serveUI,
				api:                 hasAPI,
				suppressServerTools: serveFilterServerTools,
				verbose:             serveVerbose,
				basePath:            serveBasePath,
				sidebarSessions:     append([]string(nil), sidebarSessions...),
				corsOrigins:         append([]string(nil), serveCORSOrigins...),
				filesDir:            resolveFilesDir(serveFilesDir, cfg),
				writeDirs:           resolveServeWriteDirs(serveWriteDirs, cfg),
				enableWidgets:       serveEnableWidgets,
				widgetsDir:          serveWidgetsDir,
				responseTimeout:     responseTimeout,
			},
			sessionMgr:     sessionMgr,
			jobsV2:         jobsV2,
			cfgRef:         cfg,
			store:          store,
			runtimeFactory: runtimeFactory,
			widgetsMgr:     widgetsMgr,
		}
		sessionMgr.onEvict = func(rt *serveRuntime) {
			for _, rid := range rt.getResponseIDs() {
				s.responseToSession.Delete(rid)
			}
		}

		if hasWeb {
			s.webrtcHeadSnippet = webrtcHTMLSnippet()
			runWebRTCPeer(ctx, s)
		}

		if err := s.Start(); err != nil {
			return err
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "term-llm serve listening on http://%s:%d\n", serveHost, servePort)
		fmt.Fprintf(cmd.ErrOrStderr(), "auth: %s\n", authSummary(requireAuth))
		if requireAuth {
			fmt.Fprintf(cmd.ErrOrStderr(), "token: %s\n", token)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "ui: %v\n", s.cfg.ui)
		if hasJobs {
			fmt.Fprintf(cmd.ErrOrStderr(), "jobs workers: %d\n", serveJobsWorkers)
		}
		if modelName != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "model: %s\n", modelName)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "response timeout: %s\n", humanDuration(responseTimeout))
	}

	// Start non-web platforms concurrently.
	var wg sync.WaitGroup
	for _, p := range platforms {
		wg.Add(1)
		go func(p serve.Platform) {
			defer wg.Done()
			if err := p.Run(ctx, cfg, serveSettings); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[%s] error: %v", p.Name(), err)
			}
		}(p)
	}

	<-ctx.Done()

	if s != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Stop(shutdownCtx)
	}

	wg.Wait()
	return nil
}

// newServeEngineWithTools creates a new engine with tools wired up for serving.
// skillsSetup must be pre-initialized by the caller (e.g. via SetupSkills); this
// function registers the activate_skill tool on the engine but does NOT inject
// <available_skills> metadata into the system prompt — the caller must do that
// before constructing serveRuntime/serveSettings so the mutation is not lost.
func newServeEngineWithTools(cfg *config.Config, settings SessionSettings, provider llm.Provider, providerName, modelName string, yoloMode bool, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*llm.Engine, *tools.ToolManager, error) {
	engine := newEngine(provider, cfg)

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return nil, nil, err
	}
	if toolMgr != nil {
		if yoloMode {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		}
		if wireSpawn != nil {
			if err := wireSpawn(cfg, toolMgr, yoloMode); err != nil {
				return nil, nil, err
			}
		}
	}

	// Serve runtimes need the same context tracking/auto-compaction setup as ask/TUI
	// so long-lived sessions can warn or compact before hitting provider limits.
	engine.ConfigureContextManagement(provider, providerName, modelName, cfg.AutoCompact)

	// Register the activate_skill tool on the engine. Metadata injection into the
	// system prompt is handled by the caller to avoid the by-value settings copy trap.
	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	return engine, toolMgr, nil
}

var knownPlatforms = map[string]bool{"web": true, "api": true, "jobs": true, "telegram": true}

// resolvePlatforms returns the list of platforms to serve. Positional args
// take precedence; if none are given, configPlatforms (from config.yaml
// serve.platforms) is used as fallback.
func resolvePlatforms(args []string, configPlatforms []string) ([]string, error) {
	var raw []string
	if len(args) > 0 {
		raw = args
	} else if len(configPlatforms) > 0 {
		raw = configPlatforms
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no platforms specified\n\nUsage: term-llm serve <platform> [platform...]\n\nExamples:\n  term-llm serve web\n  term-llm serve telegram web\n\nOr set serve.platforms in config.yaml")
	}

	seen := make(map[string]bool)
	var out []string
	for _, a := range raw {
		p := strings.TrimSpace(strings.ToLower(a))
		if p == "" {
			continue
		}
		if !knownPlatforms[p] {
			return nil, fmt.Errorf("unknown platform %q (valid: web, api, jobs, telegram)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platforms specified\n\nUsage: term-llm serve <platform> [platform...]\n\nExamples:\n  term-llm serve web\n  term-llm serve telegram web\n\nOr set serve.platforms in config.yaml")
	}
	return out, nil
}

func platformContains(platforms []string, name string) bool {
	for _, p := range platforms {
		if p == name {
			return true
		}
	}
	return false
}

// singleServeTemplatePlatform returns a stable platform token for serve prompts.
// If multiple runtime surfaces are selected (for example web+telegram), returns
// empty so {{platform}} stays unexpanded and is not misleading.
func singleServeTemplatePlatform(platforms []string) string {
	unique := make(map[string]struct{})
	for _, p := range platforms {
		switch p {
		case "web", "api", "telegram", "jobs":
			unique[p] = struct{}{}
		}
	}
	if len(unique) != 1 {
		return ""
	}
	for p := range unique {
		return p
	}
	return ""
}

func authSummary(required bool) string {
	if required {
		return "bearer required"
	}
	return "disabled"
}

var validSidebarSessionCategories = map[string]bool{
	"all":  true,
	"chat": true,
	"web":  true,
	"ask":  true,
	"exec": true,
}

func parseSidebarSessionCategories(raw string, defaultAll bool) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if defaultAll {
			return []string{"all"}, nil
		}
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	categories := make([]string, 0, len(parts))
	for _, part := range parts {
		category := strings.ToLower(strings.TrimSpace(part))
		if category == "" {
			continue
		}
		if !validSidebarSessionCategories[category] {
			return nil, fmt.Errorf("invalid --sidebar-sessions value %q (valid: all, chat, web, ask, plan, exec)", category)
		}
		if category == "all" {
			return []string{"all"}, nil
		}
		if !seen[category] {
			seen[category] = true
			categories = append(categories, category)
		}
	}
	if len(categories) == 0 {
		if defaultAll {
			return []string{"all"}, nil
		}
		return nil, nil
	}
	return categories, nil
}

func resolveServeAuthMode(authFlagSet bool, authMode string, allowNoAuthSet bool, allowNoAuth bool) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(authMode))
	if mode == "" {
		mode = "bearer"
	}
	if mode != "bearer" && mode != "none" {
		return "", fmt.Errorf("invalid --auth %q (must be bearer or none)", authMode)
	}

	if allowNoAuthSet {
		aliasMode := "bearer"
		if allowNoAuth {
			aliasMode = "none"
		}
		if authFlagSet && mode != aliasMode {
			return "", fmt.Errorf("--auth %s conflicts with --no-auth=%v", mode, allowNoAuth)
		}
		mode = aliasMode
	}

	return mode, nil
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func generateServeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type serveServerConfig struct {
	host                string
	port                int
	requireAuth         bool
	token               string
	ui                  bool
	api                 bool
	suppressServerTools bool
	verbose             bool
	basePath            string // e.g. "/ui" or "/chat", always without trailing slash
	sidebarSessions     []string
	corsOrigins         []string
	filesDir            string   // opt-in directory for serving arbitrary files (videos, PDFs, etc)
	writeDirs           []string // tool write-dirs (CLI + config); tool-reported files inside these are trusted sources for ensureFileServeable
	enableWidgets       bool
	widgetsDir          string
	responseTimeout     time.Duration
}

// uiRoute returns the base-path with trailing slash, e.g. "/ui/" or "/chat/".
func (c serveServerConfig) uiRoute() string { return c.basePath + "/" }

// imagesRoute returns the images sub-route, e.g. "/ui/images/" or "/chat/images/".
func (c serveServerConfig) imagesRoute() string { return c.basePath + "/images/" }

// filesRoute returns the files sub-route, e.g. "/ui/files/" or "/chat/files/".
func (c serveServerConfig) filesRoute() string { return c.basePath + "/files/" }

// resolveFilesDir returns the files-dir from the flag if set, otherwise from config.
func resolveFilesDir(flagVal string, cfg *config.Config) string {
	if flagVal != "" {
		return flagVal
	}
	return cfg.Serve.FilesDir
}

// resolveWidgetsDir returns the widgets directory, defaulting to ~/.config/term-llm/widgets.
func resolveWidgetsDir(flagVal string, cfg *config.Config) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if cfg.Serve.WidgetsDir != "" {
		return cfg.Serve.WidgetsDir, nil
	}
	cfgDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve widgets dir: %w", err)
	}
	return cfgDir + "/widgets", nil
}

func resolveServeResponseTimeout(flagSet bool, flagVal time.Duration, configVal string) (time.Duration, error) {
	if flagSet {
		if flagVal <= 0 {
			return 0, fmt.Errorf("invalid --response-timeout %s (must be > 0)", flagVal)
		}
		return flagVal, nil
	}
	if strings.TrimSpace(configVal) == "" {
		return defaultServeRequestTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(configVal))
	if err != nil {
		return 0, fmt.Errorf("invalid serve.response_timeout %q (use a Go duration like 30m or 1h): %w", configVal, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("invalid serve.response_timeout %q (must be > 0)", configVal)
	}
	return timeout, nil
}

func (s *serveServer) responseTimeout() time.Duration {
	if s == nil || s.cfg.responseTimeout <= 0 {
		return defaultServeRequestTimeout
	}
	return s.cfg.responseTimeout
}

// resolveServeWriteDirs returns the merged effective write-dirs for the serve runtime,
// preserving order and de-duplicating.
func resolveServeWriteDirs(cliWriteDirs []string, cfg *config.Config) []string {
	seen := make(map[string]struct{}, len(cliWriteDirs)+len(cfg.Tools.WriteDirs))
	var out []string
	for _, d := range cfg.Tools.WriteDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	for _, d := range cliWriteDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// normalizeBasePath validates and normalizes a base-path value.
// It ensures a leading slash, strips trailing slashes, and rejects
// empty or root-only paths (use the default "/ui" instead of "/").
func normalizeBasePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("--base-path must not be empty")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", fmt.Errorf("--base-path must not be \"/\" (use the default /ui or a sub-path like /chat)")
	}
	return p, nil
}

type serveServer struct {
	cfg               serveServerConfig
	sessionMgr        *serveSessionManager
	jobsV2            *jobsV2Manager
	cfgRef            *config.Config
	store             session.Store
	server            *http.Server
	shutdownCh        chan struct{}
	shutdownOnce      sync.Once
	modelsMu          sync.Mutex
	modelsProviders   map[string]llm.Provider // keyed by provider name
	responseToSession sync.Map                // response_id (string) → session_id (string)
	sessionToResponse sync.Map                // session_id (string) → latest response_id (string)
	responseRunsOnce  sync.Once
	responseRuns      *responseRunManager
	webrtcHeadSnippet string // injected into index.html <head>; empty when WebRTC disabled
	runtimeFactory    func(ctx context.Context, providerName string, model string) (*serveRuntime, error)
	widgetsMgr        *widgets.Manager
}

func (s *serveServer) Start() error {
	s.shutdownCh = make(chan struct{})
	s.shutdownOnce = sync.Once{}
	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.cfg.host, s.cfg.port),
		Handler: s.httpHandler(),
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("start server: %w", err)
		}
		return nil
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func (s *serveServer) httpHandler() http.Handler {
	// Inner mux: all routes registered at their natural paths.
	// basePath is stripped by http.StripPrefix on the outer mux when mounted,
	// so handlers see /v1/..., /images/..., / etc. without the prefix.
	inner := http.NewServeMux()

	inner.HandleFunc("/healthz", s.handleHealth)
	inner.HandleFunc("/v1/providers", s.auth(s.cors(s.handleProviders)))
	inner.HandleFunc("/v1/models", s.auth(s.cors(s.handleModels)))
	inner.HandleFunc("/v1/responses", s.auth(s.cors(s.handleResponses)))
	inner.HandleFunc("/v1/responses/", s.auth(s.cors(s.handleResponseByID)))
	inner.HandleFunc("/v1/chat/completions", s.auth(s.cors(s.handleChatCompletions)))
	inner.HandleFunc("/v1/messages", s.auth(s.cors(s.handleAnthropicMessages)))
	inner.HandleFunc("/v1/transcribe", s.auth(s.cors(s.handleTranscribe)))
	if s.jobsV2 != nil {
		inner.HandleFunc("/v2/jobs", s.auth(s.cors(s.handleJobsV2)))
		inner.HandleFunc("/v2/jobs/", s.auth(s.cors(s.handleJobV2ByID)))
		inner.HandleFunc("/v2/runs", s.auth(s.cors(s.handleRunsV2)))
		inner.HandleFunc("/v2/runs/", s.auth(s.cors(s.handleRunV2ByID)))
	}

	inner.HandleFunc("/images/", s.auth(s.cors(s.handleImage)))
	if s.cfg.filesDir != "" {
		inner.HandleFunc("/files/", s.auth(s.cors(s.handleFile)))
	}
	if s.widgetsMgr != nil {
		s.registerWidgetRoutes(inner)
	}
	inner.HandleFunc("/v1/sessions/status", s.auth(s.cors(s.handleSessionsStatus)))
	inner.HandleFunc("/v1/sessions/", s.auth(s.cors(s.handleSessionByID)))
	inner.HandleFunc("/v1/push/subscribe", s.auth(s.cors(s.handlePushSubscribe)))

	if s.store != nil {
		inner.HandleFunc("/v1/sessions", s.auth(s.cors(s.handleSessions)))
	}

	if s.cfg.ui {
		inner.HandleFunc("/", s.cors(s.handleUI)) // catch-all SPA
	}

	// Jobs-only serve instances have no UI surface, so mount at root and keep
	// the canonical /v2/* API paths. The shared base-path wrapper is still used
	// for web/UI surfaces where the browser and API must live under one prefix.
	if s.jobsV2 != nil && !s.cfg.ui && !s.cfg.api {
		return inner
	}

	// Outer mux: mount everything under basePath.
	// Requests to basePath/ are handled by StripPrefix → inner.
	// Go's ServeMux auto-redirects basePath (no slash) → basePath/.
	prefix := s.cfg.basePath
	mux := http.NewServeMux()
	mux.Handle(prefix+"/", http.StripPrefix(prefix, inner))

	if s.cfg.ui {
		mux.HandleFunc("/", s.cors(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
		}))
	}

	return mux
}

// contextWithShutdown returns a derived context that is cancelled when either
// the parent context is done or shutdownCh is closed. This lets streaming
// handlers exit promptly on server shutdown rather than holding server.Shutdown
// open until the full timeout expires.
func (s *serveServer) contextWithShutdown(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if s.shutdownCh == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-s.shutdownCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *serveServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	// Signal all SSE handlers to return immediately so server.Shutdown
	// does not block waiting for long-lived streaming connections.
	s.shutdownOnce.Do(func() {
		if s.shutdownCh != nil {
			close(s.shutdownCh)
		}
	})
	if s.jobsV2 != nil {
		_ = s.jobsV2.Close()
	}
	if s.responseRuns != nil {
		s.responseRuns.Close()
	}
	if s.widgetsMgr != nil {
		s.widgetsMgr.Close()
	}
	s.modelsMu.Lock()
	for _, p := range s.modelsProviders {
		if cleaner, ok := p.(interface{ CleanupMCP() }); ok {
			cleaner.CleanupMCP()
		}
	}
	s.modelsProviders = nil
	s.modelsMu.Unlock()
	return s.server.Shutdown(ctx)
}
