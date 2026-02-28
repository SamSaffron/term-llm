package cmd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
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
		// Platform intentionally omitted — {{platform}} is expanded per-session
		// by platformFactory so each surface (telegram, web) gets the right value.
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 20)
	if err != nil {
		return err
	}

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
	// platformFactory creates a runtime for a specific platform surface.
	// It substitutes {{platform}} in the system prompt at session-creation time
	// so each conversation knows which surface it's on (telegram, web, etc.).
	platformFactory := func(ctx context.Context, platform string) (*serveRuntime, error) {
		provider, err := llm.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		// Substitute {{platform}} now that we know the surface.
		ps := settings
		ps.SystemPrompt = strings.ReplaceAll(settings.SystemPrompt, "{{platform}}", platform)
		engine, toolMgr, err := newServeEngineWithTools(cfg, ps, provider, serveYolo, WireSpawnAgentRunner)
		if err != nil {
			return nil, err
		}

		var mcpManager *mcp.Manager
		if ps.MCP != "" {
			mcpOpts := &MCPOptions{Provider: provider, Model: modelName, YoloMode: serveYolo}
			mgr, err := enableMCPServersWithFeedback(ctx, ps.MCP, engine, io.Discard, mcpOpts)
			if err != nil {
				return nil, err
			}
			mcpManager = mgr
		}

		runtime := &serveRuntime{
			provider:            provider,
			engine:              engine,
			toolMgr:             toolMgr,
			mcpManager:          mcpManager,
			systemPrompt:        ps.SystemPrompt,
			search:              ps.Search,
			forceExternalSearch: forceExternalSearch,
			maxTurns:            ps.MaxTurns,
			debug:               serveDebug,
			debugRaw:            debugRaw,
			defaultModel:        modelName,
			store:               store,
			toolsSetting:        ps.Tools,
			mcpSetting:          ps.MCP,
			agentName:           agentName,
		}
		runtime.Touch()
		return runtime, nil
	}
	// factory is the web-platform session factory (platform = "web").
	factory := func(ctx context.Context) (*serveRuntime, error) {
		return platformFactory(ctx, "web")
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
			rt, err := platformFactory(ctx, "telegram")
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
				corsOrigins: append([]string(nil), serveCORSOrigins...),
			},
			sessionMgr: sessionMgr,
			jobsV2:     jobsV2,
			cfgRef:     cfg,
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

	if s != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Stop(shutdownCtx)
	}

	wg.Wait()
	return nil
}

func newServeEngineWithTools(cfg *config.Config, settings SessionSettings, provider llm.Provider, yoloMode bool, wireSpawn func(*config.Config, *tools.ToolManager, bool) error) (*llm.Engine, *tools.ToolManager, error) {
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

	// Wire skills system (activate_skill tool + system prompt injection).
	skillsSetup := SetupSkills(&cfg.Skills, "", io.Discard)
	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)
	settings.SystemPrompt = InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)

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
	corsOrigins []string
}

type serveServer struct {
	cfg            serveServerConfig
	sessionMgr     *serveSessionManager
	jobsV2         *jobsV2Manager
	cfgRef         *config.Config
	server         *http.Server
	modelsMu       sync.Mutex
	modelsProvider llm.Provider
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

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *serveServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ui {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(serveui.IndexHTML())
}

func (s *serveServer) auth(next http.HandlerFunc) http.HandlerFunc {
	if !s.cfg.requireAuth {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid authentication credentials")
			return
		}
		gotToken := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
		if subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.cfg.token)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid authentication credentials")
			return
		}
		next(w, r)
	}
}

func (s *serveServer) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := make(map[string]struct{}, len(s.cfg.corsOrigins))
	allowAll := false
	for _, origin := range s.cfg.corsOrigins {
		o := strings.TrimSpace(origin)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		allowed[o] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, session_id")
			w.Header().Set("Access-Control-Expose-Headers", "x-session-id")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	provider, err := s.getModelsProvider()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	models := make([]llm.ModelInfo, 0)
	if lister, ok := provider.(interface {
		ListModels(context.Context) ([]llm.ModelInfo, error)
	}); ok {
		if listed, err := lister.ListModels(ctx); err == nil {
			models = listed
		}
	}

	if len(models) == 0 {
		providerName := s.cfgRef.DefaultProvider
		if providerCfg := s.cfgRef.GetActiveProviderConfig(); providerCfg != nil {
			if providerCfg.Model != "" {
				models = append(models, llm.ModelInfo{ID: providerCfg.Model})
			}
		}
		if curated, ok := llm.ProviderModels[providerName]; ok {
			for _, id := range curated {
				models = append(models, llm.ModelInfo{ID: id})
			}
		}
	}

	seen := map[string]bool{}
	items := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		items = append(items, map[string]any{
			"id":      m.ID,
			"object":  "model",
			"created": m.Created,
			"owned_by": func() string {
				if m.OwnedBy != "" {
					return m.OwnedBy
				}
				return "term-llm"
			}(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		idi, _ := items[i]["id"].(string)
		idj, _ := items[j]["id"].(string)
		return idi < idj
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func (s *serveServer) getModelsProvider() (llm.Provider, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.modelsProvider != nil {
		return s.modelsProvider, nil
	}
	provider, err := llm.NewProvider(s.cfgRef)
	if err != nil {
		return nil, err
	}
	s.modelsProvider = provider
	return provider, nil
}

func (s *serveServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req responsesCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	inputMessages, replaceHistory, err := parseResponsesInput(req.Input)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(inputMessages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	sessionID := resolveRequestSessionID(w, r)
	runtime, stateful, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !stateful {
		defer runtime.Close()
	}

	searchFromTools, requestedTools := parseRequestedTools(req.Tools)
	search := runtime.search || searchFromTools
	toolChoice := parseToolChoice(req.ToolChoice)
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		Tools:               runtime.selectTools(requestedTools),
		ToolChoice:          toolChoice,
		ParallelToolCalls:   parallel,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		MaxTurns:            runtime.maxTurns,
		Debug:               runtime.debug,
		DebugRaw:            runtime.debugRaw,
	}

	if req.MaxOutputTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		llmReq.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		llmReq.TopP = *req.TopP
	}

	if req.Stream {
		s.streamResponses(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID)
		return
	}

	result, err := runtime.Run(ctx, stateful, replaceHistory, inputMessages, llmReq)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	writeJSON(w, http.StatusOK, responsesFinalResponse(result, model))
}

func (s *serveServer) streamResponses(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	respID := "resp_" + sessionOrRandomID(sessionID)
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	_ = writeSSEEvent(w, "response.created", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "in_progress",
		},
	})
	flusher.Flush()

	outputIndex := 0
	result, err := runtime.RunWithEvents(ctx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		switch ev.Type {
		case llm.EventTextDelta:
			return writeSSEEvent(w, "response.output_text.delta", map[string]any{
				"output_index": outputIndex,
				"delta":        ev.Text,
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			item := map[string]any{
				"id":        "fc_" + ev.Tool.ID,
				"type":      "function_call",
				"call_id":   ev.Tool.ID,
				"name":      ev.Tool.Name,
				"arguments": string(ev.Tool.Arguments),
			}
			if err := writeSSEEvent(w, "response.output_item.added", map[string]any{"output_index": outputIndex, "item": item}); err != nil {
				return err
			}
			if err := writeSSEEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": outputIndex, "delta": string(ev.Tool.Arguments)}); err != nil {
				return err
			}
			if err := writeSSEEvent(w, "response.output_item.done", map[string]any{"output_index": outputIndex, "item": item}); err != nil {
				return err
			}
			outputIndex++
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		errType := "invalid_request_error"
		if errors.Is(err, errServeSessionBusy) {
			errType = "conflict_error"
		}
		_ = writeSSEEvent(w, "response.failed", map[string]any{
			"error": map[string]any{"message": err.Error(), "type": errType},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	_ = writeSSEEvent(w, "response.completed", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "completed",
			"usage": map[string]any{
				"input_tokens":  result.Usage.InputTokens,
				"output_tokens": result.Usage.OutputTokens,
				"total_tokens":  result.Usage.InputTokens + result.Usage.OutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": result.Usage.CachedInputTokens,
				},
			},
		},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *serveServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req chatCompletionsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	messages, replaceHistory, err := parseChatMessages(req.Messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	sessionID := resolveRequestSessionID(w, r)
	runtime, stateful, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !stateful {
		defer runtime.Close()
	}

	search := runtime.search
	requestedTools := parseChatRequestedToolNames(req.Tools)
	toolChoice := parseToolChoice(req.ToolChoice)
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		Tools:               runtime.selectTools(requestedTools),
		ToolChoice:          toolChoice,
		ParallelToolCalls:   parallel,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		MaxTurns:            runtime.maxTurns,
		Debug:               runtime.debug,
		DebugRaw:            runtime.debugRaw,
	}
	if req.MaxTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		llmReq.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		llmReq.TopP = *req.TopP
	}

	if req.Stream {
		s.streamChatCompletions(ctx, w, runtime, stateful, replaceHistory, messages, llmReq, req.StreamOptions, sessionID)
		return
	}

	result, err := runtime.Run(ctx, stateful, replaceHistory, messages, llmReq)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	writeJSON(w, http.StatusOK, chatCompletionFinalResponse(result, model))
}

func (s *serveServer) streamChatCompletions(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, streamOpts *chatStreamOptions, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	respID := "chatcmpl_" + sessionOrRandomID(sessionID)
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	first := true
	toolCallSeen := false
	result, err := runtime.RunWithEvents(ctx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		var writeErr error
		switch ev.Type {
		case llm.EventTextDelta:
			delta := map[string]any{"content": ev.Text}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{"index": 0, "delta": delta}},
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			toolCallSeen = true
			if first {
				if err := writeChatStreamChunk(w, map[string]any{
					"id":      respID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}},
				}); err != nil {
					return err
				}
				flusher.Flush()
				first = false
			}
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    ev.Tool.ID,
							"type":  "function",
							"function": map[string]any{
								"name":      ev.Tool.Name,
								"arguments": string(ev.Tool.Arguments),
							},
						}},
					},
				}},
			})
		}
		if writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			_ = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{"index": 0, "finish_reason": "error", "delta": map[string]any{}}},
				"error":   map[string]any{"message": err.Error(), "type": "conflict_error"},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "error", "delta": map[string]any{}}},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	finishReason := "stop"
	if toolCallSeen {
		finishReason = "tool_calls"
	}
	_ = writeChatStreamChunk(w, map[string]any{
		"id":      respID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
	})
	if streamOpts != nil && streamOpts.IncludeUsage {
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":     result.Usage.InputTokens,
				"completion_tokens": result.Usage.OutputTokens,
				"total_tokens":      result.Usage.InputTokens + result.Usage.OutputTokens,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": result.Usage.CachedInputTokens,
				},
			},
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeChatStreamChunk(w io.Writer, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func (s *serveServer) runtimeForRequest(ctx context.Context, sessionID string) (*serveRuntime, bool, error) {
	if sessionID == "" {
		// Ephemeral stateless runtime (fresh per request for isolation)
		rt, err := s.sessionMgr.factory(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Stateful sessions should outlive a single HTTP request context.
	rt, err := s.sessionMgr.GetOrCreate(context.Background(), sessionID)
	if err != nil {
		return nil, false, err
	}
	return rt, true, nil
}

type serveSessionManager struct {
	ttl     time.Duration
	max     int
	factory func(context.Context) (*serveRuntime, error)

	mu       sync.Mutex
	sessions map[string]*serveRuntime
	creating map[string]*sessionCreateInFlight
	closed   bool
	stopCh   chan struct{}
}

type sessionCreateInFlight struct {
	done chan struct{}
	rt   *serveRuntime
	err  error
}

func newServeSessionManager(ttl time.Duration, max int, factory func(context.Context) (*serveRuntime, error)) *serveSessionManager {
	m := &serveSessionManager{
		ttl:      ttl,
		max:      max,
		factory:  factory,
		sessions: make(map[string]*serveRuntime),
		creating: make(map[string]*sessionCreateInFlight),
		stopCh:   make(chan struct{}),
	}
	go m.janitor()
	return m
}

func (m *serveSessionManager) janitor() {
	ticker := time.NewTicker(max(30*time.Second, m.ttl/2))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.evictExpired()
		case <-m.stopCh:
			return
		}
	}
}

func (m *serveSessionManager) evictExpired() {
	now := time.Now()
	var stale []*serveRuntime

	m.mu.Lock()
	for id, rt := range m.sessions {
		if now.Sub(rt.LastUsed()) > m.ttl {
			delete(m.sessions, id)
			stale = append(stale, rt)
		}
	}
	m.mu.Unlock()

	for _, rt := range stale {
		rt.Close()
	}
}

func (m *serveSessionManager) GetOrCreate(ctx context.Context, id string) (*serveRuntime, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("session manager closed")
	}
	if rt, ok := m.sessions[id]; ok {
		rt.Touch()
		m.mu.Unlock()
		return rt, nil
	}
	if inflight, ok := m.creating[id]; ok {
		m.mu.Unlock()
		<-inflight.done
		if inflight.err != nil {
			return nil, inflight.err
		}
		if inflight.rt == nil {
			return nil, fmt.Errorf("failed to initialize session runtime")
		}
		inflight.rt.Touch()
		return inflight.rt, nil
	}
	inflight := &sessionCreateInFlight{done: make(chan struct{})}
	m.creating[id] = inflight
	m.mu.Unlock()

	rt, err := m.factory(ctx)
	m.mu.Lock()
	delete(m.creating, id)

	var duplicate *serveRuntime
	var evicted *serveRuntime
	switch {
	case err != nil:
		inflight.err = err
	case m.closed:
		inflight.err = fmt.Errorf("session manager closed")
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			if len(m.sessions) >= m.max {
				oldestID := ""
				var oldestTime time.Time
				for sid, srt := range m.sessions {
					t := srt.LastUsed()
					if oldestID == "" || t.Before(oldestTime) {
						oldestID = sid
						oldestTime = t
					}
				}
				if oldestID != "" {
					evicted = m.sessions[oldestID]
					delete(m.sessions, oldestID)
				}
			}
			m.sessions[id] = rt
			inflight.rt = rt
		}
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		duplicate.Close()
	}
	if evicted != nil {
		evicted.Close()
	}
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			rt.Close()
		}
		return nil, inflight.err
	}
	if inflight.rt == nil {
		return nil, fmt.Errorf("failed to initialize session runtime")
	}
	inflight.rt.Touch()
	return inflight.rt, nil
}

func (m *serveSessionManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopCh)
	sessions := make([]*serveRuntime, 0, len(m.sessions))
	for _, rt := range m.sessions {
		sessions = append(sessions, rt)
	}
	m.sessions = map[string]*serveRuntime{}
	m.mu.Unlock()

	for _, rt := range sessions {
		rt.Close()
	}
}

type serveRuntime struct {
	mu                  sync.Mutex
	provider            llm.Provider
	engine              *llm.Engine
	toolMgr             *tools.ToolManager
	mcpManager          *mcp.Manager
	store               session.Store
	systemPrompt        string
	history             []llm.Message
	search              bool
	toolsSetting        string
	mcpSetting          string
	agentName           string
	sessionMeta         *session.Session
	forceExternalSearch bool
	maxTurns            int
	debug               bool
	debugRaw            bool
	defaultModel        string
	lastUsedUnixNano    atomic.Int64
}

func (rt *serveRuntime) Touch() {
	rt.lastUsedUnixNano.Store(time.Now().UnixNano())
}

func (rt *serveRuntime) LastUsed() time.Time {
	unixNano := rt.lastUsedUnixNano.Load()
	if unixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNano)
}

func (rt *serveRuntime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.mcpManager != nil {
		rt.mcpManager.StopAll()
		rt.mcpManager = nil
	}
	if cleaner, ok := rt.provider.(interface{ CleanupMCP() }); ok {
		cleaner.CleanupMCP()
	}
}

func (rt *serveRuntime) ensurePersistedSession(ctx context.Context, sessionID string, inputMessages []llm.Message) bool {
	if rt.store == nil || sessionID == "" {
		return false
	}
	if rt.sessionMeta != nil && rt.sessionMeta.ID == sessionID {
		return true
	}

	providerName := "unknown"
	if rt.provider != nil {
		if name := strings.TrimSpace(rt.provider.Name()); name != "" {
			providerName = name
		}
	}
	modelName := strings.TrimSpace(rt.defaultModel)
	if modelName == "" {
		modelName = "unknown"
	}

	sess := &session.Session{
		ID:        sessionID,
		Provider:  providerName,
		Model:     modelName,
		Mode:      session.ModeChat,
		Agent:     rt.agentName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Search:    rt.search,
		Tools:     rt.toolsSetting,
		MCP:       rt.mcpSetting,
		Status:    session.StatusActive,
	}
	if cwd, err := os.Getwd(); err == nil {
		sess.CWD = cwd
	}
	for _, msg := range inputMessages {
		if msg.Role != llm.RoleUser {
			continue
		}
		if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
			sess.Summary = session.TruncateSummary(text)
			break
		}
	}

	if err := rt.store.Create(ctx, sess); err != nil {
		existing, getErr := rt.store.Get(ctx, sessionID)
		if getErr != nil || existing == nil {
			log.Printf("[serve] session Create failed for %s: %v", sessionID, err)
			return false
		}
		rt.sessionMeta = existing
		if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
			log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
		}
		return true
	}

	rt.sessionMeta = sess
	if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	return true
}

func (rt *serveRuntime) persistSnapshot(ctx context.Context, sessionID string, snapshot []llm.Message) {
	if rt.store == nil || sessionID == "" {
		return
	}
	messages := make([]session.Message, 0, len(snapshot))
	for _, msg := range snapshot {
		if msg.Role == "" {
			continue
		}
		sessionMsg := session.NewMessage(sessionID, msg, -1)
		messages = append(messages, *sessionMsg)
	}
	if err := rt.store.ReplaceMessages(ctx, sessionID, messages); err != nil {
		log.Printf("[serve] session ReplaceMessages failed for %s: %v", sessionID, err)
		return
	}
	if rt.sessionMeta != nil && rt.sessionMeta.Summary == "" {
		for _, msg := range snapshot {
			if msg.Role != llm.RoleUser {
				continue
			}
			if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
				rt.sessionMeta.Summary = session.TruncateSummary(text)
				if updateErr := rt.store.Update(ctx, rt.sessionMeta); updateErr != nil {
					log.Printf("[serve] session Update failed for %s: %v", sessionID, updateErr)
				}
				break
			}
		}
	}
	if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	if statusErr := rt.store.UpdateStatus(ctx, sessionID, session.StatusActive); statusErr != nil {
		log.Printf("[serve] session UpdateStatus(active) failed for %s: %v", sessionID, statusErr)
	}
}

func (rt *serveRuntime) persistStatus(ctx context.Context, sessionID string, status session.SessionStatus) {
	if rt.store == nil || sessionID == "" {
		return
	}
	if err := rt.store.UpdateStatus(ctx, sessionID, status); err != nil {
		log.Printf("[serve] session UpdateStatus(%s) failed for %s: %v", status, sessionID, err)
	}
}

func (rt *serveRuntime) selectTools(requested map[string]bool) []llm.ToolSpec {
	all := rt.engine.Tools().AllSpecs()
	if len(requested) == 0 {
		return all
	}
	out := make([]llm.ToolSpec, 0, len(all))
	for _, spec := range all {
		if requested[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

type serveRunResult struct {
	Text      strings.Builder
	ToolCalls []llm.ToolCall
	Usage     llm.Usage
}

var errServeSessionBusy = errors.New("session is busy processing another request")

func (rt *serveRuntime) Run(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request) (serveRunResult, error) {
	return rt.run(ctx, stateful, replaceHistory, inputMessages, req, nil)
}

func (rt *serveRuntime) RunWithEvents(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onEvent func(llm.Event) error) (serveRunResult, error) {
	return rt.run(ctx, stateful, replaceHistory, inputMessages, req, onEvent)
}

func (rt *serveRuntime) run(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onEvent func(llm.Event) error) (serveRunResult, error) {
	if !rt.mu.TryLock() {
		return serveRunResult{}, errServeSessionBusy
	}
	defer rt.mu.Unlock()
	rt.Touch()
	persisted := rt.ensurePersistedSession(ctx, req.SessionID, inputMessages)
	if persisted {
		rt.persistStatus(ctx, req.SessionID, session.StatusActive)
	}

	if !stateful {
		rt.engine.ResetConversation()
		rt.history = nil
	}

	baseHistory := make([]llm.Message, len(rt.history))
	copy(baseHistory, rt.history)
	if replaceHistory {
		baseHistory = nil
		rt.engine.ResetConversation()
	}

	messages := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
	if rt.systemPrompt != "" && !containsSystemMessage(baseHistory) && !containsSystemMessage(inputMessages) {
		messages = append(messages, llm.SystemText(rt.systemPrompt))
	}
	messages = append(messages, baseHistory...)
	messages = append(messages, inputMessages...)
	req.Messages = messages

	var produced []llm.Message
	rt.engine.SetTurnCompletedCallback(func(_ context.Context, _ int, msgs []llm.Message, _ llm.TurnMetrics) error {
		produced = append(produced, msgs...)
		return nil
	})
	defer rt.engine.SetTurnCompletedCallback(nil)

	stream, err := rt.engine.Stream(ctx, req)
	if err != nil {
		if persisted {
			rt.persistStatus(ctx, req.SessionID, statusForRunError(err))
		}
		return serveRunResult{}, err
	}
	defer stream.Close()

	result := serveRunResult{}
	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if persisted {
				rt.persistStatus(ctx, req.SessionID, statusForRunError(recvErr))
			}
			return serveRunResult{}, recvErr
		}

		if onEvent != nil {
			if err := onEvent(ev); err != nil {
				if persisted {
					rt.persistStatus(ctx, req.SessionID, statusForRunError(err))
				}
				return serveRunResult{}, err
			}
		}

		switch ev.Type {
		case llm.EventTextDelta:
			result.Text.WriteString(ev.Text)
		case llm.EventToolCall:
			if ev.Tool != nil {
				result.ToolCalls = append(result.ToolCalls, *ev.Tool)
			}
		case llm.EventUsage:
			if ev.Use != nil {
				result.Usage.InputTokens += ev.Use.InputTokens
				result.Usage.OutputTokens += ev.Use.OutputTokens
				result.Usage.CachedInputTokens += ev.Use.CachedInputTokens
			}
		case llm.EventError:
			if ev.Err != nil {
				if persisted {
					rt.persistStatus(ctx, req.SessionID, statusForRunError(ev.Err))
				}
				return serveRunResult{}, ev.Err
			}
		}
	}

	newHistory := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+len(produced)+1)
	newHistory = append(newHistory, baseHistory...)
	newHistory = append(newHistory, inputMessages...)
	newHistory = append(newHistory, produced...)
	if len(produced) == 0 && result.Text.Len() > 0 {
		newHistory = append(newHistory, llm.AssistantText(result.Text.String()))
	}
	if stateful {
		rt.history = newHistory
	}
	if persisted {
		rt.persistSnapshot(ctx, req.SessionID, newHistory)
	}

	return result, nil
}

func statusForRunError(err error) session.SessionStatus {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return session.StatusInterrupted
	}
	return session.StatusError
}

func containsSystemMessage(messages []llm.Message) bool {
	for _, msg := range messages {
		if msg.Role == llm.RoleSystem {
			return true
		}
	}
	return false
}

type responsesCreateRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int               `json:"max_output_tokens,omitempty"`
	Temperature       *float32          `json:"temperature,omitempty"`
	TopP              *float32          `json:"top_p,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
}

type chatCompletionsRequest struct {
	Model             string             `json:"model"`
	Messages          []chatMessage      `json:"messages"`
	Tools             []chatTool         `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Temperature       *float32           `json:"temperature,omitempty"`
	TopP              *float32           `json:"top_p,omitempty"`
	MaxTokens         int                `json:"max_tokens,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Name     string           `json:"name,omitempty"`
	Function *chatToolFuncDef `json:"function,omitempty"`
}

type chatToolFuncDef struct {
	Name string `json:"name"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseChatMessages(msgs []chatMessage) ([]llm.Message, bool, error) {
	callNameByID := make(map[string]string)
	result := make([]llm.Message, 0, len(msgs))
	replaceHistory := len(msgs) > 1

	for _, msg := range msgs {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			result = append(result, llm.SystemText(extractMessageText(msg.Content)))
			replaceHistory = true
		case "user":
			result = append(result, llm.UserText(extractMessageText(msg.Content)))
		case "assistant":
			parts := []llm.Part{}
			text := extractMessageText(msg.Content)
			if text != "" {
				parts = append(parts, llm.Part{Type: llm.PartText, Text: text})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				callNameByID[tc.ID] = tc.Function.Name
				parts = append(parts, llm.Part{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: json.RawMessage(args),
					},
				})
			}
			if len(parts) == 0 {
				continue
			}
			result = append(result, llm.Message{Role: llm.RoleAssistant, Parts: parts})
			replaceHistory = true
		case "tool":
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				return nil, false, fmt.Errorf("tool message missing tool_call_id")
			}
			name := callNameByID[callID]
			result = append(result, llm.ToolResultMessage(callID, name, extractMessageText(msg.Content), nil))
			replaceHistory = true
		default:
			return nil, false, fmt.Errorf("unsupported message role: %s", msg.Role)
		}
	}

	return result, replaceHistory, nil
}

func parseResponsesInput(input json.RawMessage) ([]llm.Message, bool, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil, false, fmt.Errorf("input is required")
	}

	// string shorthand
	var inputText string
	if err := json.Unmarshal(input, &inputText); err == nil {
		if strings.TrimSpace(inputText) == "" {
			return nil, false, fmt.Errorf("input is empty")
		}
		return []llm.Message{llm.UserText(inputText)}, false, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, false, fmt.Errorf("invalid input format")
	}

	var messages []llm.Message
	callNameByID := map[string]string{}
	replaceHistory := false
	userCount := 0

	for _, item := range items {
		itemType := jsonString(item["type"])
		switch itemType {
		case "message":
			role := strings.ToLower(strings.TrimSpace(jsonString(item["role"])))
			content := extractItemContent(item["content"])
			switch role {
			case "developer", "system":
				messages = append(messages, llm.SystemText(content))
				replaceHistory = true
			case "assistant":
				messages = append(messages, llm.AssistantText(content))
				replaceHistory = true
			default:
				messages = append(messages, llm.UserText(content))
				userCount++
			}
		case "function_call":
			id := jsonString(item["call_id"])
			name := jsonString(item["name"])
			args := jsonString(item["arguments"])
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			callNameByID[id] = name
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type:     llm.PartToolCall,
				ToolCall: &llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)},
			}}})
			replaceHistory = true
		case "function_call_output":
			id := jsonString(item["call_id"])
			out := jsonString(item["output"])
			messages = append(messages, llm.ToolResultMessage(id, callNameByID[id], out, nil))
			replaceHistory = true
		}
	}

	if userCount > 1 {
		replaceHistory = true
	}
	return messages, replaceHistory, nil
}

func parseRequestedTools(raw []json.RawMessage) (bool, map[string]bool) {
	search := false
	toolNames := map[string]bool{}

	for _, item := range raw {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(item, &generic); err != nil {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(jsonString(generic["type"])))
		switch typeName {
		case "web_search_preview", "web_search":
			search = true
		case "function":
			name := strings.TrimSpace(jsonString(generic["name"]))
			if name == "" {
				var fn chatToolFuncDef
				if rawFunc := generic["function"]; len(rawFunc) > 0 {
					_ = json.Unmarshal(rawFunc, &fn)
					name = strings.TrimSpace(fn.Name)
				}
			}
			if name != "" {
				toolNames[name] = true
			}
		}
	}

	return search, toolNames
}

func parseChatRequestedToolNames(tools []chatTool) map[string]bool {
	selected := map[string]bool{}
	for _, t := range tools {
		if strings.ToLower(t.Type) != "function" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name == "" && t.Function != nil {
			name = strings.TrimSpace(t.Function.Name)
		}
		if name != "" {
			selected[name] = true
		}
	}
	return selected
}

func parseToolChoice(raw json.RawMessage) llm.ToolChoice {
	if len(raw) == 0 {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "none":
			return llm.ToolChoice{Mode: llm.ToolChoiceNone}
		case "required":
			return llm.ToolChoice{Mode: llm.ToolChoiceRequired}
		default:
			return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		}
	}

	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if strings.ToLower(strings.TrimSpace(obj.Type)) == "function" {
			name := strings.TrimSpace(obj.Name)
			if name == "" {
				name = strings.TrimSpace(obj.Function.Name)
			}
			if name != "" {
				return llm.ToolChoice{Mode: llm.ToolChoiceName, Name: name}
			}
		}
	}
	return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
}

func extractMessageText(content json.RawMessage) string {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			pType := strings.ToLower(strings.TrimSpace(jsonString(p["type"])))
			switch pType {
			case "text", "input_text", "output_text":
				b.WriteString(jsonString(p["text"]))
			}
		}
		return b.String()
	}
	return ""
}

func extractItemContent(content json.RawMessage) string {
	return extractMessageText(content)
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func writeOpenAIError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 10<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func resolveRequestSessionID(w http.ResponseWriter, r *http.Request) string {
	sessionID := strings.TrimSpace(r.Header.Get("session_id"))
	if sessionID == "" {
		sessionID = session.NewID()
		w.Header().Set("x-session-id", sessionID)
	}
	return sessionID
}

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("invalid Content-Type header")
	}
	if mediaType != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	return nil
}

func responsesFinalResponse(result serveRunResult, model string) map[string]any {
	output := []map[string]any{}
	if result.Text.Len() > 0 {
		output = append(output, map[string]any{
			"id":   "msg_" + randomSuffix(),
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": result.Text.String(),
			}},
		})
	}
	for _, call := range result.ToolCalls {
		output = append(output, map[string]any{
			"id":        "fc_" + call.ID,
			"type":      "function_call",
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": string(call.Arguments),
		})
	}

	return map[string]any{
		"id":      "resp_" + randomSuffix(),
		"object":  "response",
		"created": time.Now().Unix(),
		"model":   model,
		"output":  output,
		"usage": map[string]any{
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.InputTokens + result.Usage.OutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": result.Usage.CachedInputTokens,
			},
		},
	}
}

func chatCompletionFinalResponse(result serveRunResult, model string) map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text.String(),
	}
	finishReason := "stop"
	if len(result.ToolCalls) > 0 {
		finishReason = "tool_calls"
		toolCalls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": string(call.Arguments),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	return map[string]any{
		"id":      "chatcmpl_" + randomSuffix(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     result.Usage.InputTokens,
			"completion_tokens": result.Usage.OutputTokens,
			"total_tokens":      result.Usage.InputTokens + result.Usage.OutputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": result.Usage.CachedInputTokens,
			},
		},
	}
}

func sessionOrRandomID(sessionID string) string {
	if sessionID != "" {
		return sanitizeID(sessionID)
	}
	return randomSuffix()
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return randomSuffix()
	}
	return b.String()
}

func randomSuffix() string {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
