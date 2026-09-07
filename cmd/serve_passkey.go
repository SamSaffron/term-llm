package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/spf13/cobra"
)

var (
	servePasskeyAuthFile       string
	servePasskeyBootstrapFile  string
	servePasskeyRecoveryFile   string
	servePrintPasskeyBootstrap bool
	servePasskeyTrustedProxies []string
)

func init() {
	f := serveCmd.Flags()
	f.StringVar(&servePasskeyAuthFile, "passkey-auth-file", "", "Private Web passkey store (default: <data-dir>/web-auth/auth.json)")
	f.StringVar(&servePasskeyBootstrapFile, "passkey-bootstrap-token-file", "", "Private file containing the first-passkey setup secret")
	f.StringVar(&servePasskeyRecoveryFile, "passkey-recovery-token-file", "", "Private file containing a short-lived passkey recovery secret")
	f.BoolVar(&servePrintPasskeyBootstrap, "print-passkey-bootstrap-token", false, "Explicitly allow printing the one-time setup code to non-interactive output")
	f.StringSliceVar(&servePasskeyTrustedProxies, "passkey-trusted-proxy", nil, "Trusted proxy IP/CIDR for passkey rate limits (repeatable; never determines the public origin)")
}

func resolveWebPasskeyEndpoint(cmd *cobra.Command) (passkeyauth.Endpoint, error) {
	if serveToken != "" || os.Getenv("TERM_LLM_SERVE_TOKEN") != "" {
		return passkeyauth.Endpoint{}, fmt.Errorf("--auth passkey does not accept --token or TERM_LLM_SERVE_TOKEN; use bearer mode for API clients")
	}
	if serveWebRTC || serveHubURL != "" || serveHubRegister || strings.EqualFold(strings.TrimSpace(serveHubConnect), "reverse") || len(serveCORSOrigins) > 0 {
		return passkeyauth.Endpoint{}, fmt.Errorf("--auth passkey supports direct, same-origin Web access only; Hub, WebRTC and --cors-origin are not supported")
	}
	publicURL := strings.TrimSpace(servePublicURL)
	if publicURL == "" {
		publicURL = strings.TrimSpace(os.Getenv("TERM_LLM_SERVE_PUBLIC_URL"))
	}
	endpoint, err := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: publicURL, BasePath: serveBasePath, BasePathExplicit: cmd.Flags().Changed("base-path")})
	if err != nil {
		return endpoint, fmt.Errorf("invalid Web passkey --public-url/--base-path configuration: %w", err)
	}
	if endpoint.BasePath == "" {
		return endpoint, fmt.Errorf("Web passkeys require a non-root public URL path such as /ui/")
	}
	return endpoint, nil
}

func validateWebPasskeyConfigPath(cmd *cobra.Command, endpoint passkeyauth.Endpoint, configured string) error {
	if !cmd.Flags().Changed("base-path") && configured != "" {
		_, err := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: endpoint.URL.String(), BasePath: configured, BasePathExplicit: true})
		if err != nil {
			return fmt.Errorf("serve.base_path conflicts with passkey --public-url: %w", err)
		}
	}
	return nil
}

func openWebPasskeys(cmd *cobra.Command, endpoint passkeyauth.Endpoint) (*browserPasskeyHandler, string, func() error, error) {
	authFile := strings.TrimSpace(servePasskeyAuthFile)
	if authFile == "" {
		dir, err := appdata.GetDataDir()
		if err != nil {
			return nil, "", nil, err
		}
		authFile = filepath.Join(dir, "web-auth", "auth.json")
	}
	runtime, display, cleanup, err := openBrowserPasskeys(cmd, endpoint, authFile, browserPasskeyOptions{
		userName: "Web operator", displayName: "term-llm Web", trustedProxies: servePasskeyTrustedProxies,
		bootstrap: func(cmd *cobra.Command, needed bool) ([]byte, string, error) {
			return resolveBrowserBootstrapSecret(cmd, needed, servePasskeyBootstrapFile, "TERM_LLM_SERVE_BOOTSTRAP_TOKEN", servePrintPasskeyBootstrap, cmd.ErrOrStderr())
		},
		recovery: func(enabled bool) ([]byte, error) {
			return resolveBrowserRecoverySecret(enabled, servePasskeyRecoveryFile, "TERM_LLM_SERVE_RECOVERY_TOKEN")
		},
	})
	if err != nil {
		return nil, "", nil, err
	}
	return newWebPasskeyHandler(runtime), display, cleanup, nil
}

func newWebPasskeyHandler(runtime *hubPasskeyRuntime) *browserPasskeyHandler {
	base := runtime.endpoint.BasePath
	publicPath := func(p string) string { return base + p }
	return &browserPasskeyHandler{passkey: runtime, basePath: base, web: true, publicPath: publicPath,
		publicURLString: func(u *url.URL) string { return browserReturnURL(u, publicPath) },
		writeHubShell:   writeBrowserAuthShell,
		// OAuth callback authorization is its single-use SDK state, not a cookie.
		bypass: func(r *http.Request) bool {
			return r.Method == http.MethodGet && r.URL.Path == "/v1/mcp/oauth/callback"
		},
	}
}

func (s *browserPasskeyHandler) handleSecurityPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.writeHubShell(w, r, http.StatusOK, "Security - term-llm", hubPageConfig{Page: "security", AuthMode: "passkey", BasePath: s.basePath, PasskeyAuth: true, FormAction: s.passkey.endpoint.SafeReturnPath(r.URL.Query().Get("return"))})
}

func printWebPasskeyStatus(cmd *cobra.Command, auth *browserPasskeyHandler, display string) {
	out := cmd.ErrOrStderr()
	fmt.Fprintln(out, "auth: passkey")
	fmt.Fprintf(out, "Web UI: %s\n", auth.passkey.endpoint.URL.String())
	if auth.passkey.store.CredentialCount() == 0 {
		fmt.Fprintf(out, "\nFirst-passkey setup: %s\n", auth.passkey.endpoint.Origin+auth.publicPath("/auth/setup"))
		if display != "" {
			fmt.Fprintf(out, "Enter one-time setup code: %s\n", display)
		}
		fmt.Fprintln(out, "The setup code expires in 10 minutes and can create exactly one passkey.")
	}
}
