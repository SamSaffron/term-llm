package cmd

import (
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/spf13/cobra"
)

var serveHubFlags apphub.Options

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
  PATCH /api/nodes/order  reorder dashboard nodes (Hub auth or node auth)
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
issues expiring server-side browser sessions. /api/connect, node-originated
delegation calls, and node-saved dashboard order changes continue to use
independent node auth. Use --auth none only for loopback-only local
development.`,
	Args: cobra.NoArgs,
	RunE: runServeHub,
}

func runServeHub(cmd *cobra.Command, args []string) error {
	return appserve.RunHub(cmd.Context(), serveHubFlags, apphub.Invocation{Flags: apphub.FlagState{BasePath: cmd.Flags().Changed("base-path")}, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
}

func init() {
	serveCmd.AddCommand(serveHubCmd)
	serveHubCmd.Flags().StringVar(&serveHubFlags.Host, "host", "127.0.0.1", "Host to bind")
	serveHubCmd.Flags().IntVar(&serveHubFlags.Port, "port", 8090, "Port to bind")
	serveHubCmd.Flags().StringVar(&serveHubFlags.Config, "config", "", "Path to a static nodes config file (YAML or JSON)")
	serveHubCmd.Flags().BoolVar(&serveHubFlags.Contain, "contain", true, "Discover nodes from local contain workspaces")
	serveHubCmd.Flags().StringVar(&serveHubFlags.NodesFile, "nodes-file", "", "Path to the JSON store for dashboard-added nodes (default: <data-dir>/hub/nodes.json)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.AuthMode, "auth", "bearer", "Hub auth mode: bearer, passkey, or none (none is loopback-only)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.Token, "token", "", "Hub bearer token (auto-generated in bearer mode; optional explicit API token in passkey mode)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.BasePath, "base-path", "/", "URL prefix for the Hub dashboard/API when mounted behind a reverse proxy (e.g. /hub)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.PublicURL, "public-url", "", "Stable browser-visible URL required for passkey auth (or $TERM_LLM_HUB_PUBLIC_URL)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.PasskeyAuthFile, "passkey-auth-file", "", "Passkey credential store (default: <data-dir>/hub/auth.json)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.BootstrapTokenFile, "passkey-bootstrap-token-file", "", "Private file containing the first-passkey setup secret")
	serveHubCmd.Flags().StringVar(&serveHubFlags.RecoveryTokenFile, "passkey-recovery-token-file", "", "Private file containing a short-lived passkey recovery secret")
	serveHubCmd.Flags().BoolVar(&serveHubFlags.PrintBootstrapToken, "print-passkey-bootstrap-token", false, "Print a generated first-passkey setup code even when output is not interactive (unsafe for service logs)")
	serveHubCmd.Flags().StringSliceVar(&serveHubFlags.PasskeyTrustedProxies, "passkey-trusted-proxy", nil, "Trusted reverse-proxy IP or CIDR allowed to supply X-Forwarded-For (repeatable)")
	serveHubCmd.Flags().StringVar(&serveHubFlags.RegistrationToken, "registration-token", "", "Token that allows reverse nodes to self-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN; empty disables registration)")
}
