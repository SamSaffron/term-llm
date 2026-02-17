package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/charmbracelet/huh"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

// providerOption represents a provider choice in the setup wizard
type providerOption struct {
	name      string
	value     string
	available bool
	hint      string // Shows how to enable if not available
}

// detectAvailableProviders checks which providers have credentials configured
func detectAvailableProviders() []providerOption {
	options := []providerOption{
		{
			name:      "Anthropic - API key or OAuth token",
			value:     "anthropic",
			available: isAnthropicAvailable(),
			hint:      "set ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or run 'claude setup-token'",
		},
		{
			name:      "Claude Code CLI - local credentials",
			value:     "claude-bin",
			available: isClaudeBinaryAvailable(),
			hint:      "install Claude Code CLI",
		},
		{
			name:      "OpenAI - OPENAI_API_KEY",
			value:     "openai",
			available: os.Getenv("OPENAI_API_KEY") != "",
			hint:      "set OPENAI_API_KEY",
		},
		{
			name:      "Gemini - GEMINI_API_KEY",
			value:     "gemini",
			available: os.Getenv("GEMINI_API_KEY") != "",
			hint:      "set GEMINI_API_KEY",
		},
		{
			name:      "Gemini Code Assist - ~/.gemini/oauth_creds.json",
			value:     "codeassist",
			available: isGeminiOAuthAvailable(),
			hint:      "run 'gemini' to login",
		},
		{
			name:      "OpenRouter - OPENROUTER_API_KEY",
			value:     "openrouter",
			available: os.Getenv("OPENROUTER_API_KEY") != "",
			hint:      "set OPENROUTER_API_KEY",
		},
		{
			name:      "Zen - free, no key required",
			value:     "zen",
			available: true, // Always available
			hint:      "",
		},
	}

	return options
}

// isAnthropicAvailable checks if any Anthropic credential source is available
func isAnthropicAvailable() bool {
	return os.Getenv("ANTHROPIC_API_KEY") != "" ||
		os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" ||
		credentials.AnthropicOAuthCredentialsExist() ||
		isClaudeBinaryAvailable()
}

// isClaudeBinaryAvailable checks if the claude CLI is in PATH
func isClaudeBinaryAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// isGeminiOAuthAvailable checks if gemini-cli OAuth credentials exist
func isGeminiOAuthAvailable() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	credPath := filepath.Join(home, ".gemini", "oauth_creds.json")
	_, err = os.Stat(credPath)
	return err == nil
}

// imageProviderOption represents an image provider choice in the setup wizard
type imageProviderOption struct {
	name      string
	value     string
	available bool
	hint      string
}

// detectAvailableImageProviders checks which image providers have credentials configured
func detectAvailableImageProviders() []imageProviderOption {
	return []imageProviderOption{
		{
			name:      "Gemini - GEMINI_API_KEY (recommended)",
			value:     "gemini",
			available: os.Getenv("GEMINI_API_KEY") != "" || isGeminiOAuthAvailable(),
			hint:      "set GEMINI_API_KEY",
		},
		{
			name:      "OpenAI - OPENAI_API_KEY",
			value:     "openai",
			available: os.Getenv("OPENAI_API_KEY") != "",
			hint:      "set OPENAI_API_KEY",
		},
		{
			name:      "Flux (Black Forest Labs) - BFL_API_KEY",
			value:     "flux",
			available: os.Getenv("BFL_API_KEY") != "",
			hint:      "set BFL_API_KEY",
		},
		{
			name:      "OpenRouter - OPENROUTER_API_KEY",
			value:     "openrouter",
			available: os.Getenv("OPENROUTER_API_KEY") != "",
			hint:      "set OPENROUTER_API_KEY",
		},
		{
			name:      "None (skip image generation)",
			value:     "",
			available: true,
			hint:      "",
		},
	}
}

// RunSetupWizard runs the first-time setup wizard and returns the config
func RunSetupWizard() (*config.Config, error) {
	// Use /dev/tty for output to bypass redirections
	tty, ttyErr := getTTY()
	if ttyErr == nil {
		defer tty.Close()
		fmt.Fprint(tty, "Welcome to term-llm! Let's get you set up.\n\n")
	} else {
		fmt.Fprint(os.Stderr, "Welcome to term-llm! Let's get you set up.\n\n")
	}

	// Detect available providers
	providers := detectAvailableProviders()

	// Build LLM provider options - available first, then unavailable
	var llmOptions []huh.Option[string]
	var availableOptions []huh.Option[string]
	var unavailableOptions []huh.Option[string]

	for _, p := range providers {
		label := p.name
		if p.available {
			label = p.name + " ✓"
			availableOptions = append(availableOptions, huh.NewOption(label, p.value))
		} else {
			label = p.name + " (not set)"
			unavailableOptions = append(unavailableOptions, huh.NewOption(label, p.value))
		}
	}
	llmOptions = append(llmOptions, availableOptions...)
	llmOptions = append(llmOptions, unavailableOptions...)

	// Detect available image providers
	imageProviders := detectAvailableImageProviders()

	// Build image provider options - available first, then unavailable
	var imageOptions []huh.Option[string]
	var imageAvailableOptions []huh.Option[string]
	var imageUnavailableOptions []huh.Option[string]

	for _, p := range imageProviders {
		if p.value == "" {
			continue // Skip "None" - add at end
		}
		label := p.name
		if p.available {
			label = p.name + " ✓"
			imageAvailableOptions = append(imageAvailableOptions, huh.NewOption(label, p.value))
		} else {
			label = p.name + " (not set)"
			imageUnavailableOptions = append(imageUnavailableOptions, huh.NewOption(label, p.value))
		}
	}
	imageOptions = append(imageOptions, imageAvailableOptions...)
	imageOptions = append(imageOptions, imageUnavailableOptions...)
	imageOptions = append(imageOptions, huh.NewOption("None (skip image generation)", "none"))

	// Step 1: LLM provider selection
	var provider string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which LLM provider do you want to use?").
				Description("Providers marked ✓ are ready to use").
				Options(llmOptions...).
				Value(&provider),
		),
	)

	if ttyErr == nil {
		tty2, _ := getTTY()
		defer tty2.Close()
		form = form.WithInput(tty2).WithOutput(tty2)
	}

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Validate LLM provider
	var selectedProvider *providerOption
	for i := range providers {
		if providers[i].value == provider {
			selectedProvider = &providers[i]
			break
		}
	}
	if selectedProvider != nil && !selectedProvider.available {
		return nil, fmt.Errorf("provider %s is not configured\n\n%s", selectedProvider.name, selectedProvider.hint)
	}

	// Show selection before next step
	if ttyErr == nil {
		tty3, _ := getTTY()
		fmt.Fprintf(tty3, "\nLLM Provider: %s ✓\n\n", selectedProvider.name)
		tty3.Close()
	}

	// Step 2: Image provider selection
	var imageProvider string
	imageForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which image generation provider do you want to use?").
				Description("For 'term-llm image' command. Providers marked ✓ are ready").
				Options(imageOptions...).
				Value(&imageProvider),
		),
	)

	if ttyErr == nil {
		tty4, _ := getTTY()
		defer tty4.Close()
		imageForm = imageForm.WithInput(tty4).WithOutput(tty4)
	}

	if err := imageForm.Run(); err != nil {
		return nil, err
	}

	// Validate image provider
	if imageProvider != "" && imageProvider != "none" {
		for _, p := range imageProviders {
			if p.value == imageProvider && !p.available {
				return nil, fmt.Errorf("image provider %s is not configured\n\n%s", p.name, p.hint)
			}
		}
	}

	cfg := &config.Config{
		DefaultProvider: provider,
		Providers: map[string]config.ProviderConfig{
			"anthropic": {
				Model: "claude-sonnet-4-6",
			},
			"openai": {
				Model: "gpt-5.2",
			},
			"claude-bin": {
				Model: "sonnet",
			},
			"openrouter": {
				Model:    "x-ai/grok-code-fast-1",
				AppURL:   "https://github.com/samsaffron/term-llm",
				AppTitle: "term-llm",
			},
			"gemini": {
				Model: "gemini-3-flash-preview",
			},
			"codeassist": {
				Model: "gemini-2.5-pro",
			},
			"zen": {
				Model: "minimax-m2.1-free",
			},
		},
		Exec: config.ExecConfig{
			Suggestions: 3,
		},
		Image: config.ImageConfig{
			Provider: func() string {
				if imageProvider == "none" {
					return ""
				}
				return imageProvider
			}(),
		},
	}

	// Save the config
	if err := config.Save(cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}

	path, _ := config.GetConfigPath()
	if tty, err := getTTY(); err == nil {
		fmt.Fprintf(tty, "Config saved to %s\n\n", path)
		tty.Close()
	} else {
		fmt.Fprintf(os.Stderr, "Config saved to %s\n\n", path)
	}

	// Reload to pick up the env var
	return config.Load()
}
