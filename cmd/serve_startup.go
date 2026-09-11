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

func parseServeToolMap(entries []string) (map[string]string, error) {
	var toolMap map[string]string
	for _, entry := range entries {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid --tool-map %q (expected ClientName:ServerName)", entry)
		}
		if toolMap == nil {
			toolMap = make(map[string]string)
		}
		toolMap[parts[0]] = parts[1]
	}
	return toolMap, nil
}

func resolveServeStartup(cmd *cobra.Command) (serveStartupResolution, error) {
	var out serveStartupResolution
	var err error
	out.startupDir, err = os.Getwd()
	if err != nil {
		return out, fmt.Errorf("resolve serve startup directory: %w", err)
	}
	if cmd.Flags().Changed("projects") && cmd.Flags().Changed("no-projects") {
		return out, fmt.Errorf("--projects and --no-projects cannot be used together")
	}
	if servePort <= 0 || servePort > 65535 {
		return out, fmt.Errorf("invalid --port %d (must be 1-65535)", servePort)
	}
	if serveSessionTTL <= 0 {
		return out, fmt.Errorf("invalid --session-ttl %s (must be > 0)", serveSessionTTL)
	}
	if serveSessionMax <= 0 {
		return out, fmt.Errorf("invalid --session-max %d (must be > 0)", serveSessionMax)
	}
	if serveTelegramCarryoverChars < 0 {
		return out, fmt.Errorf("invalid --telegram-carryover-chars %d (must be >= 0)", serveTelegramCarryoverChars)
	}
	if serveJobsWorkers <= 0 {
		return out, fmt.Errorf("invalid --jobs-workers %d (must be > 0)", serveJobsWorkers)
	}
	if cmd.Flags().Changed("response-timeout") && serveResponseTimeout <= 0 {
		return out, fmt.Errorf("invalid --response-timeout %s (must be > 0)", serveResponseTimeout)
	}
	out.sidebarSessions, err = parseSidebarSessionCategories(serveSidebarSessions, true)
	if err != nil {
		return out, err
	}
	out.authMode, err = resolveServeAuthMode(cmd.Flags().Changed("auth"), serveAuthMode, cmd.Flags().Changed("no-auth") || cmd.Flags().Changed("allow-no-auth"), serveAllowNoAuth)
	if err != nil {
		return out, err
	}
	if out.authMode == "passkey" {
		out.passkeyEndpoint, err = resolveWebPasskeyEndpoint(cmd)
		if err != nil {
			return out, err
		}
		serveBasePath, servePublicURL = out.passkeyEndpoint.BasePath, out.passkeyEndpoint.URL.String()
	}
	out.requireAuth = out.authMode != "none"
	if !out.requireAuth && !isLoopbackHost(serveHost) {
		return out, fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", serveHost)
	}
	out.token, out.tokenSource, err = resolveServeToken(serveToken, os.Getenv("TERM_LLM_SERVE_TOKEN"), out.authMode == "bearer", restoreOrGenerateServeToken)
	if err != nil {
		return out, err
	}
	serveHubConnect = strings.ToLower(strings.TrimSpace(serveHubConnect))
	if serveHubConnect == "" {
		serveHubConnect = "direct"
	}
	if serveHubConnect != "direct" && serveHubConnect != "reverse" {
		return out, fmt.Errorf("invalid --hub-connect %q (use direct or reverse)", serveHubConnect)
	}
	if serveHubRegister {
		out.hubRegistrationToken = resolveServeHubRegistrationToken(serveHubRegistrationToken)
	}
	if hubURL, hubNodeID := strings.TrimSpace(serveHubURL), strings.TrimSpace(serveHubNodeID); hubURL != "" && hubNodeID != "" && out.token != "" {
		tools.ConfigureHubDelegation(hubURL, hubNodeID, out.token)
	}
	return out, nil
}
