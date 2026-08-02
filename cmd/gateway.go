package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/search"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	gatewayStateDir               string
	gatewayListen                 string
	gatewayTLSCert                string
	gatewayTLSKey                 string
	gatewayAllowProviders         []string
	gatewayDenyProviders          []string
	gatewayAllowModels            []string
	gatewayDenyModels             []string
	gatewayAllowCLI               bool
	gatewayNoSearch               bool
	gatewayNoFetch                bool
	gatewayIdleTimeout            time.Duration
	gatewayToolTimeout            time.Duration
	gatewayCatalogTTL             time.Duration
	gatewayRetryAttempts          int
	gatewayRetryElapsed           time.Duration
	gatewayProviderSessionTimeout time.Duration
	gatewayClientAllowCLI         bool
	gatewayClientAllow            []string
	gatewayClientDeny             []string
	gatewayClientModels           []string
	gatewayClientDenyModel        []string
	gatewayClientSearch           bool
	gatewayClientFetch            bool
	gatewayClientInference        int
	gatewayClientSearchRPM        int
	gatewayClientSearchMax        int
	gatewayClientFetchRPM         int
	gatewayClientFetchMax         int
	gatewayClientEnroll           bool
	gatewayEnrollmentTTL          time.Duration
	gatewayEnrollName             string
	gatewayEnrollWrite            bool
	gatewayEnrollTokenFile        string
	gatewayEnrollPrintOnly        bool
	gatewayUsageClient            string
	gatewayUsageJSON              bool
)

var gatewayCmd = &cobra.Command{Use: "gateway", Short: "Serve and manage the private inference gateway"}

var gatewayServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the private gateway and OpenAI Responses edge",
	RunE:  runGatewayServe,
}

var gatewayClientCmd = &cobra.Command{Use: "client", Short: "Manage gateway satellite credentials"}
var gatewayClientAddCmd = &cobra.Command{Use: "add NAME", Args: cobra.ExactArgs(1), Short: "Create a satellite credential or enrollment token", RunE: runGatewayClientAdd}
var gatewayClientListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List gateway clients", RunE: runGatewayClientList}
var gatewayClientRevokeCmd = &cobra.Command{Use: "revoke ID_OR_NAME", Args: cobra.ExactArgs(1), Short: "Revoke a satellite credential", RunE: runGatewayClientRevoke}
var gatewayEnrollCmd = &cobra.Command{Use: "enroll URL ENROLLMENT_TOKEN", Args: cobra.ExactArgs(2), Short: "Consume a one-use enrollment token and configure this satellite", RunE: runGatewayEnroll}
var gatewayHealthCmd = &cobra.Command{Use: "health URL", Args: cobra.ExactArgs(1), Short: "Check an HTTP health endpoint", RunE: runGatewayHealth}
var gatewayUsageCmd = &cobra.Command{Use: "usage", Args: cobra.NoArgs, Short: "Inspect attributed gateway inference usage", RunE: runGatewayUsage}

func init() {
	rootCmd.AddCommand(gatewayCmd)
	gatewayCmd.AddCommand(gatewayServeCmd, gatewayClientCmd, gatewayEnrollCmd, gatewayHealthCmd, gatewayUsageCmd)
	gatewayClientCmd.AddCommand(gatewayClientAddCmd, gatewayClientListCmd, gatewayClientRevokeCmd)

	gatewayCmd.PersistentFlags().StringVar(&gatewayStateDir, "state-dir", "", "Gateway state directory (default: config directory/gateway)")
	gatewayServeCmd.Flags().StringVar(&gatewayListen, "listen", "127.0.0.1:8787", "Gateway listen address")
	gatewayServeCmd.Flags().StringVar(&gatewayTLSCert, "tls-cert", "", "TLS certificate PEM (requires --tls-key)")
	gatewayServeCmd.Flags().StringVar(&gatewayTLSKey, "tls-key", "", "TLS private key PEM (requires --tls-cert)")
	gatewayServeCmd.Flags().StringSliceVar(&gatewayAllowProviders, "allow-provider", nil, "Allowed provider key/prefix (repeatable)")
	gatewayServeCmd.Flags().StringSliceVar(&gatewayDenyProviders, "deny-provider", nil, "Denied provider key/prefix (repeatable)")
	gatewayServeCmd.Flags().StringSliceVar(&gatewayAllowModels, "allow-model", nil, "Allowed provider:model/model pattern (repeatable)")
	gatewayServeCmd.Flags().StringSliceVar(&gatewayDenyModels, "deny-model", nil, "Denied provider:model/model pattern (repeatable)")
	gatewayServeCmd.Flags().BoolVar(&gatewayAllowCLI, "allow-cli", false, "Allow CLI providers globally (clients must also opt in)")
	gatewayServeCmd.Flags().BoolVar(&gatewayNoSearch, "no-search", false, "Disable centralized gateway search")
	gatewayServeCmd.Flags().BoolVar(&gatewayNoFetch, "no-fetch", false, "Disable centralized gateway fetch")
	gatewayServeCmd.Flags().DurationVar(&gatewayIdleTimeout, "idle-timeout", 5*time.Minute, "Cancel provider streams with no events for this duration")
	gatewayServeCmd.Flags().DurationVar(&gatewayToolTimeout, "tool-timeout", 10*time.Minute, "Maximum wait for a satellite tool callback")
	gatewayServeCmd.Flags().DurationVar(&gatewayCatalogTTL, "catalog-ttl", 5*time.Minute, "Refresh provider config and live model catalogs after this interval")
	gatewayServeCmd.Flags().IntVar(&gatewayRetryAttempts, "upstream-retry-attempts", gateway.DefaultUpstreamRetryAttempts, "Maximum upstream attempts per gateway inference request")
	gatewayServeCmd.Flags().DurationVar(&gatewayRetryElapsed, "upstream-retry-elapsed", gateway.DefaultUpstreamRetryElapsed, "Maximum elapsed time across upstream attempts")
	gatewayServeCmd.Flags().DurationVar(&gatewayProviderSessionTimeout, "provider-session-idle-timeout", gateway.DefaultProviderSessionIdleTimeout, "Keep successful satellite WebSocket provider sessions warm for this idle duration (0 disables)")

	gatewayClientAddCmd.Flags().BoolVar(&gatewayClientAllowCLI, "allow-cli", false, "Allow this client to use CLI providers")
	gatewayClientAddCmd.Flags().StringSliceVar(&gatewayClientAllow, "allow-provider", nil, "Allowed provider key/prefix")
	gatewayClientAddCmd.Flags().StringSliceVar(&gatewayClientDeny, "deny-provider", nil, "Denied provider key/prefix")
	gatewayClientAddCmd.Flags().StringSliceVar(&gatewayClientModels, "allow-model", nil, "Allowed model/provider:model pattern")
	gatewayClientAddCmd.Flags().StringSliceVar(&gatewayClientDenyModel, "deny-model", nil, "Denied model/provider:model pattern")
	gatewayClientAddCmd.Flags().BoolVar(&gatewayClientSearch, "allow-search", false, "Allow centralized search for this client")
	gatewayClientAddCmd.Flags().BoolVar(&gatewayClientFetch, "allow-fetch", false, "Allow centralized fetch for this client")
	gatewayClientAddCmd.Flags().IntVar(&gatewayClientInference, "max-concurrent-inference", gateway.DefaultMaxConcurrentInference, "Maximum concurrent inference requests for this client")
	gatewayClientAddCmd.Flags().IntVar(&gatewayClientSearchRPM, "search-rate", gateway.DefaultSearchRatePerMinute, "Maximum search requests per minute")
	gatewayClientAddCmd.Flags().IntVar(&gatewayClientSearchMax, "max-concurrent-search", gateway.DefaultMaxConcurrentSearch, "Maximum concurrent search requests")
	gatewayClientAddCmd.Flags().IntVar(&gatewayClientFetchRPM, "fetch-rate", gateway.DefaultFetchRatePerMinute, "Maximum fetch requests per minute")
	gatewayClientAddCmd.Flags().IntVar(&gatewayClientFetchMax, "max-concurrent-fetch", gateway.DefaultMaxConcurrentFetch, "Maximum concurrent fetch requests")
	gatewayClientAddCmd.Flags().BoolVar(&gatewayClientEnroll, "enroll", false, "Generate a persisted one-use enrollment token instead of a client token")
	gatewayClientAddCmd.Flags().DurationVar(&gatewayEnrollmentTTL, "enroll-ttl", gateway.DefaultEnrollmentTTL, "Enrollment token lifetime (maximum 24h)")
	gatewayEnrollCmd.Flags().StringVar(&gatewayEnrollName, "name", "", "Satellite name (must match the enrollment token; default hostname)")
	gatewayEnrollCmd.Flags().BoolVar(&gatewayEnrollWrite, "write-config", true, "Atomically update satellite config and write a separate 0600 token file")
	gatewayEnrollCmd.Flags().StringVar(&gatewayEnrollTokenFile, "token-file", "", "Token file path (default: config directory/gateway-token)")
	gatewayEnrollCmd.Flags().BoolVar(&gatewayEnrollPrintOnly, "print-only", false, "Print config including the client token instead of writing files")
	gatewayUsageCmd.Flags().StringVar(&gatewayUsageClient, "client", "", "Filter by client ID or name")
	gatewayUsageCmd.Flags().BoolVar(&gatewayUsageJSON, "json", false, "Output usage records as JSON")
}

func runGatewayServe(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	if (strings.TrimSpace(gatewayTLSCert) == "") != (strings.TrimSpace(gatewayTLSKey) == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	if gatewayRetryAttempts <= 0 {
		return fmt.Errorf("--upstream-retry-attempts must be positive")
	}
	if gatewayRetryElapsed <= 0 {
		return fmt.Errorf("--upstream-retry-elapsed must be positive")
	}
	if gatewayProviderSessionTimeout < 0 {
		return fmt.Errorf("--provider-session-idle-timeout cannot be negative; use 0 to disable session reuse")
	}
	stateDir, err := resolveGatewayStateDir()
	if err != nil {
		return err
	}
	clients, err := gateway.OpenClientStore(filepath.Join(stateDir, "clients.json"))
	if err != nil {
		return err
	}
	sealer, err := gateway.OpenStateSealer(filepath.Join(stateDir, "state.key"))
	if err != nil {
		return err
	}
	central := *cfg
	central.Gateway = config.GatewayConfig{}
	var searcher search.Searcher
	if !gatewayNoSearch {
		searcher, err = search.NewSearcher(&central)
		if err != nil {
			return fmt.Errorf("configure gateway search: %w", err)
		}
	}
	fetchTool := newReadURLToolForConfig(&central)
	if gatewayNoFetch {
		fetchTool = nil
	}
	server, err := gateway.NewServer(gateway.ServerConfig{
		Config: &central,
		ConfigLoader: func() (*config.Config, error) {
			loaded, loadErr := config.Load()
			if loadErr != nil {
				return nil, loadErr
			}
			loaded.Gateway = config.GatewayConfig{}
			return loaded, nil
		},
		Clients: clients, Sealer: sealer,
		Usage:    &gateway.JSONLUsageRecorder{Path: filepath.Join(stateDir, "usage.jsonl")},
		Searcher: searcher, FetchTool: fetchTool,
		IdleTimeout: gatewayIdleTimeout, ToolTimeout: gatewayToolTimeout, CatalogTTL: gatewayCatalogTTL,
		UpstreamRetryAttempts: gatewayRetryAttempts, UpstreamRetryMaxElapsed: gatewayRetryElapsed,
		ProviderSessionIdleTimeout:  gatewayProviderSessionTimeout,
		DisableProviderSessionReuse: gatewayProviderSessionTimeout == 0,
		RunTempRoot:                 filepath.Join(stateDir, "runs"),
		Policy: gateway.Policy{
			AllowProviders: gatewayAllowProviders, DenyProviders: gatewayDenyProviders,
			AllowModels: gatewayAllowModels, DenyModels: gatewayDenyModels,
			AllowCLI: gatewayAllowCLI, AllowSearch: !gatewayNoSearch, AllowFetch: !gatewayNoFetch,
		},
	})
	if err != nil {
		return err
	}
	defer server.Close()
	httpServer := &http.Server{Addr: gatewayListen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-cmd.Context().Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	scheme := "http"
	if gatewayTLSCert != "" {
		scheme = "https"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Inference gateway listening on %s://%s (/g1 and /v1 Responses)\n", scheme, gatewayListen)
	fmt.Fprintln(cmd.OutOrStdout(), "Create one-use enrollment tokens with `term-llm gateway client add NAME --enroll --allow-provider PROVIDER`.")
	if gatewayTLSCert != "" {
		err = httpServer.ListenAndServeTLS(gatewayTLSCert, gatewayTLSKey)
	} else {
		err = httpServer.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func gatewayClientPolicy() gateway.Policy {
	return gateway.Policy{
		AllowProviders: gatewayClientAllow, DenyProviders: gatewayClientDeny,
		AllowModels: gatewayClientModels, DenyModels: gatewayClientDenyModel,
		AllowCLI: gatewayClientAllowCLI, AllowSearch: gatewayClientSearch, AllowFetch: gatewayClientFetch,
		MaxConcurrentInference: gatewayClientInference,
		SearchRatePerMinute:    gatewayClientSearchRPM, SearchBurst: min(gatewayClientSearchRPM, gateway.DefaultSearchBurst), MaxConcurrentSearch: gatewayClientSearchMax,
		FetchRatePerMinute: gatewayClientFetchRPM, FetchBurst: min(gatewayClientFetchRPM, gateway.DefaultFetchBurst), MaxConcurrentFetch: gatewayClientFetchMax,
	}
}

func runGatewayClientAdd(cmd *cobra.Command, args []string) error {
	store, err := gatewayClientStore()
	if err != nil {
		return err
	}
	policy := gatewayClientPolicy()
	if gatewayClientEnroll {
		enrollment, token, err := store.CreateEnrollment(args[0], policy, gatewayEnrollmentTTL)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token (shown once, expires %s): %s\n", enrollment.ExpiresAt.Format(time.RFC3339), token)
		fmt.Fprintf(cmd.OutOrStdout(), "Satellite: term-llm gateway enroll https://gateway.example %s --name %s\n", token, enrollment.Name)
		return nil
	}
	client, token, err := store.Add(args[0], policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Client: %s (%s)\nToken (shown once): %s\n", client.Name, client.ID, token)
	fmt.Fprintln(cmd.OutOrStdout(), "\nSatellite config:\ngateway:\n  url: https://gateway.example\n  token: "+token)
	return nil
}

func runGatewayClientList(cmd *cobra.Command, _ []string) error {
	store, err := gatewayClientStore()
	if err != nil {
		return err
	}
	for _, client := range store.List() {
		status := "active"
		if !client.RevokedAt.IsZero() {
			status = "revoked"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tcli=%t\tinference=%d\tsearch=%t\tfetch=%t\n", client.ID, client.Name, status, client.Policy.AllowCLI, client.Policy.InferenceConcurrency(), client.Policy.AllowSearch, client.Policy.AllowFetch)
	}
	return nil
}

func runGatewayClientRevoke(_ *cobra.Command, args []string) error {
	store, err := gatewayClientStore()
	if err != nil {
		return err
	}
	return store.Revoke(args[0])
}

func runGatewayEnroll(cmd *cobra.Command, args []string) error {
	if !gatewayEnrollPrintOnly && !gatewayEnrollWrite && strings.TrimSpace(gatewayEnrollTokenFile) == "" {
		return fmt.Errorf("select a credential destination with --write-config, --token-file, or --print-only")
	}
	name := strings.TrimSpace(gatewayEnrollName)
	if name == "" {
		name, _ = os.Hostname()
	}
	payload, _ := json.Marshal(protocol.EnrollmentRequest{Version: protocol.Version, Name: name})
	url := strings.TrimRight(args[0], "/") + "/g1/enroll"
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+args[1])
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("enroll with gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var wire protocol.Error
		_ = json.NewDecoder(resp.Body).Decode(&wire)
		if wire.Message != "" {
			return fmt.Errorf("gateway enrollment failed: %s", wire.Message)
		}
		return fmt.Errorf("gateway enrollment failed: HTTP %d", resp.StatusCode)
	}
	var enrolled protocol.EnrollmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrolled); err != nil {
		return err
	}
	if gatewayEnrollPrintOnly {
		fmt.Fprintf(cmd.OutOrStdout(), "gateway:\n  url: %s\n  token: %s\n", strings.TrimRight(args[0], "/"), enrolled.Token)
		return nil
	}
	tokenPath := strings.TrimSpace(gatewayEnrollTokenFile)
	if tokenPath == "" {
		configDir, pathErr := config.GetConfigDir()
		if pathErr != nil {
			return pathErr
		}
		tokenPath = filepath.Join(configDir, "gateway-token")
	}
	tokenPath, err = expandGatewayEnrollPath(tokenPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return fmt.Errorf("create gateway token directory: %w", err)
	}
	if err := config.WriteFileAtomically(tokenPath, []byte(enrolled.Token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write gateway token: %w", err)
	}
	if err := os.Chmod(tokenPath, 0o600); err != nil {
		return fmt.Errorf("secure gateway token: %w", err)
	}
	if !gatewayEnrollWrite {
		fmt.Fprintf(cmd.OutOrStdout(), "Gateway token written to %s (mode 0600).\n", tokenPath)
		return nil
	}
	configPath, err := config.GetConfigPath()
	if err != nil {
		return err
	}
	if err := writeGatewaySatelliteConfig(configPath, strings.TrimRight(args[0], "/"), tokenPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Gateway enrollment complete.\nConfig: %s\nToken: %s (mode 0600)\n", configPath, tokenPath)
	return nil
}

func expandGatewayEnrollPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve token file: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve token file: %w", err)
	}
	return filepath.Clean(abs), nil
}

func writeGatewaySatelliteConfig(path, gatewayURL, tokenPath string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	root := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse existing config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing config: %w", err)
	}
	gatewayMap := make(map[string]any)
	if existing, ok := root["gateway"].(map[string]any); ok {
		for key, value := range existing {
			gatewayMap[key] = value
		}
	}
	gatewayMap["url"] = gatewayURL
	gatewayMap["token_file"] = tokenPath
	delete(gatewayMap, "token")
	delete(gatewayMap, "token_env")
	root["gateway"] = gatewayMap
	data, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("encode satellite config: %w", err)
	}
	if err := config.WriteFileAtomically(path, data, 0o600); err != nil {
		return fmt.Errorf("write satellite config: %w", err)
	}
	return nil
}

func runGatewayHealth(cmd *cobra.Command, args []string) error {
	url := strings.TrimSpace(args[0])
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("health request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned HTTP %d", resp.StatusCode)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok")
	return nil
}

func runGatewayUsage(cmd *cobra.Command, _ []string) error {
	stateDir, err := resolveGatewayStateDir()
	if err != nil {
		return err
	}
	records, err := gateway.ReadUsageRecords(filepath.Join(stateDir, "usage.jsonl"))
	if err != nil {
		return err
	}
	filtered := records[:0]
	for _, record := range records {
		if gatewayUsageClient == "" || record.ClientID == gatewayUsageClient || record.ClientName == gatewayUsageClient {
			filtered = append(filtered, record)
		}
	}
	if gatewayUsageJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(filtered)
	}
	var input, output int
	var cost float64
	for _, record := range filtered {
		input += record.InputTokens + record.CachedInputTokens + record.CacheWriteTokens
		output += record.OutputTokens
		if record.CostUSD != nil {
			cost += *record.CostUSD
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s:%s\tin=%d\tout=%d\terror=%s\n", record.CompletedAt.UTC().Format(time.RFC3339), record.ClientName, record.RequestID, record.ProviderKey, record.Model, record.InputTokens, record.OutputTokens, record.ErrorCode)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Total\trequests=%d\tinput=%d\toutput=%d\tcost=$%.4f\n", len(filtered), input, output, cost)
	return nil
}

func gatewayClientStore() (*gateway.ClientStore, error) {
	stateDir, err := resolveGatewayStateDir()
	if err != nil {
		return nil, err
	}
	return gateway.OpenClientStore(filepath.Join(stateDir, "clients.json"))
}

func resolveGatewayStateDir() (string, error) {
	if strings.TrimSpace(gatewayStateDir) != "" {
		return filepath.Clean(gatewayStateDir), nil
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "gateway"), nil
}
