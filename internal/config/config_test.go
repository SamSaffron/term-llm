package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func TestApplyOverrides(t *testing.T) {
	cfg := &Config{
		DefaultProvider: "anthropic",
		Providers: map[string]ProviderConfig{
			"anthropic": {
				Model: "claude-sonnet-4-6",
			},
			"openai": {
				Model: "gpt-5.2",
			},
			"gemini": {
				Model: "gemini-3-flash-preview",
			},
		},
	}

	cfg.ApplyOverrides("openai", "gpt-4o")
	if cfg.DefaultProvider != "openai" {
		t.Fatalf("provider=%q, want %q", cfg.DefaultProvider, "openai")
	}
	if cfg.Providers["openai"].Model != "gpt-4o" {
		t.Fatalf("openai model=%q, want %q", cfg.Providers["openai"].Model, "gpt-4o")
	}
	if cfg.Providers["anthropic"].Model != "claude-sonnet-4-6" {
		t.Fatalf("anthropic model changed unexpectedly: %q", cfg.Providers["anthropic"].Model)
	}

	cfg.ApplyOverrides("", "gemini-2.5-flash")
	if cfg.DefaultProvider != "openai" {
		t.Fatalf("provider changed unexpectedly: %q", cfg.DefaultProvider)
	}
	if cfg.Providers["openai"].Model != "gemini-2.5-flash" {
		t.Fatalf("openai model=%q, want %q", cfg.Providers["openai"].Model, "gemini-2.5-flash")
	}
}

func TestLoad_ProviderUseWebSocket(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: openai
providers:
  openai:
    model: gpt-5.2
    use_websocket: true
  chatgpt:
    model: gpt-5.5-medium
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Providers["openai"].UseWebSocket {
		t.Fatal("openai use_websocket = false, want true")
	}
	if !cfg.Providers["chatgpt"].UseWebSocket {
		t.Fatal("chatgpt use_websocket = false, want default true")
	}
}

func TestLoad_ProviderUseWebSocketExplicitFalseOverridesDefault(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: openai
providers:
  openai:
    model: gpt-5.2
    use_websocket: false
  chatgpt:
    model: gpt-5.5-medium
    use_websocket: false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Providers["openai"].UseWebSocket {
		t.Fatal("openai use_websocket = true, want explicit false")
	}
	if cfg.Providers["chatgpt"].UseWebSocket {
		t.Fatal("chatgpt use_websocket = true, want explicit false")
	}
}

func TestLoad_OnlyResolvesDefaultProviderCredentials(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	homeDir := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: openai
providers:
  openai:
    model: gpt-5.2
  gemini-cli:
    model: gemini-2.5-pro
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["openai"].ResolvedAPIKey; got != "sk-openai-test" {
		t.Fatalf("openai ResolvedAPIKey = %q, want %q", got, "sk-openai-test")
	}
	if cfg.Providers["gemini-cli"].OAuthCreds != nil {
		t.Fatal("expected gemini-cli OAuth creds to remain unresolved until used")
	}
	if err := cfg.ResolveProviderCredentials("gemini-cli"); err == nil {
		t.Fatal("expected deferred gemini-cli credential resolution to fail without oauth creds")
	}
}

func TestLoad_PreservesProviderEnvKeyCase(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: claude-bin
providers:
  claude-bin:
    model: sonnet
    env:
      IS_SANDBOX: "1"
      MY_AUTH_TOKEN: file:///tmp/oauth.json#access_token
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	providerCfg, ok := cfg.Providers["claude-bin"]
	if !ok {
		t.Fatal("expected claude-bin provider to be loaded")
	}
	if got := providerCfg.Env["IS_SANDBOX"]; got != "1" {
		t.Fatalf("IS_SANDBOX = %q, want %q", got, "1")
	}
	if got := providerCfg.Env["MY_AUTH_TOKEN"]; got != "file:///tmp/oauth.json#access_token" {
		t.Fatalf("MY_AUTH_TOKEN = %q", got)
	}
	if _, ok := providerCfg.Env["is_sandbox"]; ok {
		t.Fatal("did not expect lowercased is_sandbox key")
	}
	if _, ok := providerCfg.Env["my_auth_token"]; ok {
		t.Fatal("did not expect lowercased my_auth_token key")
	}
}

func TestSetServeTelegramConfig_PreservesProviderEnvKeyCase(t *testing.T) {
	// Regression: viper.WriteConfig() lowercases YAML keys, which corrupts
	// providers.<n>.env.<KEY> during unrelated saves like telegram setup.
	// writeConfigPreservingEnvCase must restore the original casing.
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	configYAML := `default_provider: claude-bin
providers:
  claude-bin:
    model: sonnet
    env:
      CLAUDE_CODE_OAUTH_TOKEN: sk-ant-oat01-original
      IS_SANDBOX: "1"
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := SetServeTelegramConfig(TelegramServeConfig{Token: "telegram-token"}); err != nil {
		t.Fatalf("SetServeTelegramConfig: %v", err)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(written)
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN: sk-ant-oat01-original", "IS_SANDBOX:", "telegram-token"} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q after telegram save:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"claude_code_oauth_token", "is_sandbox"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("config still contains lowercased env key %q:\n%s", unwanted, got)
		}
	}

	// Verify the round-trip Load also returns the uppercase keys.
	viper.Reset()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if got := cfg.Providers["claude-bin"].Env["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-oat01-original" {
		t.Fatalf("loaded CLAUDE_CODE_OAUTH_TOKEN = %q", got)
	}
	if got := cfg.Providers["claude-bin"].Env["IS_SANDBOX"]; got != "1" {
		t.Fatalf("loaded IS_SANDBOX = %q", got)
	}
}

func TestResolveProviderCredentials_ExpandsEnvMap(t *testing.T) {
	t.Setenv("CLAUDE_TEST_FLAG", "1")
	cfg := &ProviderConfig{
		Env: map[string]string{
			"IS_SANDBOX": "$CLAUDE_TEST_FLAG",
		},
	}
	if err := resolveProviderCredentials("claude-bin", cfg); err != nil {
		t.Fatalf("resolveProviderCredentials: %v", err)
	}
	if got := cfg.Env["IS_SANDBOX"]; got != "1" {
		t.Fatalf("env expansion = %q, want %q", got, "1")
	}
}

func TestResolveProviderCredentials_ResolvesLazyEnvMapForInference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"secret-token"}`), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cfg := &ProviderConfig{
		Env: map[string]string{
			"MY_AUTH_TOKEN": "file://" + path + "#access_token",
		},
	}
	if err := resolveProviderCredentials("claude-bin", cfg); err != nil {
		t.Fatalf("resolveProviderCredentials: %v", err)
	}
	if !cfg.needsLazyResolution {
		t.Fatal("expected lazy resolution to be enabled for file:// env value")
	}
	if err := cfg.ResolveForInference(); err != nil {
		t.Fatalf("ResolveForInference: %v", err)
	}
	if got := cfg.Env["MY_AUTH_TOKEN"]; got != "secret-token" {
		t.Fatalf("resolved env value = %q, want %q", got, "secret-token")
	}
}

func TestInferProviderType(t *testing.T) {
	tests := []struct {
		name     string
		explicit ProviderType
		want     ProviderType
	}{
		{"anthropic", "", ProviderTypeAnthropic},
		{"openai", "", ProviderTypeOpenAI},
		{"gemini", "", ProviderTypeGemini},
		{"openrouter", "", ProviderTypeOpenRouter},
		{"zen", "", ProviderTypeZen},
		{"cerebras", "", ProviderTypeOpenAICompat},
		{"groq", "", ProviderTypeOpenAICompat},
		{"custom", ProviderTypeOpenAICompat, ProviderTypeOpenAICompat},
		{"anthropic", ProviderTypeOpenAICompat, ProviderTypeOpenAICompat}, // explicit overrides
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InferProviderType(tc.name, tc.explicit)
			if got != tc.want {
				t.Errorf("InferProviderType(%q, %q) = %q, want %q", tc.name, tc.explicit, got, tc.want)
			}
		})
	}
}

func TestDescribeCredentialSource_AnthropicExplicitKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg := &ProviderConfig{APIKey: "sk-test-123"}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if source != "config api_key" {
		t.Fatalf("source=%q, want %q", source, "config api_key")
	}
}

func TestDescribeCredentialSource_AnthropicEnvKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key-456")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if source != "ANTHROPIC_API_KEY env" {
		t.Fatalf("source=%q, want %q", source, "ANTHROPIC_API_KEY env")
	}
}

func TestDescribeCredentialSource_AnthropicNone(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if found {
		t.Fatalf("expected no credential found, got source=%q", source)
	}
	if !strings.Contains(source, "ANTHROPIC_API_KEY") {
		t.Fatalf("source=%q, expected to mention ANTHROPIC_API_KEY", source)
	}
}

func TestDescribeCredentialSource_OpenAI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-123")
	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("openai", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if source != "OPENAI_API_KEY env" {
		t.Fatalf("source=%q, want %q", source, "OPENAI_API_KEY env")
	}
}

func TestDescribeCredentialSource_ZenNoKey(t *testing.T) {
	t.Setenv("ZEN_API_KEY", "")
	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("zen", cfg)
	if !found {
		t.Fatal("expected credential to be found (zen free tier)")
	}
	if !strings.Contains(source, "free tier") {
		t.Fatalf("source=%q, expected to mention free tier", source)
	}
}

func TestDescribeCredentialSource_ClaudeBin(t *testing.T) {
	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("claude-bin", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if !strings.Contains(source, "no key needed") {
		t.Fatalf("source=%q, expected to mention no key needed", source)
	}
}

func TestGetDefaultsEnablesAutoCompact(t *testing.T) {
	defaults := GetDefaults()

	got, ok := defaults["auto_compact"].(bool)
	if !ok {
		t.Fatalf("auto_compact default has unexpected type %T", defaults["auto_compact"])
	}
	if !got {
		t.Fatal("auto_compact should default to true")
	}
}

func TestGetDefaultsEnableWebSocketForOpenAIAndChatGPTOnly(t *testing.T) {
	defaults := GetDefaults()
	for _, key := range []string{"providers.openai.use_websocket", "providers.chatgpt.use_websocket"} {
		got, ok := defaults[key].(bool)
		if !ok || !got {
			t.Fatalf("%s default = %#v, want true", key, defaults[key])
		}
	}
	if _, ok := defaults["providers.openai_compatible.use_websocket"]; ok {
		t.Fatal("openai_compatible websocket default should not be set")
	}
	if _, ok := defaults["providers.openrouter.use_websocket"]; ok {
		t.Fatal("openrouter websocket default should not be set")
	}
}

func TestGetDefaultsIncludeChatGPTProviderModel(t *testing.T) {
	defaults := GetDefaults()

	got, ok := defaults["providers.chatgpt.model"].(string)
	if !ok {
		t.Fatalf("providers.chatgpt.model default has unexpected type %T", defaults["providers.chatgpt.model"])
	}
	if got != "gpt-5.5-medium" {
		t.Fatalf("providers.chatgpt.model = %q, want %q", got, "gpt-5.5-medium")
	}
	if !IsKnownKey("providers.chatgpt.model") {
		t.Fatal("providers.chatgpt.model should be a known key")
	}
}

func TestLoadBlankChatGPTProviderUsesDefaultModel(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: chatgpt
providers:
  chatgpt: {}
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["chatgpt"].Model; got != "gpt-5.5-medium" {
		t.Fatalf("providers.chatgpt.model = %q, want %q", got, "gpt-5.5-medium")
	}
}

func TestGetDefaultsIncludeChatGPTImageModel(t *testing.T) {
	defaults := GetDefaults()

	got, ok := defaults["image.chatgpt.model"].(string)
	if !ok {
		t.Fatalf("image.chatgpt.model default has unexpected type %T", defaults["image.chatgpt.model"])
	}
	if got != "gpt-5.4-mini" {
		t.Fatalf("image.chatgpt.model = %q, want %q", got, "gpt-5.4-mini")
	}
	if !KnownKeys["image.chatgpt.model"] {
		t.Fatal("KnownKeys missing image.chatgpt.model")
	}
}

func TestSave_QuotesSpecialYAMLValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		DefaultProvider: "openai",
		Exec: ExecConfig{
			Suggestions: 1,
		},
		Image: ImageConfig{
			Provider: "debug:provider",
		},
		Providers: map[string]ProviderConfig{
			"openai": {
				Model:    "gpt:5",
				BaseURL:  "https://example.test/api?x=1&y=2",
				AppTitle: "term-llm: dev",
			},
		},
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	path, err := GetConfigPath()
	if err != nil {
		t.Fatalf("GetConfigPath failed: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("saved config should be valid yaml: %v\n%s", err, string(content))
	}

	providers, ok := parsed["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers missing from saved config: %v", parsed)
	}
	openai, ok := providers["openai"].(map[string]any)
	if !ok {
		t.Fatalf("openai provider missing from saved config: %v", providers)
	}
	if got := openai["model"]; got != "gpt:5" {
		t.Fatalf("model = %#v, want %q", got, "gpt:5")
	}
	if got := openai["app_title"]; got != "term-llm: dev" {
		t.Fatalf("app_title = %#v, want %q", got, "term-llm: dev")
	}
}
