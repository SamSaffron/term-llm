package runtime

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestResolveSessionProviderKey_PrefersCanonicalProviderKey(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"corp-openai": {},
		},
	}
	sess := &session.Session{
		Provider:    "OpenAI (gpt-5)",
		ProviderKey: "corp-openai",
		Model:       "gpt-5",
	}

	got := ResolveSessionProviderKey(cfg, sess)
	if got != "corp-openai" {
		t.Fatalf("resolveSessionProviderKey() = %q, want %q", got, "corp-openai")
	}
}

func TestResolveSessionProviderKey_InfersFromCustomDisplayPrefix(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"my-provider": {},
		},
	}
	sess := &session.Session{
		Provider: "my-provider (gpt-4.1)",
		Model:    "gpt-4.1",
	}

	got := ResolveSessionProviderKey(cfg, sess)
	if got != "my-provider" {
		t.Fatalf("resolveSessionProviderKey() = %q, want %q", got, "my-provider")
	}
}

func TestResolveSessionProviderKey_InfersCopilotDisplay(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	sess := &session.Session{
		Provider: "GitHub Copilot (gpt-5.3-codex)",
		Model:    "gpt-5.3-codex",
	}

	got := ResolveSessionProviderKey(cfg, sess)
	if got != "copilot" {
		t.Fatalf("resolveSessionProviderKey() = %q, want %q", got, "copilot")
	}
}
