package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

var (
	providersJSON       bool
	providersConfigured bool
	providersBuiltin    bool
)

// ProviderInfo describes a provider for external consumption
type ProviderInfo struct {
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	Credential         string   `json:"credential"`        // "api_key", "oauth", "aws", "none"
	EnvVar             string   `json:"env_var,omitempty"` // Environment variable for API key
	RequiresKey        bool     `json:"requires_key"`      // Whether API key is required
	SupportsListModels bool     `json:"supports_list_models"`
	Models             []string `json:"models,omitempty"`         // Curated model list
	Configured         bool     `json:"configured"`               // Enabled under provider_discovery (config, default, env var, or local login)
	ConfiguredVia      string   `json:"configured_via,omitempty"` // "config", "default", "env", or "login"
	Disabled           bool     `json:"disabled,omitempty"`       // providers.<name>.enabled: false; never configured
	IsBuiltin          bool     `json:"is_builtin"`               // Whether this is a built-in provider
}

var providersCmd = &cobra.Command{
	Use:   "providers [name]",
	Short: "List available LLM providers",
	Long: `List available LLM providers and their configuration details.

This command shows built-in providers, their credential requirements,
and available models. Useful for scripting and third-party integrations.

Examples:
  term-llm providers                    # list all providers
  term-llm providers --json             # JSON output for scripting
  term-llm providers --builtin          # only built-in providers
  term-llm providers --configured       # only configured providers
  term-llm providers anthropic          # details for specific provider`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProviders,
}

func init() {
	rootCmd.AddCommand(providersCmd)
	providersCmd.Flags().BoolVar(&providersJSON, "json", false, "Output as JSON")
	providersCmd.Flags().BoolVar(&providersConfigured, "configured", false, "Show only configured providers")
	providersCmd.Flags().BoolVar(&providersBuiltin, "builtin", false, "Show only built-in providers")
}

func runProviders(cmd *cobra.Command, args []string) error {
	// A missing config file loads defaults; anything else (such as an invalid
	// provider_discovery) is reported rather than silently treated as auto.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Build provider info list
	providers := buildProviderList(cfg)

	// If specific provider requested, show details
	if len(args) == 1 {
		return showProviderDetails(args[0], providers)
	}

	// Apply filters
	var filtered []ProviderInfo
	for _, p := range providers {
		if providersConfigured && !p.Configured {
			continue
		}
		if providersBuiltin && !p.IsBuiltin {
			continue
		}
		filtered = append(filtered, p)
	}

	if providersJSON {
		return outputProvidersJSON(filtered)
	}

	return outputProvidersText(filtered, cfg)
}

func buildProviderList(cfg *config.Config) []ProviderInfo {
	return buildProviderListWith(cfg, llm.ProviderHasLocalCredentials)
}

// buildProviderListWith builds the provider list, using hasLocalCredentials to
// detect local login state for built-ins (so callers can cache the probes).
// Whether a built-in counts as configured follows provider_discovery; see
// llm.ProviderConfiguredVia.
func buildProviderListWith(cfg *config.Config, hasLocalCredentials func(string) bool) []ProviderInfo {
	var providers []ProviderInfo
	for _, spec := range config.BuiltinProviders() {
		info := newProviderInfo(cfg, spec.Name, spec, hasLocalCredentials)
		info.IsBuiltin = true
		info.Models = llm.ProviderModelIDs(spec.Name)
		providers = append(providers, info)
	}
	if cfg != nil {
		for name, provCfg := range cfg.Providers {
			if _, builtin := config.BuiltinProvider(name); builtin {
				continue
			}
			// A custom provider describes itself like the built-in type it
			// names; anything else is a generic OpenAI-compatible endpoint.
			providerType := config.InferProviderType(name, provCfg.Type)
			spec, ok := config.BuiltinProvider(string(providerType))
			if !ok {
				spec = config.ProviderSpec{Type: providerType, Credential: config.CredentialAPIKey, ListModels: true}
			}
			info := newProviderInfo(cfg, name, spec, hasLocalCredentials)
			info.EnvVar = ""
			if len(provCfg.Models) > 0 {
				info.Models = provCfg.Models
			} else if provCfg.Model != "" {
				info.Models = []string{provCfg.Model}
			}
			providers = append(providers, info)
		}
	}
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})
	return providers
}

// newProviderInfo describes provider name, which behaves like spec.
func newProviderInfo(cfg *config.Config, name string, spec config.ProviderSpec, hasLocalCredentials func(string) bool) ProviderInfo {
	info := ProviderInfo{
		Name:               name,
		Type:               string(spec.Type),
		Credential:         string(spec.Credential),
		EnvVar:             spec.APIKeyEnv,
		RequiresKey:        spec.RequiresAPIKey(),
		SupportsListModels: spec.ListModels,
		Disabled:           cfg.ProviderDisabled(name),
		ConfiguredVia:      llm.ProviderConfiguredVia(cfg, name, hasLocalCredentials),
	}
	info.Configured = info.ConfiguredVia != ""
	return info
}

func showProviderDetails(name string, providers []ProviderInfo) error {
	var provider *ProviderInfo
	for i := range providers {
		if providers[i].Name == name {
			provider = &providers[i]
			break
		}
	}

	if provider == nil {
		return fmt.Errorf("provider '%s' not found. Use 'term-llm providers' to list available providers", name)
	}

	if providersJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(provider)
	}

	// Text output
	fmt.Printf("Provider: %s\n", provider.Name)
	fmt.Printf("  Type:           %s\n", provider.Type)

	if spec, ok := config.BuiltinProvider(provider.Name); ok {
		fmt.Printf("  Description:    %s\n", spec.Description)
	}

	fmt.Printf("  Credential:     %s\n", provider.Credential)
	if provider.EnvVar != "" {
		fmt.Printf("  Env variable:   %s\n", provider.EnvVar)
	}
	if provider.RequiresKey {
		fmt.Printf("  Requires key:   yes\n")
	} else {
		fmt.Printf("  Requires key:   no\n")
	}
	if provider.SupportsListModels {
		fmt.Printf("  List models:    yes (use 'term-llm models --provider %s')\n", provider.Name)
	} else {
		fmt.Printf("  List models:    no\n")
	}
	if provider.Disabled {
		fmt.Printf("  Configured:     no (disabled: providers.%s.enabled: false)\n", provider.Name)
	} else if provider.Configured {
		fmt.Printf("  Configured:     yes (%s)\n", provider.ConfiguredVia)
	} else {
		fmt.Printf("  Configured:     no\n")
	}

	if len(provider.Models) > 0 {
		fmt.Printf("\n  Available models:\n")
		for _, m := range provider.Models {
			fmt.Printf("    %s\n", m)
		}
	}

	return nil
}

func outputProvidersJSON(providers []ProviderInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(providers)
}

func outputProvidersText(providers []ProviderInfo, cfg *config.Config) error {
	defaultProvider := ""
	if cfg != nil {
		defaultProvider = cfg.DefaultProvider
	}
	mode := cfg.ProviderDiscoveryMode()
	var defaultErr error
	if defaultProvider != "" {
		defaultErr = llm.ProviderUnavailableError(cfg, defaultProvider)
	}
	writeProviderDiscoveryNotice(os.Stdout, mode, defaultErr)
	writeProvidersText(os.Stdout, providers, defaultProvider, mode)
	return nil
}

// writeProviderDiscoveryNotice explains a non-auto provider_discovery mode and
// warns when default_provider cannot be used under it.
func writeProviderDiscoveryNotice(w io.Writer, mode string, defaultErr error) {
	switch mode {
	case config.ProviderDiscoveryConfig:
		fmt.Fprintln(w, "Provider discovery: config (only providers named under providers: in config.yaml are enabled)")
	case config.ProviderDiscoveryEnv:
		fmt.Fprintln(w, "Provider discovery: env (providers in config.yaml or enabled by environment variables; local logins are not detected)")
	default:
		return
	}
	if defaultErr != nil {
		fmt.Fprintf(w, "Warning: default_provider: %v\n", defaultErr)
	}
	fmt.Fprintln(w)
}

// providerNextStep returns the step that would set up an unconfigured
// provider under provider_discovery mode.
func providerNextStep(name, mode string) string {
	spec, ok := config.BuiltinProvider(name)
	if !ok || mode != config.ProviderDiscoveryAuto {
		return config.ProviderEnableHint(mode, name)
	}
	return spec.SetupHint()
}

// writeProvidersText renders the provider list as two sections: providers
// that can be selected (with where their setup came from) and built-ins that
// are not set up yet (with the step that would set them up). Input order is
// preserved; buildProviderList sorts by name.
func writeProvidersText(w io.Writer, providers []ProviderInfo, defaultProvider, mode string) {
	var configured, available, disabled []ProviderInfo
	nameWidth := len("PROVIDER")
	for _, p := range providers {
		switch {
		case p.Disabled:
			disabled = append(disabled, p)
		case p.Configured:
			configured = append(configured, p)
		default:
			available = append(available, p)
		}
		nameWidth = max(nameWidth, len(p.Name))
	}
	if len(providers) == 0 {
		if providersConfigured {
			fmt.Fprintln(w, "No configured providers found.")
		} else {
			fmt.Fprintln(w, "No providers found.")
		}
		return
	}

	row := func(marker, name string, cols ...string) {
		line := marker + name + strings.Repeat(" ", nameWidth-len(name))
		for _, col := range cols {
			line += "  " + col
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}

	if len(configured) > 0 {
		fmt.Fprintln(w, "Configured:")
		row("  ", "PROVIDER", fmt.Sprintf("%-8s", "KIND"), "SOURCE")
		for _, p := range configured {
			marker := "  "
			if p.Name == defaultProvider {
				marker = "* "
			}
			kind := "custom"
			if p.IsBuiltin {
				kind = "built-in"
			}
			row(marker, p.Name, fmt.Sprintf("%-8s", kind), providerSourceLabel(p))
		}
	}
	if len(available) > 0 {
		if len(configured) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Available (not set up):")
		row("  ", "PROVIDER", "NEXT STEP")
		for _, p := range available {
			row("  ", p.Name, providerNextStep(p.Name, mode))
		}
	}
	if len(disabled) > 0 {
		if len(configured)+len(available) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Disabled:")
		row("  ", "PROVIDER", "TO ENABLE")
		for _, p := range disabled {
			row("  ", p.Name, "remove providers."+p.Name+".enabled: false")
		}
	}

	fmt.Fprintln(w)
	if defaultProvider != "" {
		fmt.Fprintln(w, "* default provider; configured does not verify connectivity or credentials.")
	} else {
		fmt.Fprintln(w, "Configured does not verify connectivity or credentials.")
	}
	fmt.Fprintln(w, "Details: term-llm providers <name>")
	fmt.Fprintln(w, "Models:  term-llm models --provider <name>")
}

// providerSourceLabel describes, in user terms, why a provider is configured.
func providerSourceLabel(p ProviderInfo) string {
	switch p.ConfiguredVia {
	case llm.ConfiguredViaConfig:
		return "config.yaml"
	case llm.ConfiguredViaDefault:
		return "default"
	case llm.ConfiguredViaEnv:
		spec, _ := config.BuiltinProvider(p.Name)
		return "$" + spec.EnabledByEnv()
	case llm.ConfiguredViaLogin:
		// A provider without credentials of its own (claude-bin) is detected
		// by its installed CLI, not by a sign-in.
		if p.Credential == string(config.CredentialNone) {
			return "CLI installed"
		}
		return "signed in"
	}
	return p.ConfiguredVia
}
