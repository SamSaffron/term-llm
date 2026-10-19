package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/tools"
)

type serveStartupResolution struct {
	startupDir           string
	sidebarSessions      []string
	authMode             string
	passkeyEndpoint      passkeyauth.Endpoint
	requireAuth          bool
	token                string
	tokenSource          string
	hubRegistrationToken string
}

func shutdownServe(ctx context.Context, server *serveServer, wg *sync.WaitGroup, hubURL, hubNodeID, registrationToken string) {
	<-ctx.Done()
	if hubURL != "" && hubNodeID != "" && registrationToken != "" {
		deregisterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := unregisterServeHubNode(deregisterCtx, nil, hubURL, registrationToken, hubNodeID); err != nil {
			log.Printf("hub registration: deregister %s from %s: %v", hubNodeID, hubURL, err)
		} else {
			log.Printf("hub registration: deregistered %s from %s", hubNodeID, hubURL)
		}
		cancel()
	}
	if server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Stop(shutdownCtx)
	}
	wg.Wait()
}

func resolveServeStartup(cmd *cobra.Command, opts *ServeOptions) (serveStartupResolution, error) {
	var out serveStartupResolution
	var err error
	out.startupDir, err = os.Getwd()
	if err != nil {
		return out, fmt.Errorf("resolve serve startup directory: %w", err)
	}
	if cmd.Flags().Changed("projects") && cmd.Flags().Changed("no-projects") {
		return out, fmt.Errorf("--projects and --no-projects cannot be used together")
	}
	if err := opts.Validate(cmd.Flags().Changed("response-timeout")); err != nil {
		return out, err
	}
	out.sidebarSessions, err = parseSidebarSessionCategories(opts.SidebarSessions, true)
	if err != nil {
		return out, err
	}
	out.authMode, err = resolveServeAuthMode(cmd.Flags().Changed("auth"), opts.AuthMode, cmd.Flags().Changed("no-auth") || cmd.Flags().Changed("allow-no-auth"), opts.AllowNoAuth)
	if err != nil {
		return out, err
	}
	if out.authMode == "passkey" {
		out.passkeyEndpoint, err = resolveWebPasskeyEndpoint(cmd, *opts)
		if err != nil {
			return out, err
		}
		opts.BasePath, opts.PublicURL = out.passkeyEndpoint.BasePath, out.passkeyEndpoint.URL.String()
	}
	out.requireAuth = out.authMode != "none"
	if !out.requireAuth && !isLoopbackHost(opts.Host) {
		return out, fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", opts.Host)
	}
	out.token, out.tokenSource, err = resolveServeToken(opts.Token, os.Getenv("TERM_LLM_SERVE_TOKEN"), out.authMode == "bearer", restoreOrGenerateServeToken)
	if err != nil {
		return out, err
	}
	opts.HubConnect = strings.ToLower(strings.TrimSpace(opts.HubConnect))
	if opts.HubConnect == "" {
		opts.HubConnect = "direct"
	}
	if opts.HubConnect != "direct" && opts.HubConnect != "reverse" {
		return out, fmt.Errorf("invalid --hub-connect %q (use direct or reverse)", opts.HubConnect)
	}
	if opts.HubRegister {
		out.hubRegistrationToken = resolveServeHubRegistrationToken(opts.HubRegistrationToken)
	}
	if hubURL, hubNodeID := strings.TrimSpace(opts.HubURL), strings.TrimSpace(opts.HubNodeID); hubURL != "" && hubNodeID != "" && out.token != "" {
		tools.ConfigureHubDelegation(hubURL, hubNodeID, out.token)
	}
	return out, nil
}
