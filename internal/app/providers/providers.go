// Package providers implements the providers command.
package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// Info describes a provider for external consumption
type Info struct {
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

// Options are the flag-derived settings for the providers command.
type Options struct {
	JSON       bool
	Configured bool
	Builtin    bool
	Stdout     io.Writer // defaults to os.Stdout
}

func (o Options) stdout() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return os.Stdout
}

// Run lists providers, or details for one provider when args names it.
func Run(opts Options, args []string) error {
	// A missing config file loads defaults; anything else (such as an invalid
	// provider_discovery) is reported rather than silently treated as auto.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Build provider info list
	providers := BuildList(cfg)

	// If specific provider requested, show details
	if len(args) == 1 {
		return showProviderDetails(opts, args[0], providers)
	}

	// Apply filters
	var filtered []Info
	for _, p := range providers {
		if opts.Configured && !p.Configured {
			continue
		}
		if opts.Builtin && !p.IsBuiltin {
			continue
		}
		filtered = append(filtered, p)
	}

	if opts.JSON {
		return outputProvidersJSON(opts, filtered)
	}

	return outputProvidersText(opts, filtered, cfg)
}

// BuildList builds the provider list using real local-credential probes.
func BuildList(cfg *config.Config) []Info {
	return BuildListWith(cfg, llm.ProviderHasLocalCredentials)
}

// BuildListWith builds the provider list, using hasLocalCredentials to
// detect local login state for built-ins (so callers can cache the probes).
// Whether a built-in counts as configured follows provider_discovery; see
// config.Config.ProviderConfiguredVia.
func BuildListWith(cfg *config.Config, hasLocalCredentials func(string) bool) []Info {
	var providers []Info
	for _, spec := range config.BuiltinProviders() {
		info := newInfo(cfg, spec.Name, spec, hasLocalCredentials)
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
			info := newInfo(cfg, name, spec, hasLocalCredentials)
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

// newInfo describes provider name, which behaves like spec.
func newInfo(cfg *config.Config, name string, spec config.ProviderSpec, hasLocalCredentials func(string) bool) Info {
	info := Info{
		Name:               name,
		Type:               string(spec.Type),
		Credential:         string(spec.Credential),
		EnvVar:             spec.APIKeyEnv,
		RequiresKey:        spec.RequiresAPIKey(),
		SupportsListModels: spec.ListModels,
		Disabled:           cfg.ProviderDisabled(name),
		ConfiguredVia:      cfg.ProviderConfiguredVia(name, hasLocalCredentials),
	}
	info.Configured = info.ConfiguredVia != ""
	return info
}

func showProviderDetails(opts Options, name string, providers []Info) error {
	var provider *Info
	for i := range providers {
		if providers[i].Name == name {
			provider = &providers[i]
			break
		}
	}

	if provider == nil {
		return fmt.Errorf("provider '%s' not found. Use 'term-llm providers' to list available providers", name)
	}

	if opts.JSON {
		enc := json.NewEncoder(opts.stdout())
		enc.SetIndent("", "  ")
		return enc.Encode(provider)
	}

	// Text output
	w := opts.stdout()
	fmt.Fprintf(w, "Provider: %s\n", provider.Name)
	fmt.Fprintf(w, "  Type:           %s\n", provider.Type)

	if spec, ok := config.BuiltinProvider(provider.Name); ok {
		fmt.Fprintf(w, "  Description:    %s\n", spec.Description)
	}

	fmt.Fprintf(w, "  Credential:     %s\n", provider.Credential)
	if provider.EnvVar != "" {
		fmt.Fprintf(w, "  Env variable:   %s\n", provider.EnvVar)
	}
	if provider.RequiresKey {
		fmt.Fprintf(w, "  Requires key:   yes\n")
	} else {
		fmt.Fprintf(w, "  Requires key:   no\n")
	}
	if provider.SupportsListModels {
		fmt.Fprintf(w, "  List models:    yes (use 'term-llm models --provider %s')\n", provider.Name)
	} else {
		fmt.Fprintf(w, "  List models:    no\n")
	}
	if provider.Disabled {
		fmt.Fprintf(w, "  Configured:     no (disabled: providers.%s.enabled: false)\n", provider.Name)
	} else if provider.Configured {
		fmt.Fprintf(w, "  Configured:     yes (%s)\n", provider.ConfiguredVia)
	} else {
		fmt.Fprintf(w, "  Configured:     no\n")
	}

	if len(provider.Models) > 0 {
		fmt.Fprintf(w, "\n  Available models:\n")
		for _, m := range provider.Models {
			fmt.Fprintf(w, "    %s\n", m)
		}
	}

	return nil
}

func outputProvidersJSON(opts Options, providers []Info) error {
	enc := json.NewEncoder(opts.stdout())
	enc.SetIndent("", "  ")
	return enc.Encode(providers)
}

func outputProvidersText(opts Options, providers []Info, cfg *config.Config) error {
	defaultProvider := ""
	if cfg != nil {
		defaultProvider = cfg.DefaultProvider
	}
	mode := cfg.ProviderDiscoveryMode()
	var defaultErr error
	if defaultProvider != "" {
		defaultErr = llm.ProviderUnavailableError(cfg, defaultProvider)
	}
	writeProviderDiscoveryNotice(opts.stdout(), mode, defaultErr)
	writeProvidersText(opts.stdout(), providers, defaultProvider, mode, opts.Configured)
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
// preserved; BuildList sorts by name.
func writeProvidersText(w io.Writer, providers []Info, defaultProvider, mode string, configuredOnly bool) {
	var configured, available, disabled []Info
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
		if configuredOnly {
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
func providerSourceLabel(p Info) string {
	switch p.ConfiguredVia {
	case config.ConfiguredViaConfig:
		return "config.yaml"
	case config.ConfiguredViaDefault:
		return "default"
	case config.ConfiguredViaEnv:
		spec, _ := config.BuiltinProvider(p.Name)
		return "$" + spec.EnabledByEnv()
	case config.ConfiguredViaLogin:
		// A provider without credentials of its own (claude-bin) is detected
		// by its installed CLI, not by a sign-in.
		if p.Credential == string(config.CredentialNone) {
			return "CLI installed"
		}
		return "signed in"
	}
	return p.ConfiguredVia
}
