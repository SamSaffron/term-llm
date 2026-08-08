package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasTTYRejectsCI(t *testing.T) {
	t.Setenv("CI", "1")
	if HasTTY() {
		t.Fatal("HasTTY() = true in CI, want false")
	}
}

func TestDetectAvailableProvidersIncludesChatGPTCodexFirst(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	providers := detectAvailableProviders()
	if len(providers) == 0 {
		t.Fatal("expected provider options")
	}

	got := providers[0]
	if got.value != "chatgpt" {
		t.Fatalf("first provider value = %q, want %q", got.value, "chatgpt")
	}
	if got.name != "ChatGPT (Codex) - ChatGPT OAuth" {
		t.Fatalf("first provider name = %q, want ChatGPT (Codex) label", got.name)
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
	if got := providers[0]; got.value != "chatgpt" || !got.available {
		t.Fatalf("first provider = %#v, want available chatgpt", got)
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
