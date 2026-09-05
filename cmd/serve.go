package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mentions"
	projectpkg "github.com/samsaffron/term-llm/internal/project"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/serve"
	servehttp "github.com/samsaffron/term-llm/internal/serve/http"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	webrtcpkg "github.com/samsaffron/term-llm/internal/webrtc"
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
	servePublicURL              string
	serveTitle                  string
	serveDisableLocationSharing bool
	serveCORSOrigins            []string
	serveSessionTTL             time.Duration
	serveSessionMax             int
	serveDebug                  bool
	serveSearch                 bool
	serveNoSearch               bool
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
	serveApproval               string
	serveYolo                   bool
	serveAuto                   bool
	serveTelegramCarryoverChars int
	serveJobsWorkers            int
	serveSetup                  bool
	serveVerbose                bool
	serveFilterServerTools      bool
	serveSidebarSessions        string
	serveToolMap                []string
	serveFilesDir               string
	serveDisableWidgets         bool
	serveWidgetsDir             string
	serveResponseTimeout        time.Duration
	serveEnableFileTracking     bool
	serveHubURL                 string
	serveHubNodeID              string
	serveHubNodeName            string
	serveHubConnect             string
	serveHubRegister            bool
	serveHubRegistrationToken   string
	serveProjects               bool
	serveNoProjects             bool
)

const (
	servePlatformInitialRetryDelay = time.Second
	servePlatformMaxRetryDelay     = 30 * time.Second
	servePlatformRetryResetAfter   = time.Minute
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
serve.platforms list from config.yaml is used. Ordinary serve runs default to
Guardian-reviewed auto approval. Use --approval prompt to require human approval
for unmatched actions. The separate "serve mcp" command remains prompt by default.

Examples:
  term-llm serve web             # web server with UI enabled
  term-llm serve web --approval prompt # require approval for unmatched actions
  term-llm serve api             # API only (no chat UI)
  term-llm serve telegram        # Telegram bot only
  term-llm serve telegram web    # both platforms
  term-llm serve web --base-path /chat
  term-llm serve web --title "My Lab"

All HTTP routes are mounted under --base-path (default /ui):
  POST {base}/v1/responses
  POST {base}/v1/chat/completions
  POST {base}/v1/messages
  POST {base}/v1/transcribe
  GET  {base}/v1/models
  GET  {base}/healthz
  GET  {base}/                       (web UI)
  GET  {base}/images/:file
  GET  {base}/media/:file

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
	serveCmd.Flags().StringVar(&serveToken, "token", "", "Bearer token for API auth (defaults to $TERM_LLM_SERVE_TOKEN, else auto-generated)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "no-auth", false, "Disable auth (only allowed on loopback host)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "allow-no-auth", false, "Disable auth (alias for --no-auth)")
	_ = serveCmd.Flags().MarkHidden("allow-no-auth")
	serveCmd.Flags().StringVar(&serveAuthMode, "auth", "bearer", "Auth mode: bearer or none")
	serveCmd.Flags().StringVar(&serveBasePath, "base-path", "/ui", "URL prefix the UI uses for session URLs (e.g. /chat)")
	serveCmd.Flags().StringVar(&servePublicURL, "public-url", "", "Browser-visible URL for OAuth callbacks (defaults to $TERM_LLM_SERVE_PUBLIC_URL or the authenticated request origin)")
	serveCmd.Flags().StringVar(&serveTitle, "title", "", "Override the web UI sidebar title (defaults to agent name or Chat)")
	serveCmd.Flags().BoolVar(&serveDisableLocationSharing, "disable-location-sharing", false, "Hide the web UI action for sharing the browser's current location")
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
	serveCmd.Flags().Bool("enable-widgets", false, "Deprecated no-op; widgets are enabled by default")
	serveCmd.Flags().BoolVar(&serveDisableWidgets, "disable-widgets", false, "Disable local widget apps under {base}/widgets/<mount>/")
	serveCmd.Flags().StringVar(&serveWidgetsDir, "widgets-dir", "", "Directory containing widget sub-directories (default: ~/.config/term-llm/widgets)")
	serveCmd.Flags().DurationVar(&serveResponseTimeout, "response-timeout", defaultServeRequestTimeout, "Maximum inactivity before the first or next completed LLM response (default 30m; pauses for interactive waits)")
	serveCmd.Flags().BoolVar(&serveEnableFileTracking, "enable-file-tracking", false, "Enable session file-change tracking for this serve process")
	serveCmd.Flags().BoolVar(&serveProjects, "projects", false, "Strictly enable Web UI project selection")
	serveCmd.Flags().BoolVar(&serveNoProjects, "no-projects", false, "Use the single-workspace Web UI for dedicated agent deployments")
	serveCmd.Flags().StringVar(&serveHubURL, "hub-url", "", "URL of the term-llm Hub this node belongs to (renders a Back to Hub link in the web UI)")
	serveCmd.Flags().StringVar(&serveHubNodeID, "hub-node-id", "", "This node's id on the hub (used with --hub-url)")
	serveCmd.Flags().StringVar(&serveHubNodeName, "hub-node-name", "", "This node's display name on the hub (used with --hub-url)")
	serveCmd.Flags().StringVar(&serveHubConnect, "hub-connect", "direct", "Hub connection mode for this node: direct or reverse")
	serveCmd.Flags().BoolVar(&serveHubRegister, "hub-register", false, "Register this reverse node with the Hub before connecting")
	serveCmd.Flags().StringVar(&serveHubRegistrationToken, "hub-registration-token", "", "Hub registration token for --hub-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN)")

	AddCommonFlags(serveCmd,
		CommonCoreFlags|CommonSearch|CommonNativeSearch|CommonMaxTurns|CommonAgent,
		CommonFlagBindings{
			Provider:        &serveProvider,
			Debug:           &serveDebug,
			Search:          &serveSearch,
			NoSearch:        &serveNoSearch,
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
			Approval:        &serveApproval,
			Yolo:            &serveYolo,
			Auto:            &serveAuto,
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
	warnDeprecatedEnableWidgets(cmd)

	svc, err := serve.NewService(serve.Options{
		Host:      serveHost,
		Port:      servePort,
		BasePath:  serveBasePath,
		Title:     serveTitle,
		Platforms: append([]string(nil), args...),
		Runner: func(ctx context.Context) error {
			return runServeLegacy(ctx, cmd, args)
		},
	})
	if err != nil {
		return err
	}
	return svc.Run(cmd.Context())
}

func warnDeprecatedEnableWidgets(cmd *cobra.Command) {
	if cmd.Flags().Changed("enable-widgets") {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning: --enable-widgets is deprecated and has no effect; widgets are enabled by default. Use --disable-widgets to opt out.")
	}
}

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

func runServeLegacy(parentCtx context.Context, cmd *cobra.Command, args []string) error {
	startupDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve serve startup directory: %w", err)
	}
	if cmd.Flags().Changed("projects") && cmd.Flags().Changed("no-projects") {
		return fmt.Errorf("--projects and --no-projects cannot be used together")
	}
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

	token, tokenSource, err := resolveServeToken(serveToken, os.Getenv("TERM_LLM_SERVE_TOKEN"), requireAuth, generateServeToken)
	if err != nil {
		return err
	}

	serveHubConnect = strings.ToLower(strings.TrimSpace(serveHubConnect))
	if serveHubConnect == "" {
		serveHubConnect = "direct"
	}
	if serveHubConnect != "direct" && serveHubConnect != "reverse" {
		return fmt.Errorf("invalid --hub-connect %q (use direct or reverse)", serveHubConnect)
	}
	hubRegistrationToken := ""
	if serveHubRegister {
		hubRegistrationToken = resolveServeHubRegistrationToken(serveHubRegistrationToken)
	}

	// Hub-joined nodes: hand the hub context to in-process tools and jobs-v2
	// runs so hub_delegate/hub_check_delegation work without extra setup. The
	// node authenticates to the hub with its own serve token. Explicit
	// TERM_LLM_HUB_* env (captured and scrubbed at startup) wins — only gaps
	// are filled — and the token stays in process memory, never in the
	// process environment, browser-facing config, or injected HTML, so tool
	// subprocesses (shell/custom/widget/MCP) cannot inherit it.
	if hubURL, hubNodeID := strings.TrimSpace(serveHubURL), strings.TrimSpace(serveHubNodeID); hubURL != "" && hubNodeID != "" && token != "" {
		tools.ConfigureHubDelegation(hubURL, hubNodeID, token)
	}

	ctx, stop := signal.NotifyContextWithParent(parentCtx)
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	applyServeFileTrackingOverride(cfg, serveEnableFileTracking)
	resolvedApproval, err := resolveCommandApprovalMode(cmd, approvalSurfaceServe, cfg, nil, serveApproval, serveAuto, serveYolo)
	if err != nil {
		return err
	}
	resolvedYolo := resolvedApproval.Mode == tools.ModeYolo
	serveApproval = resolvedApproval.Mode.String()
	serveYolo = resolvedYolo
	serveAuto = resolvedApproval.Mode == tools.ModeAuto

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
	if strings.TrimSpace(servePublicURL) == "" {
		servePublicURL = strings.TrimSpace(os.Getenv("TERM_LLM_SERVE_PUBLIC_URL"))
	}
	servePublicURL, err = normalizeServePublicURL(servePublicURL)
	if err != nil {
		return fmt.Errorf("invalid serve --public-url: %w", err)
	}

	resolvedTitle := strings.TrimSpace(serveTitle)
	if !cmd.Flags().Changed("title") {
		resolvedTitle = strings.TrimSpace(cfg.Serve.Title)
	}
	locationSharingDisabled := serveDisableLocationSharing
	if !cmd.Flags().Changed("disable-location-sharing") {
		locationSharingDisabled = cfg.Serve.DisableLocationSharing
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
		NoSearch:      serveNoSearch,
		Platform:      singleServeTemplatePlatform(platformNames),
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
	effectiveSkillsConfig := applySkillsFlag(&cfg.Skills, agentSkills)
	baseSystemPrompt := settings.SystemPrompt
	skillsSetup := SetupSkills(effectiveSkillsConfig, "", "", cmd.ErrOrStderr())
	settings.SystemPrompt = InjectSkillsMetadata(baseSystemPrompt, skillsSetup)

	agentName := ""
	var availableAgentNames []string
	var agentPlatformMsgs agents.PlatformMessagesConfig
	if agent != nil {
		agentName = agent.Name
		agentPlatformMsgs = agent.PlatformMessages
	} else if hasWeb {
		availableAgentNames, err = ListAgentNames(cfg)
		if err != nil {
			return fmt.Errorf("list web agents: %w", err)
		}
	}

	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	defer storeCleanup()
	store = withServeSessionLogging(store)

	projectsRequested, projectsStrict := resolveServeProjectsRequested(
		cmd.Flags().Changed("projects"),
		serveProjects,
		cmd.Flags().Changed("no-projects") && serveNoProjects,
		cfg.Serve.Projects.Enabled,
		hasWeb,
	)
	projectsEnabled, bootstrapProjectID, err := initializeServeProjects(ctx, store, startupDir, projectsRequested, projectsStrict, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if hasWeb {
		switch {
		case projectsEnabled:
			projectCount := 0
			if projectStore, ok := session.AsProjectStore(store); ok {
				if projects, listErr := projectStore.ListProjects(ctx, session.ProjectListOptions{IncludeArchived: true}); listErr == nil {
					projectCount = len(projects)
				}
			}
			bootstrapCount := 0
			if bootstrapProjectID != "" {
				bootstrapCount = 1
			}
			log.Printf("projects enabled (projects=%d bootstrap=%d bootstrap_id=%s)", projectCount, bootstrapCount, bootstrapProjectID)
		case cmd.Flags().Changed("no-projects") || !cfg.Serve.Projects.Enabled:
			log.Printf("projects explicitly disabled")
		default:
			log.Printf("projects auto-disabled")
		}
	}

	if projectsEnabled {
		go func() {
			reconcileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			claimed, reconcileErr := projectpkg.ReconcileAll(reconcileCtx, store)
			if reconcileErr != nil {
				log.Printf("project history reconciliation failed: %v", reconcileErr)
			} else if claimed > 0 {
				log.Printf("project history reconciled (claimed=%d)", claimed)
			}
		}()
	}

	forceExternalSearch := resolveForceExternalSearch(cfg, serveNativeSearch, serveNoNativeSearch)
	mediaPublisher, err := newServeMediaPublisher()
	if err != nil {
		return fmt.Errorf("initialize media publisher: %w", err)
	}

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
	if err := preflightHeadlessApproval(cfg, resolvedApproval); err != nil {
		return err
	}
	var approvalErrWriter io.Writer = io.Discard
	if serveDebug || serveVerbose {
		approvalErrWriter = cmd.ErrOrStderr()
	}
	var s *serveServer
	collaborationController := &serveCollaborativeShellController{manager: func() (*serveShellManager, error) {
		if s == nil {
			return nil, errServeShellClosed
		}
		return s.shellManager()
	}}
	agentRuntimeFactory := func(ctx context.Context, providerName string, providerModel string, requestedAgent string) (*serveRuntime, error) {
		runtimeAgent := strings.TrimSpace(serveAgent)
		if runtimeAgent == "" {
			runtimeAgent = strings.TrimSpace(requestedAgent)
		}
		runner := &cmdRunner{baseCfg: cfg, defaults: cmdRunnerOptions{
			Provider:            serveProvider,
			Tools:               serveTools,
			ReadDirs:            append([]string(nil), serveReadDirs...),
			WriteDirs:           append([]string(nil), serveWriteDirs...),
			ShellAllow:          append([]string(nil), serveShellAllow...),
			MCP:                 serveMCP,
			SystemMessage:       serveSystemMessage,
			MaxTurns:            serveMaxTurns,
			Search:              serveSearch,
			NoSearch:            serveNoSearch,
			NativeSearch:        serveNativeSearch,
			NoNativeSearch:      serveNoNativeSearch,
			ApprovalMode:        resolvedApproval.Mode,
			ApprovalModeSet:     true,
			ApprovalSource:      resolvedApproval.Source,
			ApprovalHeadless:    true,
			ApprovalPrepare:     true,
			ApprovalDiagnostics: serveVerbose,
			Debug:               serveDebug,
			DebugRaw:            debugRaw,
			ErrWriter:           approvalErrWriter,
			WireSpawn:           WireSpawnAgentRunner,
			Store:               store,
		}}
		env, err := runner.prepare(ctx, runpkg.Request{
			Platform:     runpkg.PlatformWeb,
			AgentName:    runtimeAgent,
			Provider:     strings.TrimSpace(providerName),
			Model:        strings.TrimSpace(providerModel),
			DeferSession: true,
		}, nil)
		if err != nil {
			return nil, err
		}
		runtime := env.runtime
		if s != nil && s.webExec != nil && (runtime.provider.Capabilities().InlineToolLoop || runtime.mcpManagerSnapshot() != nil) {
			s.webExec.reject("a runtime initialized native provider or MCP ownership")
		}
		runtime.toolMap = toolMap
		runtime.platform = "web"
		runtime.sideProviderFactory = func(providerKey, model string) (llm.Provider, error) {
			return llm.NewProviderByName(cfg, providerKey, model)
		}
		runtime.configureSideQuestionContext()

		// Validate --tool-map targets exist as registered server tools.
		// This runs after MCP registration so mapped MCP tools are visible.
		for clientName, serverName := range toolMap {
			if _, ok := runtime.engine.Tools().Get(serverName); !ok {
				names := make([]string, 0)
				for _, spec := range runtime.engine.Tools().AllSpecs() {
					names = append(names, spec.Name)
				}
				runtime.Close()
				return nil, fmt.Errorf("--tool-map %s:%s: server tool %q not found (registered tools: %v)", clientName, serverName, serverName, names)
			}
		}

		if runtime.toolMgr != nil {
			runtime.toolMgr.ApprovalMgr.GuardianEventFunc = runtime.emitGuardianReview
			imageBaseURL := ""
			if hasWeb {
				imageBaseURL = strings.TrimRight(serveBasePath, "/") + "/images/"
			}
			runtime.toolMgr.Registry.SetServeMode(true, imageBaseURL)
			runtime.toolMgr.Registry.SetMediaPublisher(mediaPublisher)
			if hasWeb {
				runtime.toolMgr.Registry.SetCollaborativeShellController(collaborationController, tools.ShellRoutingControllerRequired)
			} else {
				runtime.toolMgr.Registry.SetCollaborativeShellController(nil, tools.ShellRoutingLocalOnly)
			}
			if !resolvedYolo {
				runtime.toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
				runtime.toolMgr.ApprovalMgr.DebugApproval = serveDebug
				runtime.toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
					return runtime.awaitApproval(path, isWrite, isShell, workDir)
				}
				runtime.toolMgr.ApprovalMgr.SharedShellPromptUIFunc = runtime.awaitSharedShellApproval
			}
			if hasWeb {
				runtime.toolMgr.ApprovalMgr.WorkspacePromptFunc = runtime.awaitWorkspaceApproval
			}
		}
		runtime.Touch()
		return runtime, nil
	}

	runtimeFactory := func(ctx context.Context, providerName string, providerModel string) (*serveRuntime, error) {
		return agentRuntimeFactory(ctx, providerName, providerModel, "")
	}
	factory := func(ctx context.Context) (*serveRuntime, error) {
		return runtimeFactory(ctx, "", "")
	}
	sessionMgr := newServeSessionManager(serveSessionTTL, serveSessionMax, factory)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessionMgr.CloseContext(shutdownCtx)
	}()

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
		Runner: newCmdRunner(cfg, cmdRunnerOptions{
			Provider:            serveProvider,
			Tools:               serveTools,
			ReadDirs:            append([]string(nil), serveReadDirs...),
			WriteDirs:           append([]string(nil), serveWriteDirs...),
			ShellAllow:          append([]string(nil), serveShellAllow...),
			MCP:                 serveMCP,
			SystemMessage:       serveSystemMessage,
			MaxTurns:            serveMaxTurns,
			Search:              serveSearch,
			NoSearch:            serveNoSearch,
			NativeSearch:        serveNativeSearch,
			NoNativeSearch:      serveNoNativeSearch,
			ApprovalMode:        resolvedApproval.Mode,
			ApprovalModeSet:     true,
			ApprovalSource:      resolvedApproval.Source,
			ApprovalHeadless:    true,
			ApprovalDiagnostics: serveVerbose,
			Debug:               serveDebug,
			DebugRaw:            debugRaw,
			ErrWriter:           approvalErrWriter,
			WireSpawn:           WireSpawnAgentRunner,
			Store:               store,
		}),
		NewSession: func(ctx context.Context) (*serve.SessionRuntime, error) {
			rt, err := factory(ctx)
			if err != nil {
				return nil, err
			}
			return &serve.SessionRuntime{
				Engine:       rt.engine,
				Provider:     rt.provider,
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
	if !hasHTTP {
		defer installWebExecSignal(ctx, &webExecCoordinator{ctx: ctx, unsupported: "SIGUSR2 requires standalone serve web"})()
	}

	registeredHubURL := ""
	registeredHubNodeID := ""
	if hasHTTP {
		var jobsV2 *jobsV2Manager
		serveUI := hasWeb

		var widgetsMgr *widgets.Manager
		if serveWidgetsEnabled(hasWeb, serveDisableWidgets) {
			wDir, wErr := resolveWidgetsDir(serveWidgetsDir, cfg)
			if wErr != nil {
				return wErr
			}
			widgetsMgr = widgets.NewManager(wDir, serveBasePath)
			defer installWidgetStopSignal(ctx, widgetsMgr)()
			log.Printf("widgets enabled, dir: %s", wDir)
		}

		s = &serveServer{
			cfg: serveServerConfig{
				host:                    serveHost,
				port:                    servePort,
				requireAuth:             requireAuth,
				token:                   token,
				ui:                      serveUI,
				api:                     hasAPI,
				suppressServerTools:     serveFilterServerTools,
				verbose:                 serveVerbose,
				basePath:                serveBasePath,
				publicURL:               servePublicURL,
				uiTitle:                 resolvedTitle,
				locationSharingDisabled: locationSharingDisabled,
				sidebarSessions:         append([]string(nil), sidebarSessions...),
				agentName:               agentName,
				agentNames:              append([]string(nil), availableAgentNames...),
				corsOrigins:             append([]string(nil), serveCORSOrigins...),
				filesDir:                resolveFilesDir(serveFilesDir, cfg),
				writeDirs:               resolveServeWriteDirs(serveWriteDirs, cfg),
				responseTimeout:         responseTimeout,
				hubURL:                  strings.TrimSpace(serveHubURL),
				hubNodeID:               strings.TrimSpace(serveHubNodeID),
				hubNodeName:             strings.TrimSpace(serveHubNodeName),
			},
			sessionMgr:          sessionMgr,
			jobsV2:              jobsV2,
			cfgRef:              cfg,
			store:               store,
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
			shells: newServeShellManager(serveSessionTTL, func(sessionID string) bool {
				if store == nil {
					return false
				}
				sess, getErr := store.Get(context.Background(), sessionID)
				return getErr == nil && sess != nil
			}),
		}

		unsupportedExec := ""
		if !hasWeb || hasAPI || hasJobs || hasTelegram {
			unsupportedExec = "only standalone serve web supports SIGUSR2"
		}
		if widgetsMgr != nil {
			unsupportedExec = "widget ownership is unsupported; start with --disable-widgets"
		}
		if serveWebRTC || strings.TrimSpace(serveHubURL) != "" {
			unsupportedExec = "Hub and WebRTC transports are not yet supported"
		}
		s.webExec = newWebExecCoordinator(ctx, s, unsupportedExec)
		defer installWebExecSignal(ctx, s.webExec)()
		if hasJobs {
			jobsV2, err = newServeJobsV2Manager(cfg, serveJobsWorkers, resolvedApproval, s.notifyJobsV2RunDone)
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

		if hasWeb {
			s.webrtcEnabled = serveWebRTC
			s.webrtcHeadSnippet = webrtcHTMLSnippet()
			runWebRTCPeer(ctx, s)
		}

		reverseHubURL := strings.TrimSpace(serveHubURL)
		reverseHubNodeID := strings.TrimSpace(serveHubNodeID)
		if serveHubRegister {
			if serveHubConnect != "reverse" {
				return fmt.Errorf("--hub-register requires --hub-connect reverse")
			}
			if reverseHubURL == "" || reverseHubNodeID == "" || token == "" || hubRegistrationToken == "" {
				return fmt.Errorf("--hub-register requires --hub-url, --hub-node-id, --hub-registration-token, and a bearer --token")
			}
		}
		if serveHubConnect == "reverse" && (reverseHubURL == "" || reverseHubNodeID == "" || token == "") {
			return fmt.Errorf("--hub-connect reverse requires --hub-url, --hub-node-id, and a bearer --token")
		}

		if err := s.Start(); err != nil {
			return err
		}

		if serveHubRegister {
			if err := registerServeHubNode(ctx, nil, reverseHubURL, hubRegistrationToken, hubRegisterNodeRequest{
				ID:         reverseHubNodeID,
				Name:       strings.TrimSpace(serveHubNodeName),
				Connection: "reverse",
				BasePath:   serveBasePath,
				Token:      token,
			}); err != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = s.Stop(shutdownCtx)
				cancel()
				return err
			}
			registeredHubURL = reverseHubURL
			registeredHubNodeID = reverseHubNodeID
			fmt.Fprintf(cmd.ErrOrStderr(), "hub registration: registered %s with %s\n", reverseHubNodeID, reverseHubURL)
		}

		if serveHubConnect == "reverse" {
			localBase := localHubConnectBase(serveHost, servePort)
			connector := startHubReverseConnector(ctx, reverseHubURL, reverseHubNodeID, token, localBase, serveBasePath, newHubReverseLocalClient())
			defer func() {
				// Join ownership; a deadline must not authorize abandoning requests.
				_ = connector.Stop(context.Background())
			}()
			fmt.Fprintf(cmd.ErrOrStderr(), "hub reverse: connecting %s to %s\n", reverseHubNodeID, reverseHubURL)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "term-llm serve listening on http://%s:%d\n", serveHost, servePort)
		fmt.Fprintf(cmd.ErrOrStderr(), "auth: %s\n", authSummary(requireAuth))
		if requireAuth {
			switch tokenSource {
			case tokenSourceGenerated:
				fmt.Fprintf(cmd.ErrOrStderr(), "token: %s (auto-generated; export TERM_LLM_SERVE_TOKEN to persist)\n", token)
			case tokenSourceEnv:
				fmt.Fprintf(cmd.ErrOrStderr(), "token: %s (from $TERM_LLM_SERVE_TOKEN)\n", token)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "token: %s\n", token)
			}
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
			runPlatformSupervisor(ctx, cfg, serveSettings, p, servePlatformInitialRetryDelay)
		}(p)
	}

	<-ctx.Done()

	if registeredHubURL != "" && registeredHubNodeID != "" && hubRegistrationToken != "" {
		deregisterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := unregisterServeHubNode(deregisterCtx, nil, registeredHubURL, hubRegistrationToken, registeredHubNodeID); err != nil {
			log.Printf("hub registration: deregister %s from %s: %v", registeredHubNodeID, registeredHubURL, err)
		} else {
			log.Printf("hub registration: deregistered %s from %s", registeredHubNodeID, registeredHubURL)
		}
		cancel()
	}

	if s != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Stop(shutdownCtx)
	}

	wg.Wait()
	return nil
}

func runPlatformSupervisor(ctx context.Context, cfg *config.Config, settings serve.Settings, platform serve.Platform, initialRetryDelay time.Duration) {
	retryDelay := initialRetryDelay
	for {
		startedAt := time.Now()
		err := platform.Run(ctx, cfg, settings)
		if ctx.Err() != nil {
			return
		}

		if time.Since(startedAt) >= servePlatformRetryResetAfter {
			retryDelay = initialRetryDelay
		}
		if err != nil {
			log.Printf("[%s] error: %v; retrying in %s", platform.Name(), err, retryDelay)
		} else {
			log.Printf("[%s] stopped unexpectedly; retrying in %s", platform.Name(), retryDelay)
		}

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}

		if retryDelay < servePlatformMaxRetryDelay {
			retryDelay *= 2
			if retryDelay > servePlatformMaxRetryDelay {
				retryDelay = servePlatformMaxRetryDelay
			}
		}
	}
}

// newServeEngineWithTools creates a new engine with tools wired up for serving.
// skillsSetup must be pre-initialized by the caller (e.g. via SetupSkills); this
// function registers the activate_skill tool on the engine but does NOT inject
// <available_skills> metadata into the system prompt — the caller must do that
// before constructing serveRuntime/serveSettings so the mutation is not lost.
func newServeEngineWithTools(cfg *config.Config, settings SessionSettings, provider llm.Provider, providerName, modelName string, yoloMode bool, autoMode bool, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*llm.Engine, *tools.ToolManager, error) {
	mode := tools.ModePrompt
	if yoloMode {
		mode = tools.ModeYolo
	} else if autoMode {
		mode = tools.ModeAuto
	}
	return newServeEngineWithToolsMode(cfg, settings, provider, providerName, modelName, resolvedApprovalMode{Mode: mode, Source: approvalModeSourceCLI}, approvalRuntimeOptions{Headless: true}, wireSpawn, skillsSetup)
}

func newServeEngineWithToolsMode(cfg *config.Config, settings SessionSettings, provider llm.Provider, providerName, modelName string, resolved resolvedApprovalMode, runtimeOpts approvalRuntimeOptions, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*llm.Engine, *tools.ToolManager, error) {
	engine := newEngine(provider, cfg)
	settings.Provider = providerName
	settings.Model = modelName

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return nil, nil, err
	}
	if toolMgr != nil {
		if err := applyResolvedApprovalMode(cfg, toolMgr.ApprovalMgr, resolved, runtimeOpts); err != nil {
			return nil, nil, err
		}
		if wireSpawn != nil {
			if err := wireSpawn(cfg, toolMgr, resolved.Mode == tools.ModeYolo); err != nil {
				toolMgr.ApprovalMgr.Close()
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

// resolvePlatforms returns the list of platforms to serve. Positional args
// take precedence; if none are given, configPlatforms (from config.yaml
// serve.platforms) is used as fallback.
func resolvePlatforms(args []string, configPlatforms []string) ([]string, error) {
	return serve.ResolvePlatforms(args, configPlatforms)
}

func platformContains(platforms []string, name string) bool {
	return serve.PlatformContains(platforms, name)
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

// Sources reported by resolveServeToken for use in the startup banner.
const (
	tokenSourceNone      = ""
	tokenSourceFlag      = "flag"
	tokenSourceEnv       = "env"
	tokenSourceGenerated = "generated"
)

// resolveServeToken returns the bearer token to use for the serve command.
// Precedence: --token flag > TERM_LLM_SERVE_TOKEN env > auto-generated.
// When requireAuth is false, returns an empty token and tokenSourceNone.
func resolveServeToken(flagValue, envValue string, requireAuth bool, generate func() (string, error)) (string, string, error) {
	if !requireAuth {
		return "", tokenSourceNone, nil
	}
	if t := strings.TrimSpace(flagValue); t != "" {
		return t, tokenSourceFlag, nil
	}
	if t := strings.TrimSpace(envValue); t != "" {
		return t, tokenSourceEnv, nil
	}
	t, err := generate()
	if err != nil {
		return "", tokenSourceNone, fmt.Errorf("generate auth token: %w", err)
	}
	return t, tokenSourceGenerated, nil
}

func generateServeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type serveServerConfig struct {
	host                    string
	port                    int
	requireAuth             bool
	token                   string
	ui                      bool
	api                     bool
	suppressServerTools     bool
	verbose                 bool
	basePath                string // e.g. "/ui" or "/chat", always without trailing slash
	publicURL               string // explicit browser-visible origin + optional prefix for OAuth callbacks
	uiTitle                 string
	locationSharingDisabled bool
	sidebarSessions         []string
	agentName               string
	agentNames              []string
	corsOrigins             []string
	filesDir                string   // opt-in directory for serving arbitrary files (videos, PDFs, etc)
	writeDirs               []string // tool write-dirs (CLI + config); tool-reported files inside these are trusted sources for ensureFileServeable
	responseTimeout         time.Duration
	// hubURL/hubNodeID/hubNodeName describe the term-llm Hub this node
	// belongs to. When hubURL is set, the web UI gets window.TERM_LLM_HUB and
	// renders a Back to Hub link. The hub proxy injects the same context
	// server-side, so these are only needed when a node should be hub-aware
	// even when opened directly.
	hubURL      string
	hubNodeID   string
	hubNodeName string
}

// uiRoute returns the base-path with trailing slash, e.g. "/ui/" or "/chat/".
func (c serveServerConfig) uiRoute() string { return c.basePath + "/" }

// imagesRoute returns the images sub-route, e.g. "/ui/images/" or "/chat/images/".
func (c serveServerConfig) imagesRoute() string { return c.basePath + "/images/" }

// mediaRoute returns the durable media sub-route.
func (c serveServerConfig) mediaRoute() string { return c.basePath + "/media/" }

// filesRoute returns the files sub-route, e.g. "/ui/files/" or "/chat/files/".
func (c serveServerConfig) filesRoute() string { return c.basePath + "/files/" }

// agentAvailable reports whether a first-party request may select name.
func (c serveServerConfig) agentAvailable(name string) bool {
	if name != "" && name == c.agentName {
		return true
	}
	for _, available := range c.agentNames {
		if name == available {
			return true
		}
	}
	return false
}

// resolveFilesDir returns the files-dir from the flag if set, otherwise from config.
func resolveFilesDir(flagVal string, cfg *config.Config) string {
	if flagVal != "" {
		return flagVal
	}
	return cfg.Serve.FilesDir
}

// serveWidgetsEnabled reports whether this process should discover and serve widgets.
func serveWidgetsEnabled(hasWeb, disabled bool) bool {
	return hasWeb && !disabled
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

func normalizeServePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("must be an http(s) URL with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must not contain a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// normalizeBasePath validates and normalizes a base-path value.
// It ensures a leading slash, strips trailing slashes, and rejects
// empty or root-only paths (use the default "/ui" instead of "/").
func normalizeBasePath(raw string) (string, error) {
	return servehttp.NormalizeBasePath(raw)
}

type serveServer struct {
	webExec                  *webExecCoordinator
	cfg                      serveServerConfig
	sessionMgr               *serveSessionManager
	jobsV2                   *jobsV2Manager
	cfgRef                   *config.Config
	store                    session.Store
	mediaPublisher           *serveMediaPublisher
	approvalDefault          tools.ApprovalMode
	sharePublisherFactory    func() (share.Publisher, error)
	shareMu                  sync.Mutex
	shareCacheKey            string
	shareCachedPublisher     share.Publisher
	shareCachedCapabilities  share.Capabilities
	shareCachedErr           error
	shareCacheUntil          time.Time
	shareBuildFlight         chan struct{}
	shareInvocation          chan struct{}
	projectsEnabled          bool
	bootstrapProjectID       string
	startupDir               string
	projectStatusMu          sync.Mutex
	projectStatuses          map[string]projectStatusCacheEntry
	server                   *http.Server
	shutdownCh               chan struct{}
	shutdownOnce             sync.Once
	modelsMu                 sync.Mutex
	modelsProviders          map[string]llm.Provider // keyed by provider name
	modelsCache              map[string]serveModelsCacheEntry
	responseToSession        sync.Map // response_id (string) → session_id (string)
	sessionToResponse        sync.Map // session_id (string) → latest response_id (string)
	branchNotes              sync.Map // child session_id → in-flight path-note preparation
	branchPathNoteFlights    sync.Map // source/idempotency key → shared path-note helper result
	responseRunsOnce         sync.Once
	responseRuns             *responseRunManager
	responseOwnerOnce        sync.Once
	responseLifecycleOnce    sync.Once
	responseLifecycleCancel  context.CancelFunc
	responseLifecycleWG      sync.WaitGroup
	responseOwnerInstanceID  string
	attentionDiagnostics     serveAttentionDiagnostics
	eventBrokerMu            sync.Mutex
	eventBroker              *serveEventBroker
	eventWatcherMu           sync.Mutex
	eventWatcherCancel       context.CancelFunc
	eventWatcherWG           sync.WaitGroup
	completionPushWake       chan struct{}
	completionPushWG         sync.WaitGroup
	transcriptIndexerOnce    sync.Once
	transcriptIndexer        session.TranscriptIndexer
	baseSystemPrompt         string
	skillsSetup              *skills.Setup
	skillsConfig             *config.SkillsConfig
	skillsCacheMu            sync.Mutex
	skillsByDir              map[string]serveSkillsCacheEntry
	mentionsCacheMu          sync.Mutex
	mentionsByRoot           map[string]*serveMentionCacheEntry
	mentionBuildFn           func(context.Context, string, mentions.BuildOptions) (*mentions.Snapshot, error)
	skillRunsMu              sync.Mutex
	skillRuns                map[string]*serveSkillRun
	skillRunsWG              sync.WaitGroup
	skillRunsStopping        bool
	skillRunRetention        time.Duration
	skillChildRunnerFactory  func(sessionID string, runtime *serveRuntime) (runpkg.ChildRunner, error)
	webrtcEnabled            bool
	webrtcMu                 sync.RWMutex
	webrtcPeer               webrtcpkg.Peer
	webrtcHeadSnippet        string // injected into index.html <head>; empty when WebRTC disabled
	runtimeFactory           func(ctx context.Context, providerName string, model string) (*serveRuntime, error)
	agentRuntimeFactory      func(ctx context.Context, providerName string, model string, agentName string) (*serveRuntime, error)
	titleProviderFactory     func(*config.Config) (llm.Provider, error)
	autoTitleProviderFactory func(string) (llm.Provider, error)
	autoTitleMu              sync.Mutex
	autoTitleFlights         map[string]struct{}
	autoTitleAttempts        map[string]serveAutoTitleAttempt
	autoTitleSlots           chan struct{}
	autoTitleCtx             context.Context
	autoTitleCancel          context.CancelFunc
	autoTitleWG              sync.WaitGroup
	autoTitleStopping        bool
	pathNotesProviderFactory func(providerName, model string) (llm.Provider, error)
	widgetsMgr               *widgets.Manager
	indexHTMLOnce            sync.Once
	cachedIndexHTML          []byte
	worktreeRootOnce         sync.Once
	worktreeRoot             string
	worktreeRootErr          error
	fileChangeRepoCacheMu    sync.Mutex
	fileChangeRepoCache      map[string]sessionGitRepoCacheEntry
	fileTrackStoreFn         func() *filetrack.Store // test seam; nil → process-wide store from config
	worktreeRootFn           func() (string, error)  // test seam; nil → os.Getwd
	rootMutationAdmitted     func()                  // test seam; called while the exclusive root lease is held
	commitMu                 sync.Mutex
	commitRuns               map[string]*serveCommitRun
	commitOperations         map[string]*serveCommitOperation
	commitOperationsWG       sync.WaitGroup
	commitRunsWG             sync.WaitGroup
	commitStopping           bool
	shellsMu                 sync.Mutex
	shells                   *serveShellManager
	shellsClosed             bool
}

// fileTrackStore returns the file-change history store, or nil when file
// tracking is disabled.
func (s *serveServer) fileTrackStore() *filetrack.Store {
	if s.fileTrackStoreFn != nil {
		return s.fileTrackStoreFn()
	}
	return fileTrackingStore(s.cfgRef)
}

func (s *serveServer) Start() error {
	s.shutdownCh = make(chan struct{})
	s.shutdownOnce = sync.Once{}
	s.autoTitleMu.Lock()
	if s.autoTitleCancel != nil {
		s.autoTitleCancel()
	}
	s.autoTitleCtx, s.autoTitleCancel = context.WithCancel(context.Background())
	s.autoTitleStopping = false
	s.autoTitleMu.Unlock()
	s.skillRunsMu.Lock()
	s.skillRunsStopping = false
	s.skillRunsMu.Unlock()
	s.resetEventBroker()
	s.skillsCacheMu.Lock()
	s.skillsByDir = nil
	s.skillsCacheMu.Unlock()
	s.mentionsCacheMu.Lock()
	s.mentionsByRoot = nil
	s.mentionsCacheMu.Unlock()
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.cfg.host, s.cfg.port),
		Handler:           s.httpHandler(),
		ReadHeaderTimeout: serveReadHeaderTimeout,
		IdleTimeout:       serveIdleTimeout,
		// Do not set server-wide WriteTimeout: long-lived SSE streams are valid.
		// Streaming handlers apply per-write deadlines instead.
	}

	if s.cfg.ui {
		s.prewarmUIAssetCache()
	}

	// Bind before consuming any handoff. Listener ownership, the stable service
	// identity and durable fences together prevent an env hint from stealing a
	// still-live service's work. No listener is inherited across exec.
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if s.webExec != nil {
		if err := s.webExec.resume(); err != nil {
			_ = listener.Close()
			return fmt.Errorf("recover web self-exec: %w", err)
		}
	}
	errCh := make(chan error, 1)
	go func() {
		err := s.server.Serve(listener)
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
		s.startEventWatcher()
		s.startResponseLifecycle()
		s.startCompletionPushDispatcher()
		if s.webExec != nil {
			s.webExec.mu.Lock()
			s.webExec.ready = true
			s.webExec.mu.Unlock()
		}
		return nil
	}
}

var registerServeBrowserFixtureRoutes func(*http.ServeMux, *serveServer)

func (s *serveServer) httpHandler() http.Handler {
	handler := s.rawHTTPHandler()
	if s.webExec != nil {
		return s.webExec.handler(handler)
	}
	return handler
}

func (s *serveServer) rawHTTPHandler() http.Handler {
	// Inner mux: all routes registered at their natural paths.
	// basePath is stripped by http.StripPrefix on the outer mux when mounted,
	// so handlers see /v1/..., /images/..., / etc. without the prefix.
	inner := http.NewServeMux()

	inner.HandleFunc("/healthz", s.handleHealth)
	inner.HandleFunc("/v1/providers", s.auth(s.cors(s.handleProviders)))
	inner.HandleFunc("/v1/models", s.auth(s.cors(s.handleModels)))
	inner.HandleFunc("/v1/mentions/search", s.auth(s.cors(s.handleMentionSearch)))
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
	inner.HandleFunc("/media/", s.auth(s.cors(s.handleMedia)))
	if s.cfg.filesDir != "" {
		inner.HandleFunc("/files/", s.auth(s.cors(s.handleFile)))
	}
	if s.widgetsMgr != nil {
		s.registerWidgetRoutes(inner)
	}
	inner.HandleFunc("/v1/capabilities", s.auth(s.cors(s.handleCapabilities)))
	inner.HandleFunc("/v1/sharing/capabilities", s.auth(s.cors(s.handleSharingCapabilities)))
	inner.HandleFunc("/v1/project-directories", s.auth(s.cors(s.handleProjectDirectories)))
	inner.HandleFunc("/v1/projects", s.auth(s.cors(s.handleProjects)))
	inner.HandleFunc("/v1/projects/", s.auth(s.cors(s.handleProjectByID)))
	inner.HandleFunc("/v1/sidebar", s.auth(s.cors(s.handleSidebar)))
	inner.HandleFunc("/v1/events", s.auth(s.cors(s.handleEvents)))
	inner.HandleFunc("/v1/events/poll", s.auth(s.cors(s.handleEventPoll)))
	inner.HandleFunc("/v1/mcp/oauth/flows/", s.auth(s.cors(s.handleMCPOAuthFlow)))
	// OAuth callbacks cannot carry the serve bearer token. A high-entropy,
	// single-use SDK state value is the capability checked by this handler.
	inner.HandleFunc("/v1/mcp/oauth/callback", s.handleMCPOAuthCallback)
	inner.HandleFunc("/v1/sessions/status", s.auth(s.cors(s.handleSessionsStatus)))
	inner.HandleFunc("/v1/attention", s.auth(s.cors(s.handleAttention)))
	inner.HandleFunc("/v1/sessions/search", s.auth(s.cors(s.handleSessionsSearch)))
	inner.HandleFunc("/v1/worktrees/diff", s.auth(s.cors(s.handleWorktreeDiff)))
	inner.HandleFunc("/v1/worktrees/merge", s.auth(s.cors(s.handleWorktreeMerge)))
	inner.HandleFunc("/v1/worktrees/assisted-merge", s.auth(s.cors(s.handleWorktreeAssistedMerge)))
	inner.HandleFunc("/v1/worktrees/promote", s.auth(s.cors(s.handleWorktreePromote)))
	inner.HandleFunc("/v1/worktrees", s.auth(s.cors(s.handleWorktrees)))
	inner.HandleFunc("/v1/sessions/", s.auth(s.cors(s.handleSessionByID)))
	inner.HandleFunc("/api/sessions/", s.auth(s.cors(s.handleSideQuestion)))
	inner.HandleFunc("/v1/push/subscribe", s.auth(s.cors(s.handlePushSubscribe)))

	if s.store != nil {
		inner.HandleFunc("/v1/sessions", s.auth(s.cors(s.handleSessions)))
	}
	if registerServeBrowserFixtureRoutes != nil {
		registerServeBrowserFixtureRoutes(inner, s)
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
	// Signal all SSE handlers and direct child runs to return immediately so
	// server shutdown cannot outlive the session store they persist into.
	s.shutdownOnce.Do(func() {
		if s.shutdownCh != nil {
			close(s.shutdownCh)
		}
	})
	s.closeShellManager()
	s.stopEventWatcher()
	// Synchronize with a concurrently starting lifecycle loop, or permanently
	// suppress a late start once shutdown has begun.
	s.responseLifecycleOnce.Do(func() {})
	if s.responseLifecycleCancel != nil {
		s.responseLifecycleCancel()
		s.responseLifecycleWG.Wait()
	}
	s.closeEventBroker()
	if s.server == nil {
		s.stopAutoTitles()
		return errors.Join(s.stopServeSkillRuns(ctx), s.stopCommitWorkflows(ctx))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 7)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	if s.jobsV2 != nil {
		run(func() error { return s.jobsV2.CloseContext(ctx) })
	}
	if s.responseRuns != nil {
		run(func() error {
			s.responseRuns.CloseContext(ctx)
			return nil
		})
	}
	run(func() error {
		done := make(chan struct{})
		go func() {
			s.completionPushWG.Wait()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if s.widgetsMgr != nil {
		run(func() error {
			s.widgetsMgr.CloseContext(ctx)
			return nil
		})
	}
	run(func() error { return s.stopServeSkillRuns(ctx) })
	run(func() error { return s.stopCommitWorkflows(ctx) })
	run(func() error {
		s.stopAutoTitles()
		return nil
	})
	run(func() error { return s.server.Shutdown(ctx) })

	s.modelsMu.Lock()
	for _, p := range s.modelsProviders {
		if cleaner, ok := p.(interface{ CleanupMCP() }); ok {
			cleaner.CleanupMCP()
		}
	}
	s.modelsProviders = nil
	s.modelsCache = nil
	s.modelsMu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Shutdown methods may return as soon as ctx expires even though their
		// owners are still unwinding. Keep the process-wide file history store
		// open in that case so late tool recorders can finish safely.
		if err := ctx.Err(); err != nil {
			return err
		}
		closeFileTrackingStore()
		close(errCh)
		for err := range errCh {
			if err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
