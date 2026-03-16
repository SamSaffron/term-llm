package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/credentials"
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
			"CLAUDE_CODE_OAUTH_TOKEN": "file://" + path + "#access_token",
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
	if got := cfg.Env["CLAUDE_CODE_OAUTH_TOKEN"]; got != "secret-token" {
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
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

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
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if source != "ANTHROPIC_API_KEY env" {
		t.Fatalf("source=%q, want %q", source, "ANTHROPIC_API_KEY env")
	}
}

func TestDescribeCredentialSource_AnthropicOAuthEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-test")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if !strings.Contains(source, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("source=%q, expected to contain CLAUDE_CODE_OAUTH_TOKEN", source)
	}
}

func TestDescribeCredentialSource_AnthropicSavedOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	// Save OAuth credentials to temp dir
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	creds := &credentials.AnthropicOAuthCredentials{
		AccessToken: "sk-ant-oat01-saved-token",
	}
	if err := credentials.SaveAnthropicOAuthCredentials(creds); err != nil {
		t.Fatalf("failed to save test credentials: %v", err)
	}

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if !strings.Contains(source, "saved OAuth") {
		t.Fatalf("source=%q, expected to contain 'saved OAuth'", source)
	}
}

func TestDescribeCredentialSource_AnthropicNone(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("anthropic", cfg)
	if found {
		t.Fatalf("expected no credential found, got source=%q", source)
	}
	if !strings.Contains(source, "prompt") {
		t.Fatalf("source=%q, expected to mention interactive prompt", source)
	}
}

func TestDescribeCredentialSource_AnthropicPriority(t *testing.T) {
	// When both API key env and OAuth env are set, API key should win
	t.Setenv("ANTHROPIC_API_KEY", "sk-api-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-oauth-token")

	cfg := &ProviderConfig{}
	source, _ := DescribeCredentialSource("anthropic", cfg)
	if source != "ANTHROPIC_API_KEY env" {
		t.Fatalf("source=%q, want %q (API key should take priority)", source, "ANTHROPIC_API_KEY env")
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
