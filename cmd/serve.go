package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	platform "github.com/samsaffron/term-llm/internal/serve"
	"github.com/spf13/cobra"
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
	bindServeFlags(serveCmd, &serveFlags)
}

func bindServeFlags(cmd *cobra.Command, opts *ServeOptions) {
	cmd.Flags().StringVar(&opts.Host, "host", "127.0.0.1", "Bind host")
	cmd.Flags().IntVar(&opts.Port, "port", 8080, "Bind port")
	cmd.Flags().StringVar(&opts.Token, "token", "", "Bearer token for API auth (defaults to $TERM_LLM_SERVE_TOKEN, else auto-generated)")
	cmd.Flags().BoolVar(&opts.AllowNoAuth, "no-auth", false, "Disable auth (only allowed on loopback host)")
	cmd.Flags().BoolVar(&opts.AllowNoAuth, "allow-no-auth", false, "Disable auth (alias for --no-auth)")
	_ = cmd.Flags().MarkHidden("allow-no-auth")
	cmd.Flags().StringVar(&opts.AuthMode, "auth", "bearer", "Auth mode: bearer, passkey (web only), or none")
	cmd.Flags().StringVar(&opts.BasePath, "base-path", "/ui", "URL prefix the UI uses for session URLs (e.g. /chat)")
	cmd.Flags().StringVar(&opts.PublicURL, "public-url", "", "Browser-visible URL required for passkeys, also used for OAuth callbacks (defaults to $TERM_LLM_SERVE_PUBLIC_URL or the authenticated request origin)")
	cmd.Flags().StringVar(&opts.Title, "title", "", "Override the web UI sidebar title (defaults to agent name or Chat)")
	cmd.Flags().BoolVar(&opts.DisableLocationSharing, "disable-location-sharing", false, "Hide the web UI action for sharing the browser's current location")
	cmd.Flags().StringArrayVar(&opts.CORSOrigins, "cors-origin", nil, "Allowed CORS origin (repeatable, or '*' for all)")
	cmd.Flags().DurationVar(&opts.SessionTTL, "session-ttl", 30*time.Minute, "Stateful session idle TTL")
	cmd.Flags().IntVar(&opts.SessionMax, "session-max", 1000, "Max stateful sessions in memory")

	cmd.Flags().BoolVar(&opts.Setup, "setup", false, "Re-run setup wizard for selected platforms")
	cmd.Flags().IntVar(&opts.TelegramCarryoverChars, "telegram-carryover-chars", 4000, "Characters of previous Telegram session context to carry into replacement sessions (0 disables)")
	cmd.Flags().IntVar(&opts.JobsWorkers, "jobs-workers", 4, "Number of concurrent job workers for --platform jobs")
	cmd.Flags().StringVar(&opts.SidebarSessions, "sidebar-sessions", "all", "Default web sidebar session categories: all or a comma-separated list like chat,web,ask,plan,exec")
	cmd.Flags().BoolVar(&opts.Verbose, "verbose", false, "Log API request/response summaries to stderr")
	cmd.Flags().StringArrayVar(&opts.ToolMap, "tool-map", nil, "Map client tool name to server tool (repeatable, format ClientName:ServerName)")
	cmd.Flags().BoolVar(&opts.FilterServerTools, "suppress-server-tool-calls", false, "Hide server-executed tool calls from API responses (use when proxying to external clients)")
	cmd.Flags().StringVar(&opts.FilesDir, "files-dir", "", "Directory for serving arbitrary files (videos, PDFs, etc) at {base}/files/")
	cmd.Flags().Bool("enable-widgets", false, "Deprecated no-op; widgets are enabled by default")
	cmd.Flags().BoolVar(&opts.DisableWidgets, "disable-widgets", false, "Disable local widget apps under {base}/widgets/<mount>/")
	cmd.Flags().StringSliceVar(&opts.ExtensionIDs, "extensions", nil, "Ordered web extension IDs (overrides serve.extensions; empty disables all)")
	cmd.Flags().StringVar(&opts.ExtensionsDir, "extensions-dir", "", "Directory containing web extension sub-directories")
	cmd.Flags().BoolVar(&opts.DisableExtensions, "disable-extensions", false, "Disable all web extension code for this process")
	cmd.Flags().StringVar(&opts.WidgetsDir, "widgets-dir", "", "Directory containing widget sub-directories (default: ~/.config/term-llm/widgets)")
	cmd.Flags().DurationVar(&opts.ResponseTimeout, "response-timeout", appserve.DefaultServeRequestTimeout, "Maximum inactivity before the first or next completed LLM response (default 30m; pauses for interactive waits)")
	cmd.Flags().BoolVar(&opts.EnableFileTracking, "enable-file-tracking", false, "Enable session file-change tracking for this serve process")
	cmd.Flags().BoolVar(&opts.Projects, "projects", false, "Strictly enable Web UI project selection")
	cmd.Flags().BoolVar(&opts.NoProjects, "no-projects", false, "Use the single-workspace Web UI for dedicated agent deployments")
	cmd.Flags().StringVar(&opts.HubURL, "hub-url", "", "URL of the term-llm Hub this node belongs to (renders a Back to Hub link in the web UI)")
	cmd.Flags().StringVar(&opts.HubNodeID, "hub-node-id", "", "This node's id on the hub (used with --hub-url)")
	cmd.Flags().StringVar(&opts.HubNodeName, "hub-node-name", "", "This node's display name on the hub (used with --hub-url)")
	cmd.Flags().StringVar(&opts.HubConnect, "hub-connect", "direct", "Hub connection mode for this node: direct or reverse")
	cmd.Flags().BoolVar(&opts.HubRegister, "hub-register", false, "Register this reverse node with the Hub before connecting")
	cmd.Flags().StringVar(&opts.HubRegistrationToken, "hub-registration-token", "", "Hub registration token for --hub-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN)")

	AddCommonFlags(cmd,
		CommonCoreFlags|CommonSearch|CommonNativeSearch|CommonMaxTurns|CommonAgent|CommonProjectSkillTools,
		CommonFlagBindings{
			Provider:        &opts.Provider,
			Debug:           &opts.Debug,
			Search:          &opts.Search,
			NoSearch:        &opts.NoSearch,
			NativeSearch:    &opts.NativeSearch,
			NoNativeSearch:  &opts.NoNativeSearch,
			MCP:             &opts.MCP,
			MaxTurns:        &opts.MaxTurns,
			MaxTurnsDefault: 200,
			Tools:           &opts.Tools,
			ReadDirs:        &opts.ReadDirs,
			WriteDirs:       &opts.WriteDirs,
			ShellAllow:      &opts.ShellAllow,
			SystemMessage:   &opts.SystemMessage,
			Agent:           &opts.Agent,
			Approval:        &opts.Approval,
			Yolo:            &opts.Yolo,
			Auto:            &opts.Auto,
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
	invocation := captureServeInvocation(cmd, &serveFlags)
	warnDeprecatedEnableWidgets(invocation.Flags.EnableWidgets, invocation.Stderr)

	svc, err := platform.NewService(platform.Options{
		Host:      serveFlags.Host,
		Port:      serveFlags.Port,
		BasePath:  serveFlags.BasePath,
		Title:     serveFlags.Title,
		Platforms: append([]string(nil), args...),
		Runner: func(ctx context.Context) error {
			return appserve.Run(ctx, &serveFlags, args, invocation)
		},
	})
	if err != nil {
		return err
	}
	return svc.Run(cmd.Context())
}

func warnDeprecatedEnableWidgets(changed bool, stderr io.Writer) {
	if changed {
		fmt.Fprintln(stderr, "Warning: --enable-widgets is deprecated and has no effect; widgets are enabled by default. Use --disable-widgets to opt out.")
	}
}
