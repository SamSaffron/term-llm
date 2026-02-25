package cmd

import (
	"encoding/json"
	"fmt"
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
	Credential         string   `json:"credential"`        // "api_key", "oauth", "none"
	EnvVar             string   `json:"env_var,omitempty"` // Environment variable for API key
	RequiresKey        bool     `json:"requires_key"`      // Whether API key is required
	SupportsListModels bool     `json:"supports_list_models"`
	Models             []string `json:"models,omitempty"` // Curated model list
	Configured         bool     `json:"configured"`       // Whether provider is in user config
	IsBuiltin          bool     `json:"is_builtin"`       // Whether this is a built-in provider
}

// builtinProviderMeta contains metadata about built-in providers
var builtinProviderMeta = map[string]struct {
	credential         string
	envVar             string
	requiresKey        bool
	supportsListModels bool
	description        string
}{
	"anthropic": {
		credential:         "api_key",
		envVar:             "ANTHROPIC_API_KEY",
		requiresKey:        true,
		supportsListModels: true,
		description:        "Anthropic API (Claude models)",
	},
	"openai": {
		credential:         "api_key",
		envVar:             "OPENAI_API_KEY",
		requiresKey:        true,
		supportsListModels: true,
		description:        "OpenAI Responses API",
	},
	"chatgpt": {
		credential:         "oauth",
		envVar:             "",
		requiresKey:        false,
		supportsListModels: false,
		description:        "ChatGPT via native OAuth (ChatGPT Plus/Pro subscription)",
	},
	"gemini": {
		credential:         "api_key",
		envVar:             "GEMINI_API_KEY",
		requiresKey:        true,
		supportsListModels: false,
		description:        "Google Gemini API (consumer API key)",
	},
	"gemini-cli": {
		credential:         "oauth",
		envVar:             "",
		requiresKey:        false,
		supportsListModels: false,
		description:        "Google Code Assist API (gemini-cli OAuth)",
	},
	"openrouter": {
		credential:         "api_key",
		envVar:             "OPENROUTER_API_KEY",
		requiresKey:        true,
		supportsListModels: true,
		description:        "OpenRouter API (access to many providers)",
	},
	"zen": {
		credential:         "api_key",
		envVar:             "ZEN_API_KEY",
		requiresKey:        false,
		supportsListModels: true,
		description:        "OpenCode Zen API (free tier available)",
	},
	"claude-bin": {
		credential:         "none",
		envVar:             "",
		requiresKey:        false,
		supportsListModels: false,
		description:        "Local Claude Code credentials (claude-bin CLI)",
	},
	"xai": {
		credential:         "api_key",
		envVar:             "XAI_API_KEY",
		requiresKey:        true,
		supportsListModels: true,
		description:        "xAI API (Grok models)",
	},
	"venice": {
		credential:         "api_key",
		envVar:             "VENICE_API_KEY",
		requiresKey:        true,
		supportsListModels: true,
		description:        "Venice AI (private, uncensored inference — OpenAI-compatible)",
	},
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
	// Load config (may fail if not set up, that's OK)
	cfg, _ := config.Load()

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
	seen := make(map[string]bool)
	var providers []ProviderInfo

	// Add built-in providers
	builtinNames := llm.GetBuiltInProviderNames()
	for _, name := range builtinNames {
		meta := builtinProviderMeta[name]
		info := ProviderInfo{
			Name:               name,
			Type:               name,
			Credential:         meta.credential,
			EnvVar:             meta.envVar,
			RequiresKey:        meta.requiresKey,
			SupportsListModels: meta.supportsListModels,
			Models:             llm.ProviderModels[name],
			IsBuiltin:          true,
		}

		// Check if configured
		if cfg != nil {
			if _, ok := cfg.Providers[name]; ok {
				info.Configured = true
			}
			// Also check if it's the default provider
			if cfg.DefaultProvider == name {
				info.Configured = true
			}
		}

		providers = append(providers, info)
		seen[name] = true
	}

	// Add custom configured providers
	if cfg != nil {
		for name, provCfg := range cfg.Providers {
			if seen[name] {
				continue
			}

			provType := string(config.InferProviderType(name, provCfg.Type))
			info := ProviderInfo{
				Name:               name,
				Type:               provType,
				Credential:         "api_key",
				RequiresKey:        true,
				SupportsListModels: provType == "openai_compatible",
				Configured:         true,
				IsBuiltin:          false,
			}

			// Get models from config or provider type
			if len(provCfg.Models) > 0 {
				info.Models = provCfg.Models
			} else if provCfg.Model != "" {
				info.Models = []string{provCfg.Model}
			}

			providers = append(providers, info)
		}
	}

	// Sort by name
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	return providers
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

	if provider.IsBuiltin {
		meta := builtinProviderMeta[provider.Name]
		fmt.Printf("  Description:    %s\n", meta.description)
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
	fmt.Printf("  Configured:     %v\n", provider.Configured)

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
	// Group by builtin vs custom
	var builtin, custom []ProviderInfo
	for _, p := range providers {
		if p.IsBuiltin {
			builtin = append(builtin, p)
		} else {
			custom = append(custom, p)
		}
	}

	// Find max name length for alignment
	maxLen := 0
	for _, p := range providers {
		if len(p.Name) > maxLen {
			maxLen = len(p.Name)
		}
	}

	// Show default provider
	defaultProvider := ""
	if cfg != nil {
		defaultProvider = cfg.DefaultProvider
	}

	if len(builtin) > 0 && !providersConfigured {
		fmt.Println("Built-in providers:")
		for _, p := range builtin {
			printProviderLine(p, maxLen, defaultProvider)
		}
	}

	if len(custom) > 0 {
		if len(builtin) > 0 && !providersConfigured {
			fmt.Println()
		}
		fmt.Println("Custom providers:")
		for _, p := range custom {
			printProviderLine(p, maxLen, defaultProvider)
		}
	}

	if len(providers) == 0 {
		fmt.Println("No providers found.")
		return nil
	}

	fmt.Println()
	fmt.Println("Use 'term-llm providers <name>' for details about a specific provider.")
	fmt.Println("Use 'term-llm models --provider <name>' to list available models.")

	return nil
}

func printProviderLine(p ProviderInfo, maxLen int, defaultProvider string) {
	marker := "  "
	if p.Name == defaultProvider {
		marker = "* "
	}

	var details []string

	if p.EnvVar != "" {
		if p.RequiresKey {
			details = append(details, fmt.Sprintf("%s (required)", p.EnvVar))
		} else {
			details = append(details, fmt.Sprintf("%s (optional)", p.EnvVar))
		}
	} else if p.Credential == "oauth" {
		details = append(details, "OAuth")
	} else if p.Credential == "none" {
		details = append(details, "no credentials needed")
	}

	if p.Configured && p.Name != defaultProvider {
		details = append(details, "configured")
	}

	padding := strings.Repeat(" ", maxLen-len(p.Name))
	if len(details) > 0 {
		fmt.Printf("%s%s%s  %s\n", marker, p.Name, padding, strings.Join(details, ", "))
	} else {
		fmt.Printf("%s%s\n", marker, p.Name)
	}
}
