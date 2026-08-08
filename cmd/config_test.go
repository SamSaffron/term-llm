package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"gopkg.in/yaml.v3"
)

func TestGenerateShellCompletions(t *testing.T) {
	wantShells := []string{"bash", "zsh", "fish", "powershell"}
	if !slices.Equal(configCompletionCmd.ValidArgs, wantShells) {
		t.Fatalf("valid completion shells = %v, want %v", configCompletionCmd.ValidArgs, wantShells)
	}

	for _, shell := range wantShells {
		t.Run(shell, func(t *testing.T) {
			var out bytes.Buffer
			if err := generateShellCompletion(shell, &out); err != nil {
				t.Fatalf("generate %s completion: %v", shell, err)
			}
			if out.Len() == 0 || !strings.Contains(out.String(), "term-llm") {
				t.Fatalf("generated %s completion does not reference term-llm", shell)
			}
			if shell == "fish" && !strings.Contains(out.String(), "complete -c term-llm") {
				t.Fatal("generated Fish completion does not register term-llm")
			}
		})
	}

	if err := generateShellCompletion("nushell", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown shell") {
		t.Fatalf("unknown shell error = %v", err)
	}
}

func TestShellCompletionPathsUseXDGDirectories(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(t.TempDir(), "config")
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	for _, tc := range []struct {
		shell string
		want  string
	}{
		{shell: "bash", want: filepath.Join(home, ".bash_completion.d", "term-llm")},
		{shell: "zsh", want: filepath.Join(dataHome, "zsh", "site-functions", "_term-llm")},
		{shell: "fish", want: filepath.Join(configHome, "fish", "completions", "term-llm.fish")},
		{shell: "powershell", want: filepath.Join(configHome, "powershell", "completions", "term-llm.ps1")},
	} {
		got, err := shellCompletionPath(tc.shell, home)
		if err != nil {
			t.Fatalf("shellCompletionPath(%q): %v", tc.shell, err)
		}
		if got != tc.want {
			t.Fatalf("shellCompletionPath(%q) = %q, want %q", tc.shell, got, tc.want)
		}
	}

	if _, err := shellCompletionPath("nushell", home); err == nil {
		t.Fatal("expected unknown shell path error")
	}
}

func TestXDGBaseDirFallsBackForUnsetOrRelativePaths(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, ".config")
	for _, value := range []string{"", "relative/config"} {
		t.Setenv("XDG_CONFIG_HOME", value)
		if got := xdgBaseDir("XDG_CONFIG_HOME", home, ".config"); got != want {
			t.Fatalf("xdgBaseDir with %q = %q, want %q", value, got, want)
		}
	}
}

func TestInstallFishCompletionUsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdgConfigHome := filepath.Join(t.TempDir(), "custom-config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	if err := installShellCompletion("fish"); err != nil {
		t.Fatalf("install fish completion: %v", err)
	}

	path := filepath.Join(xdgConfigHome, "fish", "completions", "term-llm.fish")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed fish completion: %v", err)
	}
	if !bytes.Contains(content, []byte("complete -c term-llm")) {
		t.Fatalf("installed file %s is not a Fish completion script", path)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "fish", "completions", "term-llm.fish")); !os.IsNotExist(err) {
		t.Fatalf("Fish completion should use XDG_CONFIG_HOME, fallback path stat error = %v", err)
	}
}

func TestConfigSet_AtomicWriteFailureLeavesExistingConfigUntouched(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
	configDir := filepath.Join(xdgHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	original := []byte("default_provider: anthropic\n")
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatalf("chmod config dir: %v", err)
	}
	defer os.Chmod(configDir, 0o755)

	err := configSet(nil, []string{"default_provider", "openai"})
	if err == nil {
		t.Skip("config directory remained writable; cannot exercise temp-file creation failure")
	}
	if !strings.Contains(err.Error(), "create temp file") && !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected atomic temp-file creation failure, got: %v", err)
	}

	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read config after failed set: %v", readErr)
	}
	if string(data) != string(original) {
		t.Fatalf("config was modified after failed atomic write:\n got: %q\nwant: %q", string(data), string(original))
	}
}

func TestConfigSet_AtomicWritePreservesExistingMode(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
	configDir := filepath.Join(xdgHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("default_provider: anthropic\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := configSet(nil, []string{"default_provider", "openai"}); err != nil {
		t.Fatalf("config set: %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected mode 0600 to be preserved, got %o", got)
	}
}

func TestEffectiveApprovalConfigValue(t *testing.T) {
	if got, ok, err := effectiveApprovalConfigValue("chat.approval_mode", &config.Config{}); err != nil || !ok || got != "auto (builtin_default)" {
		t.Fatalf("blank chat effective value = %q, %t, %v; want auto builtin_default", got, ok, err)
	}
	cfg := &config.Config{Approval: config.ApprovalConfig{DefaultMode: "prompt"}, Ask: config.AskConfig{ApprovalMode: "auto"}}
	for _, tc := range []struct {
		key  string
		want string
	}{
		{key: "chat.approval_mode", want: "prompt (global_config)"},
		{key: "ask.approval_mode", want: "auto (surface_config)"},
		{key: "serve.mcp.approval_mode", want: "prompt (global_config)"},
	} {
		got, ok, err := effectiveApprovalConfigValue(tc.key, cfg)
		if err != nil {
			t.Fatalf("effectiveApprovalConfigValue(%q): %v", tc.key, err)
		}
		if !ok || got != tc.want {
			t.Fatalf("effectiveApprovalConfigValue(%q) = %q, %t; want %q, true", tc.key, got, ok, tc.want)
		}
	}
}

func TestApprovalConfigValueCompletionsExcludeYolo(t *testing.T) {
	for _, key := range []string{"approval.default_mode", "chat.approval_mode", "serve.mcp.approval_mode"} {
		got := configValueCompletions(key, "")
		if !reflect.DeepEqual(got, []string{"prompt", "auto"}) {
			t.Fatalf("configValueCompletions(%q) = %v, want [prompt auto]", key, got)
		}
	}
}

func TestSearchProviderConfigValueCompletionsIncludeParallel(t *testing.T) {
	got := configValueCompletions("search.provider", "par")
	if !reflect.DeepEqual(got, []string{"parallel"}) {
		t.Fatalf("configValueCompletions(search.provider, par) = %v, want [parallel]", got)
	}
}

func TestDefaultConfigContentContainsEveryDefault(t *testing.T) {
	content := defaultConfigContent()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		t.Fatalf("generated default config is not valid YAML: %v\n%s", err, content)
	}
	rawKeys := make(map[string]bool)
	unknownKeys := make(map[string]bool)
	extractConfigKeys(&root, "", rawKeys, unknownKeys)
	if len(unknownKeys) > 0 {
		t.Fatalf("generated default config contains unknown keys: %#v", unknownKeys)
	}
	for key := range config.GetDefaults() {
		if !rawKeys[key] {
			t.Fatalf("generated default config missing default key %s", key)
		}
	}
	for _, want := range []string{
		"guardian:",
		"classify_all_shell:",
		"music:",
		"audio:",
		"reasoning:",
		"serve:",
		"file_tracking:",
		"max_tool_output_chars:",
		"strip_image_base64:",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated default config missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"approval:",
		"approval_mode:",
	} {
		if strings.Contains(content, unwanted) {
			t.Fatalf("generated default config unexpectedly materialized optional approval key %q", unwanted)
		}
	}
}

func TestConfigResetWritesGeneratedDefaultsAtomically(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
	if err := configReset(nil, nil); err != nil {
		t.Fatalf("config reset: %v", err)
	}
	configPath := filepath.Join(xdgHome, "term-llm", "config.yaml")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat reset config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("reset config mode = %o, want 0600", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read reset config: %v", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("reset config is not valid YAML: %v", err)
	}
	rawKeys := make(map[string]bool)
	unknownKeys := make(map[string]bool)
	extractConfigKeys(&root, "", rawKeys, unknownKeys)
	if len(unknownKeys) > 0 {
		t.Fatalf("reset config contains unknown keys: %#v", unknownKeys)
	}
	for _, key := range []string{"reasoning.display", "audio.gemini.model", "music.venice.model", "serve.response_timeout", "auto_compact"} {
		if !rawKeys[key] {
			t.Fatalf("reset config missing generated key %s", key)
		}
	}
}

func TestConfigShowRedactsSensitiveRawValues(t *testing.T) {
	raw := []byte(`providers:
  openai:
    api_key: sk-secret-test
    model: gpt-5.2
  claude-bin:
    env:
      CLAUDE_CODE_OAUTH_TOKEN: sk-ant-oat01-original
      IS_SANDBOX: "1"
  bedrock:
    secret_access_key: aws-secret-test
    session_token: aws-session-test
`)
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse raw config: %v", err)
	}
	rawKeys := make(map[string]bool)
	unknownKeys := make(map[string]bool)
	extractConfigKeys(&root, "", rawKeys, unknownKeys)
	var buf strings.Builder
	printAnnotatedConfig(&buf, config.GetDefaults(), rawKeys, unknownKeys, &root, true, nil)
	out := buf.String()
	for _, secret := range []string{"sk-secret-test", "sk-ant-oat01-original", "aws-secret-test", "aws-session-test"} {
		if strings.Contains(out, secret) {
			t.Fatalf("config output leaked sensitive value %q:\n%s", secret, out)
		}
	}
	if count := strings.Count(out, "<redacted>"); count < 4 {
		t.Fatalf("config output should redact sensitive values, got %d redactions:\n%s", count, out)
	}
	if !strings.Contains(out, "IS_SANDBOX: 1") {
		t.Fatalf("non-secret env key should still be rendered as config value? got:\n%s", out)
	}
}
