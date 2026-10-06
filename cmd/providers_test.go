package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestWriteProvidersText_SplitsConfiguredFromSetupHints(t *testing.T) {
	clearProviderCredentialEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or-test")

	providers := []ProviderInfo{
		{Name: "chatgpt", IsBuiltin: true, Configured: true, ConfiguredVia: "default"},
		{Name: "claude-bin", Credential: "none", IsBuiltin: true, Configured: true, ConfiguredVia: "login"},
		{Name: "copilot", IsBuiltin: true, Disabled: true},
		{Name: "grok", IsBuiltin: true},
		{Name: "lmstudio", Configured: true, ConfiguredVia: "config"},
		{Name: "openrouter", IsBuiltin: true, Configured: true, ConfiguredVia: "env"},
		{Name: "vllm", IsBuiltin: true},
		{Name: "xai", IsBuiltin: true},
	}
	var out bytes.Buffer
	writeProvidersText(&out, providers, "chatgpt", "auto")

	want := `Configured:
  PROVIDER    KIND      SOURCE
* chatgpt     built-in  default
  claude-bin  built-in  CLI installed
  lmstudio    custom    config.yaml
  openrouter  built-in  $OPENROUTER_API_KEY

Available (not set up):
  PROVIDER    NEXT STEP
  grok        run term-llm auth login grok
  vllm        add providers.vllm (base_url) to config.yaml
  xai         set XAI_API_KEY

Disabled:
  PROVIDER    TO ENABLE
  copilot     remove providers.copilot.enabled: false

* default provider; configured does not verify connectivity or credentials.
Details: term-llm providers <name>
Models:  term-llm models --provider <name>
`
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestProvidersTextUnderProviderDiscoveryConfig(t *testing.T) {
	clearProviderCredentialEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")

	cfg := &config.Config{
		DefaultProvider:   "openrouter",
		ProviderDiscovery: config.ProviderDiscoveryConfig,
		Providers:         map[string]config.ProviderConfig{"anthropic": {}},
	}
	probed := false
	var providers []ProviderInfo
	for _, p := range buildProviderListWith(cfg, func(string) bool { probed = true; return true }) {
		if p.Name == "anthropic" || p.Name == "xai" || p.Name == "chatgpt" {
			providers = append(providers, p)
		}
	}
	if probed {
		t.Fatal("provider_discovery: config ran local login probes")
	}

	var out bytes.Buffer
	writeProviderDiscoveryNotice(&out, cfg.ProviderDiscoveryMode(), llm.ProviderUnavailableError(cfg, cfg.DefaultProvider))
	writeProvidersText(&out, providers, cfg.DefaultProvider, cfg.ProviderDiscoveryMode())

	want := `Provider discovery: config (only providers named under providers: in config.yaml are enabled)
Warning: default_provider: provider "openrouter" is not enabled: provider_discovery is "config" and config.yaml has no providers.openrouter block (add "openrouter: {}" under providers: to enable it)

Configured:
  PROVIDER   KIND      SOURCE
  anthropic  built-in  config.yaml

Available (not set up):
  PROVIDER   NEXT STEP
  chatgpt    add providers.chatgpt to config.yaml
  xai        add providers.xai to config.yaml

* default provider; configured does not verify connectivity or credentials.
Details: term-llm providers <name>
Models:  term-llm models --provider <name>
`
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestProviderNextStepUnderEnvDiscovery(t *testing.T) {
	clearProviderCredentialEnv(t)
	if got, want := providerNextStep("xai", config.ProviderDiscoveryEnv), "set XAI_API_KEY, or add providers.xai to config.yaml"; got != want {
		t.Fatalf("providerNextStep(xai) = %q, want %q", got, want)
	}
	if got, want := providerNextStep("chatgpt", config.ProviderDiscoveryEnv), "add providers.chatgpt to config.yaml"; got != want {
		t.Fatalf("providerNextStep(chatgpt) = %q, want %q", got, want)
	}

	var out bytes.Buffer
	cfg := &config.Config{ProviderDiscovery: config.ProviderDiscoveryEnv}
	writeProviderDiscoveryNotice(&out, cfg.ProviderDiscoveryMode(), llm.ProviderUnavailableError(cfg, "xai"))
	if got := out.String(); !strings.Contains(got, "Provider discovery: env") || !strings.Contains(got, "$XAI_API_KEY") {
		t.Fatalf("env notice = %q, want mode line and env-var hint", got)
	}
}
