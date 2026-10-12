package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMergesDefaultsCaseInsensitively(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, `Default_Provider: openai
providers:
  OpenAI:
    API_KEY: sk-test
    env:
      My_Token: abc
      FLAG: 1.0
  declared-null:
  declared-empty: {}
chat:
  max_turns: "7"
  instructions: ~
reasoning:
  raw: false
  extract_titles: ~
`)

	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatalf("loadUnresolved: %v", err)
	}
	if cfg.DefaultProvider != "openai" {
		t.Fatalf("DefaultProvider = %q", cfg.DefaultProvider)
	}
	openai, ok := cfg.Providers["openai"]
	if !ok || openai.APIKey != "sk-test" || openai.FromDefaults {
		t.Fatalf("openai provider = %+v (present %v)", openai, ok)
	}
	if want := map[string]string{"My_Token": "abc", "FLAG": "1.0"}; !reflect.DeepEqual(openai.Env, want) {
		t.Fatalf("env = %#v, want %#v", openai.Env, want)
	}
	for _, name := range []string{"declared-null", "declared-empty"} {
		if pc, ok := cfg.Providers[name]; !ok || pc.FromDefaults {
			t.Fatalf("provider %s = %+v (present %v), want declared", name, pc, ok)
		}
	}
	if cfg.Chat.MaxTurns != 7 {
		t.Fatalf("weakly typed max_turns = %d", cfg.Chat.MaxTurns)
	}
	if cfg.Chat.Instructions != DefaultAssistantInstructions {
		t.Fatalf("null instructions did not fall back to default: %q", cfg.Chat.Instructions)
	}
	if !cfg.Reasoning.RawSet || cfg.Reasoning.ExtractTitlesSet {
		t.Fatalf("reasoning presence RawSet=%v ExtractTitlesSet=%v", cfg.Reasoning.RawSet, cfg.Reasoning.ExtractTitlesSet)
	}
}

func TestLoadConfigFileLookup(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte("default_provider: from-cwd\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatalf("loadUnresolved: %v", err)
	}
	if cfg.DefaultProvider == "from-cwd" {
		t.Fatal("config.yaml in the working directory must not be read")
	}

	dir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ymlPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(ymlPath, []byte("default_provider: from-yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := FindConfigFile(); err != nil || got != ymlPath {
		t.Fatalf("FindConfigFile = %q, %v; want %q", got, err, ymlPath)
	}
	if cfg, err = loadUnresolved(); err != nil || cfg.DefaultProvider != "from-yml" {
		t.Fatalf("config.yml: provider %q, err %v", cfg.DefaultProvider, err)
	}

	yamlPath := writeTestConfig(t, configHome, "default_provider: from-yaml\n")
	if got, _ := FindConfigFile(); got != yamlPath {
		t.Fatalf("FindConfigFile = %q, want config.yaml to win", got)
	}
}

func TestLoadRejectsInvalidYAML(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, "chat: [\n")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "failed to read config") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestConfigWritersPreserveCommentsAndLayout(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := writeTestConfig(t, configHome, `# my config
default_provider: anthropic # main provider
providers:
  claude-bin:
    env:
      CLAUDE_CODE_OAUTH_TOKEN: tok
serve:
  telegram:
    token: old # bot token
agents: {}
`)

	if err := SetServeTelegramConfig(TelegramServeConfig{Token: "new", AllowedUserIDs: []int64{42}, IdleTimeout: 5}); err != nil {
		t.Fatalf("SetServeTelegramConfig: %v", err)
	}
	if err := SetServeWebPushConfig(WebPushConfig{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}); err != nil {
		t.Fatalf("SetServeWebPushConfig: %v", err)
	}
	keys, err := SetAgentPreference("coder", "provider", "chatgpt:gpt-5")
	if err != nil || !reflect.DeepEqual(keys, []string{"provider", "model"}) {
		t.Fatalf("SetAgentPreference provider = %v, %v", keys, err)
	}
	if _, err := SetAgentPreference("coder", "tools_enabled", "read, write"); err != nil {
		t.Fatalf("SetAgentPreference tools: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# my config",
		"default_provider: anthropic # main provider",
		"CLAUDE_CODE_OAUTH_TOKEN: tok",
		"token: new # bot token",
		"idle_timeout: 5",
		"vapid_private_key: priv",
		"agents:\n  preferences:\n    coder:\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "default_provider") > strings.Index(got, "providers:") {
		t.Fatalf("key order changed:\n%s", got)
	}

	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatalf("loadUnresolved: %v", err)
	}
	if !reflect.DeepEqual(cfg.Serve.Telegram.AllowedUserIDs, []int64{42}) || cfg.Serve.WebPush.VAPIDPublicKey != "pub" {
		t.Fatalf("serve config = %+v", cfg.Serve)
	}
	pref := cfg.Agents.Preferences["coder"]
	if pref.Provider != "chatgpt" || pref.Model != "gpt-5" || !reflect.DeepEqual(pref.ToolsEnabled, []string{"read", "write"}) {
		t.Fatalf("agent preference = %+v", pref)
	}

	if err := ClearAgentPreferences("coder"); err != nil {
		t.Fatalf("ClearAgentPreferences: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "preferences") || !strings.Contains(string(data), "# my config") {
		t.Fatalf("after clear:\n%s", data)
	}
}

func TestClearAgentPreferencesWithoutConfigFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := ClearAgentPreferences("coder"); err != nil {
		t.Fatalf("ClearAgentPreferences: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configHome, "term-llm", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config file created: %v", err)
	}
}
