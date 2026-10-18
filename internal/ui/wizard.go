package ui

import (
	"fmt"
	"github.com/samsaffron/term-llm/internal/providerspec"
	"os"
	"os/exec"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
)

const openRouterGettingStartedHint = "Need a provider? See https://openrouter.ai/pricing for OpenRouter's free plan. Set OPENROUTER_API_KEY and try --provider openrouter:openrouter/free (usage limits apply)."

// providerOption represents a provider choice in the setup wizard
type providerOption struct {
	name      string
	value     string
	available bool
	hint      string // Shows how to enable if not available
}

// detectAvailableProviders checks which providers have credentials configured
func detectAvailableProviders() []providerOption {
	options := make([]providerOption, 0, len(providerspec.All()))
	specs := providerspec.All()
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Wizard.Order < specs[j].Wizard.Order })
	for _, spec := range specs {
		if spec.Wizard.Order == 0 {
			continue
		}
		available := spec.APIKeyEnv != "" && os.Getenv(spec.APIKeyEnv) != ""
		if spec.Name == "openrouter" || spec.Name == "zen" {
			available = strings.TrimSpace(os.Getenv(spec.APIKeyEnv)) != ""
		}
		switch spec.Availability {
		case "chatgpt":
			available = credentials.ChatGPTCredentialsExist()
		case "grok":
			available = credentials.GrokCredentialsExist()
		case "claude":
			available = isClaudeBinaryAvailable()
		case "grok-bin":
			available = isGrokBinaryAvailable()
		case "cursor-bin":
			available = isCursorBinaryAvailable()
		case "agy-bin":
			available = isAgyBinaryAvailable()
		}
		options = append(options, providerOption{name: spec.Wizard.Label, value: spec.Name, available: available, hint: spec.Wizard.Hint})
	}

	return options
}

// isAnthropicAvailable checks if an Anthropic API key is available
func isAnthropicAvailable() bool {
	return os.Getenv("ANTHROPIC_API_KEY") != ""
}

// isClaudeBinaryAvailable checks if the claude CLI is in PATH
func isClaudeBinaryAvailable() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func isGrokBinaryAvailable() bool {
	_, err := exec.LookPath("grok")
	return err == nil
}

func isCursorBinaryAvailable() bool {
	_, err := exec.LookPath("cursor-agent")
	return err == nil
}

func isAgyBinaryAvailable() bool {
	_, err := exec.LookPath("agy")
	return err == nil
}

func validateProviderSelection(providers []providerOption, provider string) (*providerOption, error) {
	for i := range providers {
		if providers[i].value != provider {
			continue
		}
		selected := &providers[i]
		if !selected.available && !canBootstrapProviderInteractively(selected.value) {
			return selected, fmt.Errorf("provider %s is not configured\n\n%s", selected.name, selected.hint)
		}
		return selected, nil
	}
	return nil, nil
}

func canBootstrapProviderInteractively(provider string) bool {
	spec, ok := providerspec.Lookup(provider)
	return ok && spec.Wizard.Bootstrap
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
			available: os.Getenv("GEMINI_API_KEY") != "",
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
			name:      "xAI - XAI_API_KEY",
			value:     "xai",
			available: os.Getenv("XAI_API_KEY") != "",
			hint:      "set XAI_API_KEY",
		},
		{
			name:      "Venice - VENICE_API_KEY",
			value:     "venice",
			available: os.Getenv("VENICE_API_KEY") != "",
			hint:      "set VENICE_API_KEY",
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

// HasTTY reports whether the invoking process is interactive and a controlling
// terminal can be opened.
func HasTTY() bool {
	if !terminalpolicy.Interactive(os.Stdin, os.Stderr) {
		return false
	}
	f, err := getTTY()
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func defaultWizardProviderConfigs() map[string]config.ProviderConfig {
	providers := make(map[string]config.ProviderConfig)
	for _, spec := range providerspec.All() {
		if spec.Wizard.Order == 0 || spec.Name == "openrouter" {
			continue
		}
		name := spec.Name
		providers[name] = config.ProviderConfig{Model: config.DefaultProviderModel(name), FastModel: config.DefaultProviderFastModel(name)}
	}
	// OpenRouter retains its historical application identification defaults.

	providers["openrouter"] = config.ProviderConfig{
		Model:     config.DefaultProviderModel("openrouter"),
		FastModel: config.DefaultProviderFastModel("openrouter"),
		AppURL:    "https://github.com/samsaffron/term-llm",
		AppTitle:  "term-llm",
	}
	return providers
}

func wizardGuardianDefault(provider string, providers map[string]config.ProviderConfig) (string, string) {
	provider = strings.TrimSpace(provider)
	providerCfg := providers[provider]
	targetProvider := provider
	if fastProvider := strings.TrimSpace(providerCfg.FastProvider); fastProvider != "" {
		targetProvider = fastProvider
	}
	model := strings.TrimSpace(providerCfg.FastModel)
	if model == "" {
		// The default belongs to the provider that will receive the request.
		model = config.DefaultProviderFastModel(targetProvider)
	}
	return targetProvider, model
}

// RunHeadlessSetup auto-configures term-llm when no TTY is available (e.g. Docker).
// It picks the first provider with credentials set in the environment and saves a config.
func RunHeadlessSetup() (*config.Config, error) {
	providers := detectAvailableProviders()

	// Pick the first available provider (they're ordered by preference)
	var provider string
	for _, p := range providers {
		if p.available {
			provider = p.value
			break
		}
	}
	if provider == "" {
		return nil, fmt.Errorf("no provider credentials available; configure a provider API key or run term-llm interactively to set up a provider\n\n%s", openRouterGettingStartedHint)
	}

	// Pick image provider if available
	var imageProvider string
	for _, p := range detectAvailableImageProviders() {
		if p.available && p.value != "" {
			imageProvider = p.value
			break
		}
	}

	cfg := &config.Config{
		DefaultProvider: provider,
		Providers:       defaultWizardProviderConfigs(),
		Exec: config.ExecConfig{
			Suggestions: 3,
		},
		Image: config.ImageConfig{
			Provider: imageProvider,
		},
	}

	if err := config.Save(cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}

	path, _ := config.GetConfigPath()
	fmt.Fprintf(os.Stderr, "No TTY available — auto-configured with provider %q\n", provider)
	fmt.Fprintf(os.Stderr, "Config saved to %s\n", path)

	return config.Load()
}

// RunSetupWizard runs the first-time setup wizard and returns the config
func RunSetupWizard() (*config.Config, error) {
	if !terminalpolicy.Interactive(os.Stdin, os.Stderr) {
		return nil, fmt.Errorf("setup wizard requires an interactive terminal")
	}

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

	// Keep the default provider and subscription sign-ins easy to find,
	// then show the rest as available first, then unavailable.
	var llmOptions []huh.Option[string]
	var availableOptions []huh.Option[string]
	var unavailableOptions []huh.Option[string]

	for _, p := range providers {
		label := p.name
		if p.available {
			label = p.name + " ✓"
		} else {
			label = p.name + " (not set)"
		}

		option := huh.NewOption(label, p.value)
		if p.value == config.DefaultConfigProvider || p.value == "chatgpt" || p.value == "grok" {
			llmOptions = append(llmOptions, option)
			continue
		}
		if p.available {
			availableOptions = append(availableOptions, option)
		} else {
			unavailableOptions = append(unavailableOptions, option)
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
				Description("Providers marked ✓ are ready to use.\n\n" + openRouterGettingStartedHint).
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

	// Validate LLM provider. ChatGPT can bootstrap OAuth interactively after setup,
	// so do not reject it just because credentials are not stored yet.
	selectedProvider, err := validateProviderSelection(providers, provider)
	if err != nil {
		return nil, err
	}

	// Show selection before next step
	if ttyErr == nil {
		tty3, _ := getTTY()
		fmt.Fprintf(tty3, "\nLLM Provider: %s ✓\n\n", selectedProvider.name)
		tty3.Close()
	}

	// Step 2: Guardian selection. Guardian approves unmatched tool actions in
	// auto mode, so make the model choice visible and keep alternatives limited
	// to providers that are already usable.
	providerConfigs := defaultWizardProviderConfigs()
	defaultGuardianProvider, defaultGuardianModel := wizardGuardianDefault(provider, providerConfigs)
	guardianOptions := []huh.Option[string]{
		huh.NewOption(fmt.Sprintf("Use %s:%s (recommended)", defaultGuardianProvider, defaultGuardianModel), ""),
	}
	for _, candidate := range providers {
		if candidate.value == provider || (!candidate.available && !canBootstrapProviderInteractively(candidate.value)) {
			continue
		}
		candidateProvider, candidateModel := wizardGuardianDefault(candidate.value, providerConfigs)
		if candidateProvider == "" || candidateModel == "" {
			continue
		}
		guardianOptions = append(guardianOptions,
			huh.NewOption(fmt.Sprintf("Use %s:%s", candidateProvider, candidateModel), candidate.value))
	}

	var guardianProviderOverride string
	guardianForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which provider should Guardian use for automatic tool approvals?").
				Description("Guardian reviews commands and file changes. The recommended choice is the fast model on your default provider. Custom provider/model pins remain available in config.yaml.").
				Options(guardianOptions...).
				Value(&guardianProviderOverride),
		),
	)
	if ttyErr == nil {
		if guardianTTY, err := getTTY(); err == nil {
			defer guardianTTY.Close()
			guardianForm = guardianForm.WithInput(guardianTTY).WithOutput(guardianTTY)
		}
	}
	if err := guardianForm.Run(); err != nil {
		return nil, err
	}

	guardianConfig := config.GuardianConfig{Provider: strings.TrimSpace(guardianProviderOverride)}

	// Step 3: Image provider selection
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
		Providers:       providerConfigs,
		Guardian:        guardianConfig,
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
