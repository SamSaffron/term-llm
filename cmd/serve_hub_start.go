package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/app/reload"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
)

func validateHubBind(host string, port int, requireAuth bool) error {
	return apphub.ValidateBind(host, port, requireAuth)
}

func normalizeHubBasePath(raw string) (string, error) { return apphub.NormalizeBasePath(raw) }

func defaultHubNodesFile() (string, error) { return apphub.DefaultNodesFile() }

func defaultHubAuthFile() (string, error) { return apphub.DefaultAuthFile() }

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

func runServeHubWith(parentCtx context.Context, deps hubDeps, opts *apphub.Options) error {
	authMode, err := resolveHubAuthMode(opts.AuthMode)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if err := validateHubBind(opts.Host, opts.Port, requireAuth); err != nil {
		return err
	}
	var endpoint passkeyauth.Endpoint
	if authMode == "passkey" {
		publicURL := strings.TrimSpace(opts.PublicURL)
		if publicURL == "" {
			publicURL = strings.TrimSpace(os.Getenv("TERM_LLM_HUB_PUBLIC_URL"))
		}
		endpoint, err = passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: publicURL, BasePath: opts.BasePath, BasePathExplicit: deps.Flags.BasePath})
		if err != nil {
			return fmt.Errorf("invalid Hub passkey --public-url/--base-path configuration: %w", err)
		}
	}
	hubBasePath, err := normalizeHubBasePath(opts.BasePath)
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
		if t := strings.TrimSpace(opts.Token); t != "" {
			token, tokenSource = t, tokenSourceFlag
		} else if hubTokenEnv != "" {
			token, tokenSource = hubTokenEnv, tokenSourceEnv
		}
	} else {
		token, tokenSource, err = resolveServeToken(opts.Token, hubTokenEnv, requireAuth, generateServeToken)
		if err != nil {
			return err
		}
	}

	var resolvers []hub.Resolver
	if strings.TrimSpace(opts.Config) != "" {
		resolvers = append(resolvers, hub.NewStaticResolver(opts.Config))
	}
	nodesFile := strings.TrimSpace(opts.NodesFile)
	if nodesFile == "" {
		var err error
		nodesFile, err = defaultHubNodesFile()
		if err != nil {
			return fmt.Errorf("resolve hub nodes file: %w", err)
		}
	}
	store := hub.NewStore(nodesFile)
	resolvers = append(resolvers, store)
	if opts.Contain {
		resolvers = append(resolvers, hub.NewContainResolver())
	}

	s := newHubServer(hub.NewRegistry(resolvers...), store)
	s.requireAuth = requireAuth
	s.authMode = authMode
	s.token = token
	s.registrationToken = resolveServeHubRegistrationToken(opts.RegistrationToken)
	s.basePath = hubBasePath
	// The delegation ledger and the dashboard node order live beside the
	// node store (same private dir).
	s.delegations = hub.NewDelegationStore(filepath.Join(filepath.Dir(nodesFile), "delegations.json"))
	s.nodeOrder = hub.NewNodeOrderStore(filepath.Join(filepath.Dir(nodesFile), "node-order.json"))
	attentionStore, err := hub.OpenAttentionProjectionStore(filepath.Join(filepath.Dir(nodesFile), "attention.db"))
	if err != nil {
		return fmt.Errorf("open Hub attention projection: %w", err)
	}
	defer attentionStore.Close()
	s.attentionStore = attentionStore
	s.startAttentionCollector()
	s.startHealthMonitor()
	defer func() {
		if s.healthCancel != nil {
			s.healthCancel()
			s.healthWG.Wait()
		}
		if s.attentionCancel != nil {
			s.attentionCancel()
			s.attentionWG.Wait()
		}
	}()
	bootstrapDisplay := ""
	if authMode == "passkey" {
		authFile := strings.TrimSpace(opts.PasskeyAuthFile)
		if authFile == "" {
			authFile, err = defaultHubAuthFile()
			if err != nil {
				return fmt.Errorf("resolve Hub passkey auth file: %w", err)
			}
		}
		var closePasskeys func() error
		s.passkey, bootstrapDisplay, closePasskeys, err = openBrowserPasskeys(deps.Stderr, endpoint, authFile, browserPasskeyOptions{
			userName: hubPasskeyUserName, trustedProxies: opts.PasskeyTrustedProxies,
			bootstrap: func(needed bool) ([]byte, string, error) { return resolveHubBootstrapSecret(deps, opts, needed) },
			recovery: func(enabled bool) ([]byte, error) {
				return resolveBrowserRecoverySecret(enabled, opts.RecoveryTokenFile, "TERM_LLM_HUB_RECOVERY_TOKEN")
			},
		})
		if err != nil {
			return err
		}
		defer func() {
			if err := closePasskeys(); err != nil {
				fmt.Fprintf(deps.Stderr, "passkey state close: %v\n", err)
			}
		}()
	}
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	transport := &restart.HTTPTransport{Coordinator: restart.Default}
	srv := &http.Server{Addr: addr, Handler: transport.Handler(s.handler()), ConnContext: transport.ConnContext, ConnState: transport.ConnState}

	out := deps.Stdout
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
	ctx, stop := signal.NotifyContextWithParent(parentCtx)
	defer stop()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- srv.Serve(listener) }()
	stopReload, err := reload.Bind(ctx, "TERM_LLM_HUB_TOKEN="+token)
	if err != nil {
		_ = srv.Close()
		<-finished
		return err
	}
	defer stopReload()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stopReload()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			_ = srv.Close()
		}
		<-finished
		return err
	}
}

func hubOutputIsTerminal(w any) bool { return apphub.OutputIsTerminal(w) }

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

func resolveHubAuthMode(raw string) (string, error) { return apphub.ResolveAuthMode(raw) }

func resolveHubBootstrapSecret(deps hubDeps, opts *apphub.Options, needed bool) ([]byte, string, error) {
	return resolveBrowserBootstrapSecret(deps.Stderr, needed, opts.BootstrapTokenFile, "TERM_LLM_HUB_BOOTSTRAP_TOKEN", opts.PrintBootstrapToken, deps.Stdout)
}

func resolveBrowserBootstrapSecret(stderr io.Writer, needed bool, file, envName string, printSecret bool, output io.Writer) ([]byte, string, error) {
	env := os.Getenv(envName)
	_ = os.Unsetenv(envName)
	if !needed {
		return nil, "", nil
	}
	if p := strings.TrimSpace(file); p != "" {
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
	interactive := hubOutputIsTerminal(output)
	if !interactive && !printSecret {
		return nil, "", fmt.Errorf("first-passkey setup requires --passkey-bootstrap-token-file or %s when output is non-interactive (or explicitly use --print-passkey-bootstrap-token)", envName)
	}
	secret, display, err := passkeyauth.GenerateBootstrapSecret(nil)
	if err != nil {
		return nil, "", err
	}
	if !interactive {
		fmt.Fprintln(stderr, "WARNING: printing a passkey bootstrap code to non-interactive output; service logs are temporary enrollment credentials")
	}
	return secret, display, nil
}

func resolveBrowserRecoverySecret(enabled bool, file, envName string) ([]byte, error) {
	env := os.Getenv(envName)
	_ = os.Unsetenv(envName)
	if !enabled {
		return nil, nil
	}
	if p := strings.TrimSpace(file); p != "" {
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
