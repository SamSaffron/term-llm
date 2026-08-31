package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	serveHubHost                  string
	serveHubPort                  int
	serveHubConfig                string
	serveHubContain               bool
	serveHubNodesFile             string
	serveHubAuthMode              string
	serveHubToken                 string
	serveHubRegistrationTokenFlag string
	serveHubBasePath              string
	serveHubPublicURL             string
	serveHubPasskeyAuthFile       string
	serveHubBootstrapTokenFile    string
	serveHubRecoveryTokenFile     string
	serveHubPrintBootstrapToken   bool
	serveHubPasskeyTrustedProxies []string
)

var serveHubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Run the term-llm Hub: one dashboard over many term-llm web nodes",
	Long: `Run the term-llm Hub, a launcher and control plane over many term-llm web
nodes (serves). Nodes are discovered from a static config file (--config),
from local contain workspaces, and from nodes added in the dashboard UI
(persisted to a local JSON store).

The dashboard lists every node with live reachability, latency, and any
detected agent/version/capabilities, and opens a node's full web UI through
the hub at /node/<id>/ with the node's bearer token injected server-side —
node tokens never reach the browser.

Routes (root-mounted by default; when --base-path is set, prefix each route):
  GET  /                  hub dashboard
  GET  /api/nodes         list nodes with probe status (never includes tokens)
  POST /api/nodes         add a node to the local store
  DELETE /api/nodes/<id>  remove a local-store node
  POST /api/nodes/test    probe a node spec without persisting it
  POST /api/register-node register/update a reverse node (registration token)
  DELETE /api/register-node/<id> deregister a reverse node (registration token)
  GET  /api/connect       reverse-node websocket endpoint (node auth)
  ANY  /node/<id>/...     reverse proxy to that node's serve
  POST /api/delegations   create a cross-node delegation (node auth)
  GET  /api/delegations   list delegations
  GET  /api/delegations/<id>         delegation status
  POST /api/delegations/<id>/cancel  cancel (originating node only)

Config file (--config), YAML or JSON:
  nodes:
    - name: jarvis
      url: http://127.0.0.1:8081/chat
      token: <web bearer token>

Hub auth defaults to --auth bearer for compatibility. Public browser-facing
Hubs can use --auth passkey with a stable HTTPS --public-url; WebAuthn then
issues expiring server-side browser sessions. /api/connect and node-originated
delegation calls continue to use independent node auth. Use --auth none only
for loopback-only local development.`,
	Args: cobra.NoArgs,
	RunE: runServeHub,
}

// validateHubBind rejects unauthenticated public binds. A Hub with bearer auth
// may bind publicly for use behind a reverse proxy, but --auth none stays
// loopback-only because the Hub injects node tokens server-side.
func validateHubBind(host string, port int, requireAuth bool) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", port)
	}
	if !requireAuth && !isLoopbackHost(host) {
		return fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", host)
	}
	return nil
}

func normalizeHubBasePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return "", nil
	}
	if strings.Contains(p, "://") || strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("--base-path must be a URL path such as /hub, not %q", raw)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if hubHasDotDotSegment(p) {
		return "", fmt.Errorf("--base-path must not contain .. segments")
	}
	p = path.Clean(p)
	if p == "/" || p == "." {
		return "", nil
	}
	return p, nil
}

// defaultHubNodesFile is where dashboard-added nodes persist when
// --nodes-file is not given.
func defaultHubNodesFile() (string, error) {
	dir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub", "nodes.json"), nil
}

func defaultHubAuthFile() (string, error) {
	dir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub", "auth.json"), nil
}

func lockHubPasskeyState(authFile string) (func() error, error) {
	dir := filepath.Dir(authFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create Hub passkey directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("passkey auth directory must have private permissions (0700)")
		}
	}
	unlock, err := filelock.TryLock(filepath.Join(dir, "hub-auth.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock Hub passkey state (another Hub process may be using %s): %w", dir, err)
	}
	return unlock, nil
}

func runServeHub(cmd *cobra.Command, args []string) error {
	authMode, err := resolveHubAuthMode(serveHubAuthMode)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if err := validateHubBind(serveHubHost, serveHubPort, requireAuth); err != nil {
		return err
	}
	var endpoint passkeyauth.Endpoint
	if authMode == "passkey" {
		publicURL := strings.TrimSpace(serveHubPublicURL)
		if publicURL == "" {
			publicURL = strings.TrimSpace(os.Getenv("TERM_LLM_HUB_PUBLIC_URL"))
		}
		endpoint, err = passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: publicURL, BasePath: serveHubBasePath, BasePathExplicit: cmd.Flags().Changed("base-path")})
		if err != nil {
			return fmt.Errorf("invalid Hub passkey --public-url/--base-path configuration: %w", err)
		}
	}
	hubBasePath, err := normalizeHubBasePath(serveHubBasePath)
	if authMode == "passkey" {
		hubBasePath = endpoint.BasePath
	}
	if err != nil {
		return err
	}
	hubTokenEnv := strings.TrimSpace(os.Getenv("TERM_LLM_HUB_TOKEN"))
	if hubTokenEnv == "" {
		hubTokenEnv = strings.TrimSpace(tools.HubTokenFromEnvironment())
	}
	var token, tokenSource string
	if authMode == "passkey" {
		if t := strings.TrimSpace(serveHubToken); t != "" {
			token, tokenSource = t, tokenSourceFlag
		} else if hubTokenEnv != "" {
			token, tokenSource = hubTokenEnv, tokenSourceEnv
		}
	} else {
		token, tokenSource, err = resolveServeToken(serveHubToken, hubTokenEnv, requireAuth, generateServeToken)
		if err != nil {
			return err
		}
	}

	var resolvers []hub.Resolver
	if strings.TrimSpace(serveHubConfig) != "" {
		resolvers = append(resolvers, hub.NewStaticResolver(serveHubConfig))
	}
	nodesFile := strings.TrimSpace(serveHubNodesFile)
	if nodesFile == "" {
		var err error
		nodesFile, err = defaultHubNodesFile()
		if err != nil {
			return fmt.Errorf("resolve hub nodes file: %w", err)
		}
	}
	store := hub.NewStore(nodesFile)
	resolvers = append(resolvers, store)
	if serveHubContain {
		resolvers = append(resolvers, hub.NewContainResolver())
	}

	s := newHubServer(hub.NewRegistry(resolvers...), store)
	s.requireAuth = requireAuth
	s.authMode = authMode
	s.token = token
	s.registrationToken = resolveServeHubRegistrationToken(serveHubRegistrationTokenFlag)
	s.basePath = hubBasePath
	// The delegation ledger lives beside the node store (same private dir).
	s.delegations = hub.NewDelegationStore(filepath.Join(filepath.Dir(nodesFile), "delegations.json"))
	attentionStore, err := hub.OpenAttentionProjectionStore(filepath.Join(filepath.Dir(nodesFile), "attention.db"))
	if err != nil {
		return fmt.Errorf("open Hub attention projection: %w", err)
	}
	defer attentionStore.Close()
	s.attentionStore = attentionStore
	s.startAttentionCollector()
	defer func() {
		if s.attentionCancel != nil {
			s.attentionCancel()
			s.attentionWG.Wait()
		}
	}()
	bootstrapDisplay := ""
	if authMode == "passkey" {
		authFile := strings.TrimSpace(serveHubPasskeyAuthFile)
		if authFile == "" {
			authFile, err = defaultHubAuthFile()
			if err != nil {
				return fmt.Errorf("resolve Hub passkey auth file: %w", err)
			}
		}
		unlockPasskeyState, err := lockHubPasskeyState(authFile)
		if err != nil {
			return err
		}
		defer unlockPasskeyState()
		authStore, err := passkeyauth.OpenStore(passkeyauth.StoreOptions{Path: authFile, RPID: endpoint.RPID, UserName: hubPasskeyUserName, Warnf: func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), "SECURITY: "+format+"\n", args...) }})
		if err != nil {
			return err
		}
		sessionFile := filepath.Join(filepath.Dir(authFile), "sessions.json")
		sessions, err := passkeyauth.OpenSessions(passkeyauth.SessionsOptions{
			Path:            sessionFile,
			RPID:            endpoint.RPID,
			UserID:          authStore.User().ID,
			ValidCredential: authStore.HasCredential,
			Warnf:           func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), "SECURITY: "+format+"\n", args...) },
		})
		if err != nil {
			return err
		}
		defer sessions.Close()
		bootstrapSecret, display, err := resolveHubBootstrapSecret(cmd, authStore.CredentialCount() == 0)
		if err != nil {
			return err
		}
		bootstrapDisplay = display
		bootstrapGrants, err := passkeyauth.NewGrants(passkeyauth.GrantBootstrap, bootstrapSecret, nil, nil)
		for i := range bootstrapSecret {
			bootstrapSecret[i] = 0
		}
		if err != nil {
			return err
		}
		recoverySecret, err := resolveHubRecoverySecret(authStore.CredentialCount() > 0)
		if err != nil {
			return err
		}
		recoveryGrants, err := passkeyauth.NewGrants(passkeyauth.GrantRecovery, recoverySecret, nil, nil)
		for i := range recoverySecret {
			recoverySecret[i] = 0
		}
		if err != nil {
			return err
		}
		peerResolver, err := newHubClientPeerResolver(serveHubPasskeyTrustedProxies)
		if err != nil {
			return err
		}
		s.passkey, err = newHubPasskeyRuntime(endpoint, authStore, sessions, bootstrapGrants, recoveryGrants, peerResolver)
		if err != nil {
			return err
		}
	}
	addr := net.JoinHostPort(serveHubHost, strconv.Itoa(serveHubPort))
	srv := &http.Server{Addr: addr, Handler: s.handler()}

	out := cmd.OutOrStdout()
	if authMode == "passkey" {
		fmt.Fprintf(out, "term-llm Hub backend listening on http://%s%s\n", addr, s.hubPath("/"))
	} else {
		fmt.Fprintf(out, "term-llm Hub listening on http://%s%s\n", addr, s.hubPath("/"))
	}
	fmt.Fprintf(out, "  GET http://%s%s\n", addr, s.hubPath("/api/nodes"))
	fmt.Fprintf(out, "  ANY http://%s%s\n", addr, s.hubPath("/node/<id>/..."))
	fmt.Fprintf(out, "  node store: %s\n", nodesFile)
	if authMode == "passkey" {
		fmt.Fprintln(out, "  auth: passkey")
		fmt.Fprintf(out, "  browser URL: %s\n", endpoint.URL.String())
		if bearerSummary := hubPasskeyBearerCompatibilitySummary(tokenSource); bearerSummary != "" {
			fmt.Fprintf(out, "  explicit bearer API compatibility: enabled (%s)\n", bearerSummary)
		}
		if s.passkey.store.CredentialCount() == 0 {
			fmt.Fprintln(out, "  first-passkey setup required")
		}
		if bootstrapDisplay != "" {
			fmt.Fprintf(out, "\nOpen %s\n", endpoint.URL.ResolveReference(&url.URL{Path: s.publicPath("/auth/setup")}).String())
			fmt.Fprintf(out, "Enter one-time setup code: %s\n", bootstrapDisplay)
			fmt.Fprintln(out, "The code expires in 10 minutes and can create exactly one passkey.")
		}
	} else {
		fmt.Fprintf(out, "  auth: %s\n", authSummary(requireAuth))
	}
	if requireAuth && authMode != "passkey" {
		switch tokenSource {
		case tokenSourceGenerated:
			fmt.Fprintf(out, "  generated Hub bearer token: %s\n", token)
		case tokenSourceEnv:
			fmt.Fprintln(out, "  Hub bearer token: from TERM_LLM_HUB_TOKEN")
		case tokenSourceFlag:
			fmt.Fprintln(out, "  Hub bearer token: from --token")
		}
	} else if !requireAuth {
		fmt.Fprintln(out, "WARNING: hub auth disabled; bind to loopback only.")
	}
	if s.registrationToken != "" {
		fmt.Fprintln(out, "  registration: enabled")
	}
	return srv.ListenAndServe()
}

var hubOutputIsTerminal = func(w any) bool { f, ok := w.(interface{ Fd() uintptr }); return ok && term.IsTerminal(int(f.Fd())) }

func hubPasskeyBearerCompatibilitySummary(source string) string {
	switch source {
	case tokenSourceFlag:
		return "from --token"
	case tokenSourceEnv:
		return "from TERM_LLM_HUB_TOKEN"
	default:
		return ""
	}
}

func resolveHubAuthMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		mode = "bearer"
	}
	if mode != "bearer" && mode != "none" && mode != "passkey" {
		return "", fmt.Errorf("invalid --auth %q (must be bearer, passkey, or none)", raw)
	}
	return mode, nil
}
func resolveHubBootstrapSecret(cmd *cobra.Command, needed bool) ([]byte, string, error) {
	env := os.Getenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN")
	_ = os.Unsetenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN")
	if !needed {
		return nil, "", nil
	}
	if p := strings.TrimSpace(serveHubBootstrapTokenFile); p != "" {
		secret, err := passkeyauth.ReadPrivateSecretFile(p)
		return secret, "", err
	}
	if strings.TrimSpace(env) != "" {
		secret := []byte(strings.TrimSpace(env))
		if err := passkeyauth.ValidateHostSecret(secret); err != nil {
			return nil, "", err
		}
		return secret, "", nil
	}
	interactive := hubOutputIsTerminal(cmd.OutOrStdout())
	if !interactive && !serveHubPrintBootstrapToken {
		return nil, "", fmt.Errorf("first-passkey setup requires --passkey-bootstrap-token-file or TERM_LLM_HUB_BOOTSTRAP_TOKEN when output is non-interactive (or explicitly use --print-passkey-bootstrap-token)")
	}
	secret, display, err := passkeyauth.GenerateBootstrapSecret(nil)
	if err != nil {
		return nil, "", err
	}
	if !interactive {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: printing a passkey bootstrap code to non-interactive output; service logs are temporary enrollment credentials")
	}
	return secret, display, nil
}
func resolveHubRecoverySecret(enabled bool) ([]byte, error) {
	env := os.Getenv("TERM_LLM_HUB_RECOVERY_TOKEN")
	_ = os.Unsetenv("TERM_LLM_HUB_RECOVERY_TOKEN")
	if !enabled {
		return nil, nil
	}
	if p := strings.TrimSpace(serveHubRecoveryTokenFile); p != "" {
		return passkeyauth.ReadPrivateSecretFile(p)
	}
	if strings.TrimSpace(env) == "" {
		return nil, nil
	}
	secret := []byte(strings.TrimSpace(env))
	if err := passkeyauth.ValidateHostSecret(secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func init() {
	serveCmd.AddCommand(serveHubCmd)
	serveHubCmd.Flags().StringVar(&serveHubHost, "host", "127.0.0.1", "Host to bind")
	serveHubCmd.Flags().IntVar(&serveHubPort, "port", 8090, "Port to bind")
	serveHubCmd.Flags().StringVar(&serveHubConfig, "config", "", "Path to a static nodes config file (YAML or JSON)")
	serveHubCmd.Flags().BoolVar(&serveHubContain, "contain", true, "Discover nodes from local contain workspaces")
	serveHubCmd.Flags().StringVar(&serveHubNodesFile, "nodes-file", "", "Path to the JSON store for dashboard-added nodes (default: <data-dir>/hub/nodes.json)")
	serveHubCmd.Flags().StringVar(&serveHubAuthMode, "auth", "bearer", "Hub auth mode: bearer, passkey, or none (none is loopback-only)")
	serveHubCmd.Flags().StringVar(&serveHubToken, "token", "", "Hub bearer token (auto-generated in bearer mode; optional explicit API token in passkey mode)")
	serveHubCmd.Flags().StringVar(&serveHubBasePath, "base-path", "/", "URL prefix for the Hub dashboard/API when mounted behind a reverse proxy (e.g. /hub)")
	serveHubCmd.Flags().StringVar(&serveHubPublicURL, "public-url", "", "Stable browser-visible URL required for passkey auth (or $TERM_LLM_HUB_PUBLIC_URL)")
	serveHubCmd.Flags().StringVar(&serveHubPasskeyAuthFile, "passkey-auth-file", "", "Passkey credential store (default: <data-dir>/hub/auth.json)")
	serveHubCmd.Flags().StringVar(&serveHubBootstrapTokenFile, "passkey-bootstrap-token-file", "", "Private file containing the first-passkey setup secret")
	serveHubCmd.Flags().StringVar(&serveHubRecoveryTokenFile, "passkey-recovery-token-file", "", "Private file containing a short-lived passkey recovery secret")
	serveHubCmd.Flags().BoolVar(&serveHubPrintBootstrapToken, "print-passkey-bootstrap-token", false, "Print a generated first-passkey setup code even when output is not interactive (unsafe for service logs)")
	serveHubCmd.Flags().StringSliceVar(&serveHubPasskeyTrustedProxies, "passkey-trusted-proxy", nil, "Trusted reverse-proxy IP or CIDR allowed to supply X-Forwarded-For (repeatable)")
	serveHubCmd.Flags().StringVar(&serveHubRegistrationTokenFlag, "registration-token", "", "Token that allows reverse nodes to self-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN; empty disables registration)")
}
