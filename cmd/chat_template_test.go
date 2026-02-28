package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestResolveSettingsWithPlatform_Chat(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettingsWithPlatform(cfg, nil, CLIFlags{SystemMessage: "chat={{platform}}"}, "", "", "", 0, 20, agents.TemplatePlatformChat)
	if err != nil {
		t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
	}
	if settings.SystemPrompt != "chat=chat" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "chat=chat")
	}
}
