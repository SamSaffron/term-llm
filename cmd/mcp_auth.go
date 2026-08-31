package cmd

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	internalauth "github.com/samsaffron/term-llm/internal/oauth"
	"github.com/spf13/cobra"
)

var (
	mcpLoginForce     bool
	mcpLoginNoBrowser bool
	mcpLogoutLocal    bool
)

var mcpLoginCmd = &cobra.Command{
	Use:               "login <name>",
	Short:             "Sign in to a remote MCP server",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: MCPServerArgCompletion,
	RunE:              mcpLogin,
}

var mcpStatusCmd = &cobra.Command{
	Use:               "status [name]",
	Short:             "Show MCP transport and authentication status",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: MCPServerArgCompletion,
	RunE:              mcpStatus,
}

var mcpLogoutCmd = &cobra.Command{
	Use:               "logout <name>",
	Short:             "Revoke and remove an MCP OAuth grant",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: MCPServerArgCompletion,
	RunE:              mcpLogout,
}

func init() {
	mcpLoginCmd.Flags().BoolVar(&mcpLoginForce, "force", false, "Sign in again even when the current grant is valid")
	mcpLoginCmd.Flags().BoolVar(&mcpLoginNoBrowser, "no-browser", false, "Print the authorization URL without opening a browser")
	mcpLogoutCmd.Flags().BoolVar(&mcpLogoutLocal, "local-only", false, "Remove local credentials without remote revocation")
	mcpCmd.AddCommand(mcpLoginCmd, mcpStatusCmd, mcpLogoutCmd)
}

func loadMCPAuthManager() (*mcp.Manager, error) {
	manager := mcp.NewManager()
	if err := manager.LoadConfig(); err != nil {
		return nil, fmt.Errorf("load MCP config: %w", err)
	}
	return manager, nil
}

func mcpLogin(cmd *cobra.Command, args []string) error {
	name := args[0]
	manager, err := loadMCPAuthManager()
	if err != nil {
		return err
	}
	statuses := manager.AuthStatuses()
	status, ok := statuses[name]
	if !ok {
		return fmt.Errorf("unknown MCP server: %s", name)
	}
	if status.State == mcpoauth.AuthNotNeeded {
		return fmt.Errorf("MCP server %s does not use automatic OAuth", name)
	}
	if status.State == mcpoauth.AuthSignedIn && !mcpLoginForce {
		fmt.Fprintln(cmd.OutOrStdout(), "Already signed in")
		return nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start OAuth callback listener: %w", err)
	}
	defer listener.Close()
	redirectURL := "http://" + listener.Addr().String() + "/callback"
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		_, accepted := mcpoauth.DefaultCoordinator().CompleteCallback(
			r.URL.Query().Get("state"), r.URL.Query().Get("code"),
			r.URL.Query().Get("iss"), r.URL.Query().Get("error"),
		)
		if !accepted {
			http.Error(w, "This authorization callback is invalid, expired, or was already used.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = template.Must(template.New("done").Parse(`<!doctype html><meta charset="utf-8"><title>Connected</title><main><h1>Connected</h1><p>You can close this window.</p></main>`)).Execute(w, nil)
	})
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	startCtx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
	defer cancel()
	flow, err := manager.StartOAuth(startCtx, name, mcp.OAuthStartOptions{RedirectURL: redirectURL, Force: mcpLoginForce})
	if err != nil {
		return fmt.Errorf("start MCP sign-in: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Open this URL to authorize %s:\n%s\n", name, flow.AuthorizationURL)
	if !mcpLoginNoBrowser {
		if err := internalauth.OpenBrowser(flow.AuthorizationURL); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Could not open a browser: %v\n", err)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Waiting for authorization…")

	waitCtx, waitCancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer waitCancel()
	completed, err := mcpoauth.DefaultCoordinator().Wait(waitCtx, flow.ID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("authorization timed out")
		}
		return fmt.Errorf("wait for authorization: %w", err)
	}
	switch completed.State {
	case mcpoauth.FlowSucceeded:
		fmt.Fprintf(cmd.OutOrStdout(), "Signed in to %s\n", name)
		return nil
	case mcpoauth.FlowCanceled:
		return fmt.Errorf("authorization canceled")
	case mcpoauth.FlowExpired:
		return fmt.Errorf("authorization timed out")
	default:
		if completed.Error == "" {
			return fmt.Errorf("authorization failed")
		}
		return fmt.Errorf("authorization failed: %s", completed.Error)
	}
}

func mcpStatus(cmd *cobra.Command, args []string) error {
	manager, err := loadMCPAuthManager()
	if err != nil {
		return err
	}
	statuses := manager.AuthStatuses()
	names := manager.AvailableServers()
	if len(args) == 1 {
		if _, ok := statuses[args[0]]; !ok {
			return fmt.Errorf("unknown MCP server: %s", args[0])
		}
		names = []string{args[0]}
	}
	sort.Strings(names)
	for i, name := range names {
		cfg := manager.Config().Servers[name]
		status := statuses[name]
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout())
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n  transport: %s\n  authentication: %s\n", name, cfg.TransportType(), authStateLabel(status.State))
		if status.Issuer != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  issuer: %s\n", status.Issuer)
		}
		if len(status.Scopes) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  scopes: %s\n", strings.Join(status.Scopes, " "))
		}
		if !status.ExpiresAt.IsZero() {
			fmt.Fprintf(cmd.OutOrStdout(), "  expires: %s\n", status.ExpiresAt.Local().Format(time.RFC3339))
		}
		if status.StoragePath != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  storage: %s\n", status.StoragePath)
		}
	}
	return nil
}

func authStateLabel(state mcpoauth.AuthState) string {
	switch state {
	case mcpoauth.AuthNotNeeded:
		return "not needed"
	case mcpoauth.AuthSignedIn:
		return "signed in"
	case mcpoauth.AuthExpired:
		return "expired (refreshable)"
	case mcpoauth.AuthRequired:
		return "needs sign-in"
	case mcpoauth.AuthWaiting:
		return "waiting for browser"
	case mcpoauth.AuthRetry:
		return "temporary refresh failure (retry)"
	default:
		return "signed out"
	}
}

func mcpLogout(cmd *cobra.Command, args []string) error {
	manager, err := loadMCPAuthManager()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := manager.LogoutOAuth(ctx, args[0], mcpLogoutLocal); err != nil {
		return fmt.Errorf("sign out of MCP server: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Signed out of %s\n", args[0])
	return nil
}
