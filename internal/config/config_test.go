package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func TestWriteFileAtomicallyFollowsFinalPathSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on Windows")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	link := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink("target.yaml", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if err := WriteFileAtomically(link, []byte("new\n"), 0o644); err != nil {
		t.Fatalf("atomic write via symlink: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected config path to remain a symlink, got mode %s", linkInfo.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "new\n" {
		t.Fatalf("target content = %q, want new", string(data))
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected target mode 0600 to be preserved, got %o", got)
	}
}

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

func TestWriteFileAtomically_ReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("write old config: %v", err)
	}

	if err := writeFileAtomically(path, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomically: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != "new\n" {
		t.Fatalf("config = %q, want %q", got, "new\n")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want %o", info.Mode().Perm(), 0o600)
	}
}

func TestWriteFileAtomically_CreateTempFailureLeavesExistingFileUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions behave differently on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("write original config: %v", err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("restore dir permissions: %v", err)
		}
	}()

	err := writeFileAtomically(path, []byte("updated\n"), 0o600)
	if err == nil {
		t.Fatal("expected writeFileAtomically to fail")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Fatalf("error = %v, want create temp file", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read original config: %v", readErr)
	}
	if string(got) != "original\n" {
		t.Fatalf("config changed on failure: got %q want %q", got, "original\n")
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
		{"nearai", "", ProviderTypeNearAI},
		{"zen", "", ProviderTypeZen},
		{"claude-bin", "", ProviderTypeClaudeBin},
		{"grok-bin", "", ProviderTypeGrokBin},
		{"vllm", "", ProviderTypeVLLM},
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

func TestDescribeCredentialSource_GrokBin(t *testing.T) {
	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("grok-bin", cfg)
	if !found {
		t.Fatal("expected credential to be found")
	}
	if source != "grok CLI login (no key needed)" {
		t.Fatalf("source=%q, want grok CLI login description", source)
	}
}

func TestGrokBinSchemaDefaults(t *testing.T) {
	if got := DefaultProviderModel("grok-bin"); got != "grok-4.5" {
		t.Fatalf("DefaultProviderModel(grok-bin) = %q, want grok-4.5", got)
	}
	if got := DefaultProviderFastModels()["grok-bin"]; got != "grok-composer-2.5-fast" {
		t.Fatalf("grok-bin fast model = %q, want grok-composer-2.5-fast", got)
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

func TestReasoningDefaultsAndKnownKeys(t *testing.T) {
	defaults := GetDefaults()
	checks := map[string]any{
		"reasoning.display":           ReasoningDisplayAuto,
		"reasoning.source":            ReasoningSourceSummaryOrProviderSafe,
		"reasoning.status":            ReasoningStatusTitle,
		"reasoning.history":           ReasoningHistoryCollapsed,
		"reasoning.export":            ReasoningExportAsk,
		"reasoning.raw":               false,
		"reasoning.max_summary_chars": 12000,
		"reasoning.max_raw_chars":     20000,
		"reasoning.extract_titles":    true,
		"reasoning.hidden_label":      "Thinking...",
		"reasoning.persist_summaries": true,
		"reasoning.chat.display":      ReasoningInherit,
		"reasoning.ask.display":       ReasoningInherit,
		"reasoning.serve.display":     ReasoningInherit,
		"reasoning.jobs.display":      ReasoningInherit,
	}
	for key, want := range checks {
		if got := defaults[key]; got != want {
			t.Fatalf("default %s = %#v, want %#v", key, got, want)
		}
		if !KnownKeys[key] {
			t.Fatalf("KnownKeys missing %s", key)
		}
	}
}

func TestChatTerminalTitleDefaultAndKnownKey(t *testing.T) {
	defaults := GetDefaults()
	checks := map[string]any{
		"chat.terminal_title":        "smart",
		"chat.terminal_title_format": "",
		"chat.terminal_progress":     false,
	}
	for key, want := range checks {
		if got := defaults[key]; got != want {
			t.Fatalf("%s default = %#v, want %#v", key, got, want)
		}
		if !KnownKeys[key] {
			t.Fatalf("KnownKeys missing %s", key)
		}
	}
}

func TestFileTrackingDefaultsAndKnownKeys(t *testing.T) {
	defaults := GetDefaults()
	checks := map[string]any{
		"file_tracking.enabled":           false,
		"file_tracking.max_file_bytes":    2097152,
		"file_tracking.max_session_bytes": 104857600,
		"file_tracking.max_total_bytes":   1073741824,
		"file_tracking.path":              "",
	}
	for key, want := range checks {
		if got := defaults[key]; got != want {
			t.Fatalf("default %s = %#v, want %#v", key, got, want)
		}
		if !KnownKeys[key] {
			t.Fatalf("KnownKeys missing %s", key)
		}
	}
}

func TestEveryDefaultIsKnownKey(t *testing.T) {
	for key := range GetDefaults() {
		if !IsKnownKey(key) {
			t.Fatalf("default key %s is not accepted by IsKnownKey", key)
		}
	}
}

func TestSchemaCoversConfigMapstructureFields(t *testing.T) {
	for _, key := range collectMapstructurePaths(reflect.TypeOf(Config{}), "") {
		if !IsKnownKey(key) {
			t.Fatalf("Config mapstructure field %s is missing from schema/KnownKeys", key)
		}
	}
}

func TestProviderSchemaCoversProviderConfigFields(t *testing.T) {
	for _, key := range collectMapstructurePaths(reflect.TypeOf(ProviderConfig{}), "") {
		if !IsKnownKey("providers.example." + key) {
			t.Fatalf("ProviderConfig mapstructure field %s is missing from provider schema", key)
		}
	}
	for _, key := range collectMapstructurePaths(reflect.TypeOf(AgentPreference{}), "") {
		if !KnownAgentPreferenceKeys[key] {
			t.Fatalf("AgentPreference mapstructure field %s is missing from KnownAgentPreferenceKeys", key)
		}
	}
}

func TestCanonicalDefaultsCoverage(t *testing.T) {
	defaults := GetDefaults()
	checks := map[string]any{
		"approval.default_mode":         DefaultApprovalMode,
		"audio.gemini.model":            DefaultAudioGeminiModel,
		"audio.gemini.voice":            DefaultAudioGeminiVoice,
		"audio.gemini.format":           DefaultAudioGeminiFormat,
		"audio.elevenlabs.model":        DefaultAudioElevenLabsModel,
		"audio.elevenlabs.voice":        DefaultAudioElevenLabsVoice,
		"audio.elevenlabs.format":       DefaultAudioElevenLabsFormat,
		"music.provider":                DefaultMusicProvider,
		"music.output_dir":              DefaultMusicOutputDir,
		"music.venice.model":            DefaultMusicVeniceModel,
		"music.elevenlabs.model":        DefaultMusicElevenLabsModel,
		"transcription.save_dir":        "",
		"transcription.timestamps":      false,
		"serve.base_path":               DefaultServeBasePath,
		"serve.response_timeout":        DefaultServeResponseTimeout,
		"sessions.strip_image_base64":   false,
		"tools.max_tool_output_chars":   DefaultToolsMaxToolOutputChars,
		"skills.metadata_budget_tokens": DefaultSkillsMetadataBudgetTokens,
	}
	for key, want := range checks {
		if got, ok := defaults[key]; !ok || got != want {
			t.Fatalf("default %s = %#v (present %t), want %#v", key, got, ok, want)
		}
		if !IsKnownKey(key) {
			t.Fatalf("%s should be known", key)
		}
	}
	optionalKnown := []string{
		"guardian.provider",
		"guardian.model",
		"music.venice.api_key",
		"embed.provider",
		"serve.title",
		"providers.example.reasoning",
		"providers.example.model_map.my_alias",
		"providers.example.region",
		"providers.example.use_websocket",
		"providers.example.vision_via",
	}
	for _, key := range optionalKnown {
		if !IsKnownKey(key) {
			t.Fatalf("optional key %s should be known", key)
		}
	}
}

func TestDefaultReasoningConfigMatchesSchemaDefaults(t *testing.T) {
	defaults := GetDefaults()
	r := DefaultReasoningConfig()
	checks := map[string]any{
		"reasoning.display":           r.Display,
		"reasoning.source":            r.Source,
		"reasoning.status":            r.Status,
		"reasoning.history":           r.History,
		"reasoning.export":            r.Export,
		"reasoning.raw":               r.Raw,
		"reasoning.max_summary_chars": r.MaxSummaryChars,
		"reasoning.max_raw_chars":     r.MaxRawChars,
		"reasoning.extract_titles":    r.ExtractTitles,
		"reasoning.hidden_label":      r.HiddenLabel,
		"reasoning.persist_summaries": r.PersistSummaries,
	}
	for key, want := range checks {
		if got := defaults[key]; got != want {
			t.Fatalf("%s = %#v, want DefaultReasoningConfig value %#v", key, got, want)
		}
	}
}

func TestNoViperSetDefaultOutsideConfigPackage(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	pattern := "viper." + "SetDefault("
	allowed := filepath.Clean(filepath.Join(root, "internal", "config", "config.go"))
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "docs-site", "term-llm":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || filepath.Clean(path) == allowed {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), pattern) {
			t.Fatalf("unexpected viper.SetDefault outside internal/config/config.go: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}

func TestSchemaHasNoLeafPrefixCollisions(t *testing.T) {
	paths := make([]string, 0)
	for _, spec := range ConfigKeySpecs() {
		paths = append(paths, spec.Path)
	}
	for _, spec := range ProviderKeySpecs() {
		paths = append(paths, "providers.*."+spec.Path)
	}
	for i, path := range paths {
		for j, other := range paths {
			if i == j {
				continue
			}
			if strings.HasPrefix(other, path+".") {
				t.Fatalf("schema path %q is both a leaf and prefix of %q", path, other)
			}
		}
	}
}

func collectMapstructurePaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var paths []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("mapstructure")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		paths = append(paths, path)

		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			paths = append(paths, collectMapstructurePaths(ft, path)...)
		}
	}
	return paths
}

func TestResolveReasoningPartialConfigPreservesSafeDefaults(t *testing.T) {
	cfg := &Config{Reasoning: ReasoningConfig{Display: ReasoningDisplayOff}}
	resolved := cfg.ResolveReasoning("chat")
	if resolved.Display != ReasoningDisplayOff {
		t.Fatalf("display = %q, want off", resolved.Display)
	}
	if !resolved.ExtractTitles {
		t.Fatal("partial config should preserve extract_titles default")
	}
	if !resolved.PersistSummaries {
		t.Fatal("partial config should preserve persist_summaries default")
	}

	cfg = &Config{Reasoning: ReasoningConfig{MaxSummaryChars: 500, MaxRawChars: 1000, HiddenLabel: "Pondering..."}}
	resolved = cfg.ResolveReasoning("chat")
	if !resolved.ExtractTitles {
		t.Fatal("style-only partial config should preserve extract_titles default")
	}
	if !resolved.PersistSummaries {
		t.Fatal("style-only partial config should preserve persist_summaries default")
	}
}

func TestResolveReasoningExplicitFalsePresenceOverridesDefaults(t *testing.T) {
	cfg := &Config{Reasoning: ReasoningConfig{
		ExtractTitlesSet:    true,
		PersistSummariesSet: true,
	}}
	resolved := cfg.ResolveReasoning("chat")
	if resolved.ExtractTitles {
		t.Fatal("explicit extract_titles=false should override default true")
	}
	if resolved.PersistSummaries {
		t.Fatal("explicit persist_summaries=false should override default true")
	}

	base := DefaultReasoningConfig()
	base.Raw = true
	merged := mergeReasoningConfig(base, ReasoningConfig{RawSet: true})
	if merged.Raw {
		t.Fatal("explicit raw=false should override raw=true base")
	}
}

func TestLoadReasoningExplicitFalseOverridesDefaults(t *testing.T) {
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
reasoning:
  extract_titles: false
  persist_summaries: false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolved := cfg.ResolveReasoning("chat")
	if resolved.ExtractTitles {
		t.Fatal("loaded extract_titles=false should override default true")
	}
	if resolved.PersistSummaries {
		t.Fatal("loaded persist_summaries=false should override default true")
	}
}

func TestResolveReasoningSurfaceOverrides(t *testing.T) {
	raw := true
	cfg := &Config{Reasoning: ReasoningConfig{
		Display: ReasoningDisplayAuto,
		Status:  ReasoningStatusTitle,
		History: ReasoningHistoryCollapsed,
		Export:  ReasoningExportAsk,
		Chat: ReasoningSurfaceConfig{
			Display: ReasoningDisplayExpanded,
			Status:  ReasoningStatusSummary,
			History: ReasoningHistoryExpanded,
			Export:  ReasoningExportSummaries,
			Raw:     &raw,
		},
	}}

	resolved := cfg.ResolveReasoning("chat")
	if resolved.Display != ReasoningDisplayExpanded || resolved.Status != ReasoningStatusSummary || resolved.History != ReasoningHistoryExpanded || resolved.Export != ReasoningExportSummaries || !resolved.Raw {
		t.Fatalf("unexpected chat reasoning override: %+v", resolved)
	}

	ask := cfg.ResolveReasoning("ask")
	if ask.Display != ReasoningDisplayAuto || ask.Status != ReasoningStatusTitle || ask.History != ReasoningHistoryCollapsed || ask.Export != ReasoningExportAsk || ask.Raw {
		t.Fatalf("ask should inherit top-level safe defaults, got %+v", ask)
	}
}

func TestResolveReasoningRawEnvOverride(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "1")
	resolved := (&Config{}).ResolveReasoning("chat")
	if resolved.Display != ReasoningDisplayRaw || resolved.Source != ReasoningSourceAll || !resolved.Raw {
		t.Fatalf("raw env override resolved to display=%q source=%q raw=%t", resolved.Display, resolved.Source, resolved.Raw)
	}
}

func TestGetDefaultsEnableWebSocketForChatGPTOnly(t *testing.T) {
	defaults := GetDefaults()
	openAI, ok := defaults["providers.openai.use_websocket"].(bool)
	if !ok || openAI {
		t.Fatalf("providers.openai.use_websocket default = %#v, want false", defaults["providers.openai.use_websocket"])
	}
	chatGPT, ok := defaults["providers.chatgpt.use_websocket"].(bool)
	if !ok || !chatGPT {
		t.Fatalf("providers.chatgpt.use_websocket default = %#v, want true", defaults["providers.chatgpt.use_websocket"])
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

func TestGetDefaultsIncludeSambaNovaProviderModel(t *testing.T) {
	defaults := GetDefaults()

	got, ok := defaults["providers.sambanova.model"].(string)
	if !ok || got != "gpt-oss-120b" {
		t.Fatalf("providers.sambanova.model = %v, want gpt-oss-120b", defaults["providers.sambanova.model"])
	}
	fast, ok := defaults["providers.sambanova.fast_model"].(string)
	if !ok || fast != "Meta-Llama-3.3-70B-Instruct" {
		t.Fatalf("providers.sambanova.fast_model = %v, want Meta-Llama-3.3-70B-Instruct", defaults["providers.sambanova.fast_model"])
	}
	if !IsKnownKey("providers.sambanova.model") {
		t.Fatal("providers.sambanova.model should be a known key")
	}
}

func TestGetDefaultsIncludeNearAIProviderModel(t *testing.T) {
	defaults := GetDefaults()

	got, ok := defaults["providers.nearai.model"].(string)
	if !ok || got != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("providers.nearai.model = %v, want zai-org/GLM-5.1-FP8", defaults["providers.nearai.model"])
	}
	fast, ok := defaults["providers.nearai.fast_model"].(string)
	if !ok || fast != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Fatalf("providers.nearai.fast_model = %v, want Qwen/Qwen3.6-35B-A3B-FP8", defaults["providers.nearai.fast_model"])
	}
	if !IsKnownKey("providers.nearai.model") {
		t.Fatal("providers.nearai.model should be a known key")
	}
}

func TestDescribeCredentialSource_SambaNova(t *testing.T) {
	t.Setenv("SAMBANOVA_API_KEY", "sn-env-key")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("sambanova", cfg)
	if !found {
		t.Fatal("expected credential source")
	}
	if !strings.Contains(source, "SAMBANOVA_API_KEY") {
		t.Fatalf("source = %q, want SAMBANOVA_API_KEY", source)
	}
}

func TestDescribeCredentialSource_NearAI(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "near-env-key")

	cfg := &ProviderConfig{}
	source, found := DescribeCredentialSource("nearai", cfg)
	if !found {
		t.Fatal("expected credential source")
	}
	if !strings.Contains(source, "NEARAI_API_KEY") {
		t.Fatalf("source = %q, want NEARAI_API_KEY", source)
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

func TestLoadProviderFileUploadConfig(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("OPENAI_API_KEY", "test-key")

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: openai
providers:
  openai:
    model: gpt-5.2
    file_upload:
      native_mime_types: []
      max_native_bytes: 1234
      text_embed_mime_types:
        - text/csv
      max_text_embed_bytes: 5678
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fu := cfg.Providers["openai"].FileUpload
	if fu == nil {
		t.Fatal("file_upload config was not loaded")
	}
	if fu.NativeMimeTypes == nil || len(fu.NativeMimeTypes) != 0 {
		t.Fatalf("native_mime_types = %#v, want explicit empty list", fu.NativeMimeTypes)
	}
	if fu.MaxNativeBytes != 1234 || fu.MaxTextEmbedBytes != 5678 {
		t.Fatalf("file_upload byte limits = %d/%d", fu.MaxNativeBytes, fu.MaxTextEmbedBytes)
	}
	if len(fu.TextEmbedMimeTypes) != 1 || fu.TextEmbedMimeTypes[0] != "text/csv" {
		t.Fatalf("text_embed_mime_types = %#v", fu.TextEmbedMimeTypes)
	}
	if !IsKnownKey("providers.openai.file_upload.native_mime_types") || !IsKnownKey("providers.openai.file_upload.max_native_bytes") {
		t.Fatal("file_upload nested keys should be known")
	}
}

func TestLoadProviderModelObjectConfigs(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configYAML := `default_provider: custom
providers:
  custom:
    type: openai_compatible
    base_url: https://api.example.com/v1
    api_key: test-key
    model: friendly-name
    vision_via: openai:gpt-4.1
    models:
      - id: upstream/model-id
        alias: friendly-name
        context_window: 1048576
        max_output_tokens: 131072
        parse_reasoning: true
        include_reasoning: true
        thinking_param: enable_thinking
        reasoning_efforts: [high, max]
        vision_via: gemini:gemini-2.5-pro
      - another-upstream-model
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pc := cfg.Providers["custom"]
	if got := pc.Models; len(got) != 4 || got[0] != "friendly-name" || got[1] != "another-upstream-model" || got[2] != "friendly-name-high" || got[3] != "friendly-name-max" {
		t.Fatalf("Models = %#v, want aliases/string entries plus configured effort variants", got)
	}
	if len(pc.ModelConfigs) != 1 {
		t.Fatalf("ModelConfigs len = %d, want 1", len(pc.ModelConfigs))
	}
	mc := pc.ModelConfigs[0]
	if mc.ID != "upstream/model-id" || mc.Alias != "friendly-name" || mc.ContextWindow != 1048576 || mc.MaxOutputTokens != 131072 {
		t.Fatalf("ModelConfig basic fields = %+v", mc)
	}
	if mc.ParseReasoning == nil || !*mc.ParseReasoning || mc.IncludeReasoning == nil || !*mc.IncludeReasoning {
		t.Fatalf("reasoning parser flags = %v/%v, want true/true", mc.ParseReasoning, mc.IncludeReasoning)
	}
	if mc.ThinkingParam != "enable_thinking" {
		t.Fatalf("ThinkingParam = %q, want enable_thinking", mc.ThinkingParam)
	}
	if len(mc.ReasoningEfforts) != 2 || mc.ReasoningEfforts[0] != "high" || mc.ReasoningEfforts[1] != "max" {
		t.Fatalf("ReasoningEfforts = %#v, want [high max]", mc.ReasoningEfforts)
	}
	if mc.VisionVia != "gemini:gemini-2.5-pro" {
		t.Fatalf("VisionVia = %q, want gemini:gemini-2.5-pro", mc.VisionVia)
	}
	if got := VisionViaForProviderModel(cfg, "custom", "friendly-name-high"); got != "gemini:gemini-2.5-pro" {
		t.Fatalf("VisionViaForProviderModel alias effort = %q", got)
	}
	if got := VisionViaForProviderModel(cfg, "custom", "upstream/model-id"); got != "gemini:gemini-2.5-pro" {
		t.Fatalf("VisionViaForProviderModel upstream id = %q", got)
	}
	if got := VisionViaForProviderModel(cfg, "custom", "another-upstream-model"); got != "openai:gpt-4.1" {
		t.Fatalf("VisionViaForProviderModel provider fallback = %q", got)
	}
	if got := DisplayModelForProviderModel(cfg, "custom", "upstream/model-id"); got != "friendly-name" {
		t.Fatalf("DisplayModelForProviderModel upstream id = %q", got)
	}
	if got := DisplayModelForProviderModel(cfg, "custom", "upstream/model-id-high"); got != "friendly-name-high" {
		t.Fatalf("DisplayModelForProviderModel upstream effort = %q", got)
	}
	if got := DisplayModelForProviderModel(cfg, "custom", "friendly-name-max"); got != "friendly-name-max" {
		t.Fatalf("DisplayModelForProviderModel alias effort = %q", got)
	}
	if got := DisplayModelForProviderModel(cfg, "custom", "another-upstream-model"); got != "another-upstream-model" {
		t.Fatalf("DisplayModelForProviderModel unaliased model = %q", got)
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

func TestResolveSearchCredentialsExaMCPEnvFallbackOnlyForOfficialURL(t *testing.T) {
	t.Setenv("EXA_API_KEY", "env-key")

	official := SearchConfig{}
	resolveSearchCredentials(&official)
	if official.ExaMCP.APIKey != "env-key" {
		t.Fatalf("official Exa MCP API key = %q, want env fallback", official.ExaMCP.APIKey)
	}

	custom := SearchConfig{ExaMCP: SearchExaMCPConfig{URL: "https://mcp.example.test/mcp"}}
	resolveSearchCredentials(&custom)
	if custom.ExaMCP.APIKey != "" {
		t.Fatalf("custom Exa MCP API key = %q, want no implicit env fallback", custom.ExaMCP.APIKey)
	}

	explicit := SearchConfig{ExaMCP: SearchExaMCPConfig{URL: "https://mcp.example.test/mcp", APIKey: "${EXA_API_KEY}"}}
	resolveSearchCredentials(&explicit)
	if explicit.ExaMCP.APIKey != "env-key" {
		t.Fatalf("explicit custom Exa MCP API key = %q, want expanded env value", explicit.ExaMCP.APIKey)
	}
}

func TestApprovalDefaultModeDefaultIsPrompt(t *testing.T) {
	defaults := GetDefaults()
	got, ok := defaults["approval.default_mode"].(string)
	if !ok {
		t.Fatalf("approval.default_mode default missing or not string: %#v", defaults["approval.default_mode"])
	}
	if got != "prompt" {
		t.Fatalf("approval.default_mode = %q, want prompt", got)
	}
}
