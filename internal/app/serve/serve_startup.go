package serve

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/hubnode"
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

func shutdownServe(ctx context.Context, server *serveServer, wg *sync.WaitGroup, registration *hubnode.Registration) {
	<-ctx.Done()
	if attempted, err := registration.DeregisterAfterStop(3 * time.Second); err != nil {
		log.Printf("hub registration: deregister %s from %s: %v", registration.NodeID(), registration.HubURL(), err)
	} else if attempted {
		log.Printf("hub registration: deregistered %s from %s", registration.NodeID(), registration.HubURL())
	}
	if server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Stop(shutdownCtx)
	}
	wg.Wait()
}

func closeServeListener(listener net.Listener) func() {
	if listener == nil {
		return func() {}
	}
	return func() { _ = listener.Close() }
}

func resolveServeStartup(flags FlagState, opts *Options) (serveStartupResolution, error) {
	var out serveStartupResolution
	var err error
	out.startupDir, err = os.Getwd()
	if err != nil {
		return out, fmt.Errorf("resolve serve startup directory: %w", err)
	}
	if flags.Projects && flags.NoProjects {
		return out, fmt.Errorf("--projects and --no-projects cannot be used together")
	}
	if err := opts.Validate(flags.ResponseTimeout); err != nil {
		return out, err
	}
	out.sidebarSessions, err = parseSidebarSessionCategories(opts.SidebarSessions, true)
	if err != nil {
		return out, err
	}
	out.authMode, err = resolveServeAuthMode(flags.Auth, opts.AuthMode, flags.NoAuth, opts.AllowNoAuth)
	if err != nil {
		return out, err
	}
	if out.authMode == "passkey" {
		out.passkeyEndpoint, err = resolveWebPasskeyEndpoint(flags.BasePath, *opts)
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
