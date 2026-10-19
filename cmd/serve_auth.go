package cmd

import (
	"fmt"
	"strings"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
)

func resolveServeAuthMode(authFlagSet bool, authMode string, allowNoAuthSet bool, allowNoAuth bool) (string, error) {
	return appserve.ResolveAuthMode(authFlagSet, authMode, allowNoAuthSet, allowNoAuth)
}

func isLoopbackHost(host string) bool { return appserver.IsLoopbackHost(host) }

// plainHTTPConnectionWarning explains the browser connection limit when
// browsers are likely to reach the web UI directly over plain HTTP. Browsers
// cap HTTP/1.1 at six connections per origin across every tab, and each
// visible tab keeps up to two streams open. Loopback binds are normally fronted
// by a local proxy, and an https public URL means a proxy terminates TLS.
func plainHTTPConnectionWarning(ui bool, host, publicURL string) string {
	if !ui || isLoopbackHost(host) || strings.HasPrefix(strings.ToLower(publicURL), "https://") {
		return ""
	}
	return "warning: browsers share 6 HTTP/1.1 connections per origin across all tabs; " +
		"serve the UI through an HTTPS (HTTP/2) proxy to use several visible windows at once"
}

// Sources reported by resolveServeToken for use in the startup banner.
const (
	tokenSourceNone      = ""
	tokenSourceFlag      = "flag"
	tokenSourceEnv       = "env"
	tokenSourceGenerated = "generated"
)

// resolveServeToken returns the bearer token to use for the serve command.
// Precedence: --token flag > TERM_LLM_SERVE_TOKEN env > auto-generated.
// When requireAuth is false, returns an empty token and tokenSourceNone.
func resolveServeToken(flagValue, envValue string, requireAuth bool, generate func() (string, error)) (string, string, error) {
	if !requireAuth {
		return "", tokenSourceNone, nil
	}
	if t := strings.TrimSpace(flagValue); t != "" {
		return t, tokenSourceFlag, nil
	}
	if t := strings.TrimSpace(envValue); t != "" {
		return t, tokenSourceEnv, nil
	}
	t, err := generate()
	if err != nil {
		return "", tokenSourceNone, fmt.Errorf("generate auth token: %w", err)
	}
	return t, tokenSourceGenerated, nil
}

func generateServeToken() (string, error) { return appserve.GenerateToken() }
