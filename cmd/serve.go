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

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
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
	serveUI                     bool
	serveUIPrefix               string
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
	// Platform flags
	servePlatform string
	serveSetup    bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the agent as a server (web, jobs, Telegram, or any combination)",
	Long: `Run term-llm as a server on one or more platforms simultaneously.

Default platform is "web": an OpenAI-compatible HTTP server.

Web endpoints:
  POST /v1/responses
  POST /v1/chat/completions
  GET  /v1/models
  GET  /healthz

Jobs endpoints:
  POST   /v2/jobs
  GET    /v2/jobs
  GET    /v2/jobs/:id
  PATCH  /v2/jobs/:id
  DELETE /v2/jobs/:id
  POST   /v2/jobs/:id/trigger
  POST   /v2/jobs/:id/pause
  POST   /v2/jobs/:id/resume
  GET    /v2/runs
  GET    /v2/runs/:id
  GET    /v2/runs/:id/events
  POST   /v2/runs/:id/cancel

Use --ui to also serve a minimal web chat interface.
Use --platform jobs for async per-agent queued work.
Use --platform telegram to run a Telegram bot instead (or alongside web/jobs).
Use --setup to configure credentials for the selected platforms.`,
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)

	serveCmd.Flags().StringVar(&serveHost, "host", "127.0.0.1", "Bind host")
	serveCmd.Flags().IntVar(&servePort, "port", 8080, "Bind port")
	serveCmd.Flags().StringVar(&serveToken, "token", "", "Bearer token for API auth (auto-generated if omitted)")
	serveCmd.Flags().BoolVar(&serveAllowNoAuth, "allow-no-auth", false, "Disable auth (only allowed on loopback host)")
	serveCmd.Flags().StringVar(&serveAuthMode, "auth", "bearer", "Auth mode: bearer or none")
	serveCmd.Flags().BoolVar(&serveUI, "ui", false, "Serve minimal web UI")
	serveCmd.Flags().StringVar(&serveUIPrefix, "ui-prefix", "/ui", "URL prefix the UI uses for session URLs (e.g. /chat)")
	serveCmd.Flags().StringArrayVar(&serveCORSOrigins, "cors-origin", nil, "Allowed CORS origin (repeatable, or '*' for all)")
	serveCmd.Flags().DurationVar(&serveSessionTTL, "session-ttl", 30*time.Minute, "Stateful session idle TTL")
	serveCmd.Flags().IntVar(&serveSessionMax, "session-max", 1000, "Max stateful sessions in memory")

	serveCmd.Flags().StringVar(&servePlatform, "platform", "web", "Comma-separated platforms to serve: web, jobs, telegram")
	if err := serveCmd.RegisterFlagCompletionFunc("platform", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"web", "jobs", "telegram", "web,jobs", "web,telegram", "jobs,telegram", "web,jobs,telegram"}, cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic("failed to register platform completion: " + err.Error())
	}
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

	platformNames := parsePlatforms(servePlatform)
	hasJobs := platformContains(platformNames, "jobs")
	hasWeb := platformContains(platformNames, "web")
	hasTelegram := platformContains(platformNames, "telegram")

	cfg, err := loadConfigWithSetup()
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

	// Build the insight expander (nil when disabled or store unavailable).
	// Captured by the factory closure and shared across all runtimes.
	var insightsExpander *memorydb.InsightsExpander
	var insightsMemStore *memorydb.Store // kept for deferred Close
	if settings.InsightsExpansion {
		if ms, msErr := openMemoryStore(); msErr == nil {
			insightsMemStore = ms
			insightsExpander = memorydb.NewInsightsExpander(ms, agentName, settings.InsightsMaxTokens)
		}
	}

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
			insightsExpander:    insightsExpander,
			toolsSetting:        settings.Tools,
			mcpSetting:          settings.MCP,
			agentName:           agentName,
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
		InsightsExpander:       insightsExpander,
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
		s = &serveServer{
			cfg: serveServerConfig{
				host:        serveHost,
				port:        servePort,
				requireAuth: requireAuth,
				token:       token,
				ui:          serveUI,
				uiPrefix:    strings.TrimRight(serveUIPrefix, "/"),
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
		fmt.Fprintf(cmd.ErrOrStderr(), "ui: %v\n", serveUI)
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

	if insightsMemStore != nil {
		_ = insightsMemStore.Close()
	}

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

func parsePlatforms(flag string) []string {
	var out []string
	for _, part := range strings.Split(flag, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		out = []string{"web"}
	}
	return out
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
	uiPrefix    string
	corsOrigins []string
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
}

func (s *serveServer) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.auth(s.cors(s.handleModels)))
	mux.HandleFunc("/v1/responses", s.auth(s.cors(s.handleResponses)))
	mux.HandleFunc("/v1/chat/completions", s.auth(s.cors(s.handleChatCompletions)))
	if s.jobsV2 != nil {
		mux.HandleFunc("/v2/jobs", s.auth(s.cors(s.handleJobsV2)))
		mux.HandleFunc("/v2/jobs/", s.auth(s.cors(s.handleJobV2ByID)))
		mux.HandleFunc("/v2/runs", s.auth(s.cors(s.handleRunsV2)))
		mux.HandleFunc("/v2/runs/", s.auth(s.cors(s.handleRunV2ByID)))
	}

	mux.HandleFunc("/images/", s.auth(s.cors(s.handleImage)))

	if s.store != nil {
		mux.HandleFunc("/v1/sessions", s.auth(s.cors(s.handleSessions)))
		mux.HandleFunc("/v1/sessions/", s.auth(s.cors(s.handleSessionByID)))
	}

	if s.cfg.ui {
		mux.HandleFunc("/", s.cors(s.handleUI))
		mux.HandleFunc("/ui", s.cors(s.handleUI))
		mux.HandleFunc("/ui/", s.cors(s.handleUI))
	}

	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.cfg.host, s.cfg.port),
		Handler: mux,
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

func (s *serveServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	if s.jobsV2 != nil {
		_ = s.jobsV2.Close()
	}
	s.modelsMu.Lock()
	if cleaner, ok := s.modelsProvider.(interface{ CleanupMCP() }); ok {
		cleaner.CleanupMCP()
	}
	s.modelsProvider = nil
	s.modelsMu.Unlock()
	return s.server.Shutdown(ctx)
}
