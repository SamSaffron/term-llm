package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestHasTTYRejectsCI(t *testing.T) {
	t.Setenv("CI", "1")
	if HasTTY() {
		t.Fatal("HasTTY() = true in CI, want false")
	}
}

func TestDetectAvailableProvidersIncludesOpenRouterBeforeChatGPT(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	providers := detectAvailableProviders()
	if len(providers) == 0 {
		t.Fatal("expected provider options")
	}

	if providers[0].value != "openrouter" {
		t.Fatalf("first provider = %q, want openrouter", providers[0].value)
	}
	got := providers[1]
	if got.value != "chatgpt" {
		t.Fatalf("ChatGPT provider value = %q, want %q", got.value, "chatgpt")
	}
	if got.name != "ChatGPT (Codex) - ChatGPT OAuth" {
		t.Fatalf("ChatGPT provider name = %q, want ChatGPT (Codex) label", got.name)
	}
	if got.available {
		t.Fatal("ChatGPT provider reported available without stored OAuth credentials")
	}
}

func TestDetectAvailableProvidersMarksChatGPTReadyWithOAuthCredentials(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	credDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	credPath := filepath.Join(credDir, "chatgpt_oauth.json")
	if err := os.WriteFile(credPath, []byte(`{"access_token":"token","expires_at":4102444800}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	providers := detectAvailableProviders()
	if len(providers) == 0 {
		t.Fatal("expected provider options")
	}
	if got := providers[1]; got.value != "chatgpt" || !got.available {
		t.Fatalf("ChatGPT provider = %#v, want available chatgpt", got)
	}
}

func TestValidateProviderSelectionAllowsChatGPTWithoutStoredCredentials(t *testing.T) {
	providers := []providerOption{
		{
			name:      "ChatGPT (Codex) - ChatGPT OAuth",
			value:     "chatgpt",
			available: false,
			hint:      "run login",
		},
	}

	selected, err := validateProviderSelection(providers, "chatgpt")
	if err != nil {
		t.Fatalf("validateProviderSelection() error = %v", err)
	}
	if selected == nil || selected.value != "chatgpt" {
		t.Fatalf("selected = %#v, want chatgpt", selected)
	}
}

func TestValidateProviderSelectionRejectsUnavailableAPIKeyProvider(t *testing.T) {
	providers := []providerOption{
		{
			name:      "OpenAI - OPENAI_API_KEY",
			value:     "openai",
			available: false,
			hint:      "set OPENAI_API_KEY",
		},
	}

	_, err := validateProviderSelection(providers, "openai")
	if err == nil {
		t.Fatal("expected unavailable OpenAI provider to be rejected")
	}
}

func TestDefaultWizardProviderConfigsIncludeFastModelsForEveryProvider(t *testing.T) {
	providers := defaultWizardProviderConfigs()
	for _, option := range detectAvailableProviders() {
		provider, model := wizardGuardianDefault(option.value, providers)
		if provider == "" || model == "" {
			t.Fatalf("wizard Guardian default for %q = %s:%s", option.value, provider, model)
		}
	}
}

func TestWizardGuardianDefaultUsesSelectedProviderFastModel(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"main": {Model: "large", FastProvider: "control", FastModel: "small"},
	}
	provider, model := wizardGuardianDefault("main", providers)
	if provider != "control" || model != "small" {
		t.Fatalf("wizardGuardianDefault() = %s:%s, want control:small", provider, model)
	}
}

func TestZenSetupRequiresKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, key := range []string{"", "test-key"} {
		t.Setenv("ZEN_API_KEY", key)
		for _, provider := range detectAvailableProviders() {
			if provider.value == "zen" && provider.available != (key != "") {
				t.Fatalf("Zen availability with key %q = %v", key, provider.available)
			}
		}
	}
}

func TestHeadlessSetupRequiresConfiguredProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("PATH", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY", "VENICE_API_KEY", "NEARAI_API_KEY", "SAMBANOVA_API_KEY", "OPENROUTER_API_KEY", "ZEN_API_KEY", "OPENCODE_API_KEY"} {
		t.Setenv(key, "")
	}
	if _, err := RunHeadlessSetup(); err == nil {
		t.Fatal("expected an error without provider credentials")
	} else {
		for _, want := range []string{"https://openrouter.ai/pricing", "OPENROUTER_API_KEY", "--provider openrouter:openrouter/free", "limits"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("setup error missing %q: %v", want, err)
			}
		}
	}
	path, err := config.GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("headless setup wrote config without credentials: %v", err)
	}
	t.Setenv("ZEN_API_KEY", "test-key")
	cfg, err := RunHeadlessSetup()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "zen" || cfg.Providers["zen"].Model != "deepseek-v4-flash" {
		t.Fatalf("unexpected headless defaults: %s %+v", cfg.DefaultProvider, cfg.Providers["zen"])
	}
}
