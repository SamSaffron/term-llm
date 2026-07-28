package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/spf13/cobra"
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

Hub auth is intentionally simple: --auth bearer (the default) protects the
dashboard, registry API, and node proxy with one Hub bearer token.
/api/connect and node-originated delegation calls use node auth instead. Use
--auth none only for loopback-only local development.`,
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

func runServeHub(cmd *cobra.Command, args []string) error {
	authMode, err := resolveServeAuthMode(cmd.Flags().Changed("auth"), serveHubAuthMode, false, false)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if err := validateHubBind(serveHubHost, serveHubPort, requireAuth); err != nil {
		return err
	}
	hubBasePath, err := normalizeHubBasePath(serveHubBasePath)
	if err != nil {
		return err
	}
	token, tokenSource, err := resolveServeToken(serveHubToken, os.Getenv("TERM_LLM_HUB_TOKEN"), requireAuth, generateServeToken)
	if err != nil {
		return err
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
	s.token = token
	s.registrationToken = resolveServeHubRegistrationToken(serveHubRegistrationTokenFlag)
	s.basePath = hubBasePath
	// The delegation ledger lives beside the node store (same private dir).
	s.delegations = hub.NewDelegationStore(filepath.Join(filepath.Dir(nodesFile), "delegations.json"))
	addr := net.JoinHostPort(serveHubHost, strconv.Itoa(serveHubPort))
	srv := &http.Server{Addr: addr, Handler: s.handler()}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "term-llm Hub listening on http://%s%s\n", addr, s.hubPath("/"))
	fmt.Fprintf(out, "  GET http://%s%s\n", addr, s.hubPath("/api/nodes"))
	fmt.Fprintf(out, "  ANY http://%s%s\n", addr, s.hubPath("/node/<id>/..."))
	fmt.Fprintf(out, "  node store: %s\n", nodesFile)
	fmt.Fprintf(out, "  auth: %s\n", authSummary(requireAuth))
	if requireAuth {
		switch tokenSource {
		case tokenSourceGenerated:
			fmt.Fprintf(out, "  generated Hub bearer token: %s\n", token)
		case tokenSourceEnv:
			fmt.Fprintln(out, "  Hub bearer token: from TERM_LLM_HUB_TOKEN")
		case tokenSourceFlag:
			fmt.Fprintln(out, "  Hub bearer token: from --token")
		}
	} else {
		fmt.Fprintln(out, "WARNING: hub auth disabled; bind to loopback only.")
	}
	if s.registrationToken != "" {
		fmt.Fprintln(out, "  registration: enabled")
	}
	return srv.ListenAndServe()
}

func init() {
	serveCmd.AddCommand(serveHubCmd)
	serveHubCmd.Flags().StringVar(&serveHubHost, "host", "127.0.0.1", "Host to bind")
	serveHubCmd.Flags().IntVar(&serveHubPort, "port", 8090, "Port to bind")
	serveHubCmd.Flags().StringVar(&serveHubConfig, "config", "", "Path to a static nodes config file (YAML or JSON)")
	serveHubCmd.Flags().BoolVar(&serveHubContain, "contain", true, "Discover nodes from local contain workspaces")
	serveHubCmd.Flags().StringVar(&serveHubNodesFile, "nodes-file", "", "Path to the JSON store for dashboard-added nodes (default: <data-dir>/hub/nodes.json)")
	serveHubCmd.Flags().StringVar(&serveHubAuthMode, "auth", "bearer", "Hub auth mode: bearer or none (none is loopback-only)")
	serveHubCmd.Flags().StringVar(&serveHubToken, "token", "", "Hub bearer token (defaults to $TERM_LLM_HUB_TOKEN, else auto-generated)")
	serveHubCmd.Flags().StringVar(&serveHubBasePath, "base-path", "/", "URL prefix for the Hub dashboard/API when mounted behind a reverse proxy (e.g. /hub)")
	serveHubCmd.Flags().StringVar(&serveHubRegistrationTokenFlag, "registration-token", "", "Token that allows reverse nodes to self-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN; empty disables registration)")
}
