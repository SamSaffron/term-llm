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
	"github.com/spf13/cobra"
)

var (
	serveHost                   string
	servePort                   int
	serveToken                  string
	serveAllowNoAuth            bool
	serveAuthMode               string
	serveNoUI                   bool
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
)

var serveCmd = &cobra.Command{
	Use:   "serve <platform> [platform...]",
	Short: "Run the agent as a server (web, jobs, Telegram, or any combination)",
	Long: `Run term-llm as a server on one or more platforms simultaneously.

Platforms are specified as positional arguments. If none are given, the
serve.platforms list from config.yaml is used.

Examples:
  term-llm serve web             # web server with UI enabled
  term-llm serve web --no-ui     # web API only (no chat UI)
  term-llm serve telegram        # Telegram bot only
  term-llm serve telegram web    # both platforms
  term-llm serve web --base-path /chat

All HTTP routes are mounted under --base-path (default /ui):
  POST {base}/v1/responses
  POST {base}/v1/chat/completions
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
	ValidArgs: []string{"web", "jobs", "telegram"},
	RunE:      runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)

	serveCmd.Flags().StringVar(&serveHost, "host", "127.0.0.1", "Bind host")
	serveCmd.Flags().IntVar(&servePort, "port", 8080, "Bind port")
	serveCmd.Flags().StringVar(&serveToken, "token", "", "Bearer token for API auth (auto-generated if omitted)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "allow-no-auth", false, "Disable auth (only allowed on loopback host)")
	serveCmd.Flags().StringVar(&serveAuthMode, "auth", "bearer", "Auth mode: bearer or none")
	serveCmd.Flags().BoolVar(&serveNoUI, "no-ui", false, "Disable web UI (enabled by default when web platform is active)")
	serveCmd.Flags().StringVar(&serveBasePath, "base-path", "/ui", "URL prefix the UI uses for session URLs (e.g. /chat)")
	serveCmd.Flags().StringArrayVar(&serveCORSOrigins, "cors-origin", nil, "Allowed CORS origin (repeatable, or '*' for all)")
	serveCmd.Flags().DurationVar(&serveSessionTTL, "session-ttl", 30*time.Minute, "Stateful session idle TTL")
	serveCmd.Flags().IntVar(&serveSessionMax, "session-max", 1000, "Max stateful sessions in memory")

	serveCmd.Flags().BoolVar(&serveSetup, "setup", false, "Re-run setup wizard for selected platforms")
	serveCmd.Flags().IntVar(&serveTelegramCarryoverChars, "telegram-carryover-chars", 4000, "Characters of previous Telegram session context to carry into replacement sessions (0 disables)")
	serveCmd.Flags().IntVar(&serveJobsWorkers, "jobs-workers", 4, "Number of concurrent job workers for --platform jobs")

	AddProviderFlag(serveCmd, &serveProvider)
	AddDebugFlag(serveCmd, &serveDebug)
	AddSearchFlag(serveCmd, &serveSearch)
	AddNativeSearchFlags(serveCmd, &serveNativeSearch, &serveNoNativeSearch)
	AddMCPFlag(serveCmd, &serveMCP)
	AddMaxTurnsFlag(serveCmd, &serveMaxTurns, 200)
	AddToolFlags(serveCmd, &serveTools, &serveReadDirs, &serveWriteDirs, &serveShellAllow)
	AddSystemMessageFlag(serveCmd, &serveSystemMessage)
	AddAgentFlag(serveCmd, &serveAgent)
	AddYoloFlag(serveCmd, &serveYolo)
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

	authMode, err := resolveServeAuthMode(cmd.Flags().Changed("auth"), serveAuthMode, cmd.Flags().Changed("allow-no-auth"), serveAllowNoAuth)
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

	var agent *agents.Agent
	if hasWeb || hasTelegram {
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
	skillsSetup := SetupSkills(&cfg.Skills, "", cmd.ErrOrStderr())
	settings.SystemPrompt = InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)

	agentName := ""
	if agent != nil {
		agentName = agent.Name
	}

	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	defer storeCleanup()
	if store != nil {
		store = session.NewLoggingStore(store, func(format string, args ...any) {
			log.Printf("[serve] "+format, args...)
		})
	}

	forceExternalSearch := resolveForceExternalSearch(cfg, serveNativeSearch, serveNoNativeSearch)

	modelName := activeModel(cfg)
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider, err := llm.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, serveYolo, WireSpawnAgentRunner, skillsSetup)
		if err != nil {
			return nil, err
		}

		var mcpManager *mcp.Manager
		if settings.MCP != "" {
			mcpOpts := &MCPOptions{Provider: provider, Model: modelName, YoloMode: serveYolo}
			mgr, err := enableMCPServersWithFeedback(ctx, settings.MCP, engine, io.Discard, mcpOpts)
			if err != nil {
				return nil, err
			}
			mcpManager = mgr
		}

		runtime := &serveRuntime{
			provider:            provider,
			providerKey:         cfg.DefaultProvider,
			engine:              engine,
			toolMgr:             toolMgr,
			mcpManager:          mcpManager,
			systemPrompt:        settings.SystemPrompt,
			search:              settings.Search,
			forceExternalSearch: forceExternalSearch,
			maxTurns:            settings.MaxTurns,
			debug:               serveDebug,
			debugRaw:            debugRaw,
			defaultModel:        modelName,
			store:               store,
			toolsSetting:        settings.Tools,
			mcpSetting:          settings.MCP,
			agentName:           agentName,
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
				runtime.toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
					return runtime.awaitApproval(path, isWrite, isShell)
				}
			}
		}
		runtime.Touch()
		return runtime, nil
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

	hasHTTP := hasWeb || hasJobs

	var s *serveServer
	if hasHTTP {
		var jobsV2 *jobsV2Manager
		if hasJobs {
			jobsV2, err = newServeJobsV2Manager(cfg, serveJobsWorkers)
			if err != nil {
				return fmt.Errorf("initialize jobs v2 manager: %w", err)
			}
		}
		serveUI := hasWeb && !serveNoUI
		s = &serveServer{
			cfg: serveServerConfig{
				host:        serveHost,
				port:        servePort,
				requireAuth: requireAuth,
				token:       token,
				ui:          serveUI,
				basePath:    serveBasePath,
				corsOrigins: append([]string(nil), serveCORSOrigins...),
			},
			sessionMgr: sessionMgr,
			jobsV2:     jobsV2,
			cfgRef:     cfg,
			store:      store,
		}
		sessionMgr.onEvict = func(rt *serveRuntime) {
			for _, rid := range rt.getResponseIDs() {
				s.responseToSession.Delete(rid)
			}
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
func newServeEngineWithTools(cfg *config.Config, settings SessionSettings, provider llm.Provider, yoloMode bool, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*llm.Engine, *tools.ToolManager, error) {
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

	// Register the activate_skill tool on the engine. Metadata injection into the
	// system prompt is handled by the caller to avoid the by-value settings copy trap.
	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	return engine, toolMgr, nil
}

var knownPlatforms = map[string]bool{"web": true, "jobs": true, "telegram": true}

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
			return nil, fmt.Errorf("unknown platform %q (valid: web, jobs, telegram)", p)
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
		case "web", "telegram", "jobs":
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
			return "", fmt.Errorf("--auth %s conflicts with --allow-no-auth=%v", mode, allowNoAuth)
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
	host        string
	port        int
	requireAuth bool
	token       string
	ui          bool
	basePath    string // e.g. "/ui" or "/chat", always without trailing slash
	corsOrigins []string
}

// uiRoute returns the base-path with trailing slash, e.g. "/ui/" or "/chat/".
func (c serveServerConfig) uiRoute() string { return c.basePath + "/" }

// imagesRoute returns the images sub-route, e.g. "/ui/images/" or "/chat/images/".
func (c serveServerConfig) imagesRoute() string { return c.basePath + "/images/" }

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
	modelsMu          sync.Mutex
	modelsProvider    llm.Provider
	responseToSession sync.Map // response_id (string) → session_id (string)
	responseRunsOnce  sync.Once
	responseRuns      *responseRunManager
}

func (s *serveServer) Start() error {
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
	inner.HandleFunc("/v1/models", s.auth(s.cors(s.handleModels)))
	inner.HandleFunc("/v1/responses", s.auth(s.cors(s.handleResponses)))
	inner.HandleFunc("/v1/responses/", s.auth(s.cors(s.handleResponseByID)))
	inner.HandleFunc("/v1/chat/completions", s.auth(s.cors(s.handleChatCompletions)))
	inner.HandleFunc("/v1/transcribe", s.auth(s.cors(s.handleTranscribe)))
	if s.jobsV2 != nil {
		inner.HandleFunc("/v2/jobs", s.auth(s.cors(s.handleJobsV2)))
		inner.HandleFunc("/v2/jobs/", s.auth(s.cors(s.handleJobV2ByID)))
		inner.HandleFunc("/v2/runs", s.auth(s.cors(s.handleRunsV2)))
		inner.HandleFunc("/v2/runs/", s.auth(s.cors(s.handleRunV2ByID)))
	}

	inner.HandleFunc("/images/", s.auth(s.cors(s.handleImage)))
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
	if s.jobsV2 != nil && !s.cfg.ui {
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

func (s *serveServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	if s.jobsV2 != nil {
		_ = s.jobsV2.Close()
	}
	if s.responseRuns != nil {
		s.responseRuns.Close()
	}
	s.modelsMu.Lock()
	if cleaner, ok := s.modelsProvider.(interface{ CleanupMCP() }); ok {
		cleaner.CleanupMCP()
	}
	s.modelsProvider = nil
	s.modelsMu.Unlock()
	return s.server.Shutdown(ctx)
}
