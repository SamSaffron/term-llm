package cmd

import (
	"bytes"
	"testing"
)

func TestWriteProvidersText_SplitsConfiguredFromSetupHints(t *testing.T) {
	clearProviderCredentialEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or-test")

	providers := []ProviderInfo{
		{Name: "chatgpt", IsBuiltin: true, Configured: true, ConfiguredVia: "default"},
		{Name: "claude-bin", IsBuiltin: true, Configured: true, ConfiguredVia: "login"},
		{Name: "copilot", IsBuiltin: true, Disabled: true},
		{Name: "grok", IsBuiltin: true},
		{Name: "lmstudio", Configured: true, ConfiguredVia: "config"},
		{Name: "openrouter", IsBuiltin: true, Configured: true, ConfiguredVia: "env"},
		{Name: "vllm", IsBuiltin: true},
		{Name: "xai", IsBuiltin: true},
	}
	var out bytes.Buffer
	writeProvidersText(&out, providers, "chatgpt")

	want := `Configured:
  PROVIDER    KIND      SOURCE
* chatgpt     built-in  default
  claude-bin  built-in  CLI installed
  lmstudio    custom    config.yaml
  openrouter  built-in  $OPENROUTER_API_KEY

Available (not set up):
  PROVIDER    NEXT STEP
  grok        run term-llm auth login grok
  vllm        add providers.vllm to config.yaml
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
