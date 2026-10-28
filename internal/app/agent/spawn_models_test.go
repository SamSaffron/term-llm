package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func configDiscoveryCatalog() SpawnModelCatalog {
	disabled := false
	return SpawnModelCatalog{
		cfg: &config.Config{
			DefaultProvider:   "chatgpt",
			ProviderDiscovery: config.ProviderDiscoveryConfig,
			Providers: map[string]config.ProviderConfig{
				"chatgpt":    {Model: "gpt-5.6-luna"},
				"claude-bin": {Model: "opus"},
				"openai":     {Model: "gpt-5.2", Enabled: &disabled},
				"anthropic":  {Model: "claude-sonnet-4-6", FromDefaults: true},
			},
		},
		hasLocalCredentials: func(string) bool { return true },
	}
}

func TestSpawnModelProvidersMatchPickerDiscovery(t *testing.T) {
	got := configDiscoveryCatalog().SpawnModelProviders()
	if want := []string{"chatgpt", "claude-bin"}; !slices.Equal(got, want) {
		t.Fatalf("SpawnModelProviders() = %v, want %v", got, want)
	}
}

func TestValidateSpawnModel(t *testing.T) {
	catalog := configDiscoveryCatalog()
	tests := []struct {
		name, model, wantErr string
	}{
		{name: "enabled provider", model: "chatgpt:gpt-5.4"},
		{name: "enabled provider with colon model", model: "claude-bin:opus:extra"},
		{name: "debug provider", model: "debug:fast"},
		{name: "branding guess", model: "codex:gpt-5.4", wantErr: `unknown provider "codex"`},
		{name: "invented tier", model: "expensive:default", wantErr: `unknown provider "expensive"`},
		{name: "disabled built-in", model: "openai:gpt-5.2", wantErr: `provider "openai" is not enabled on this host`},
		{name: "schema default only", model: "anthropic:claude-sonnet-4-6", wantErr: `provider "anthropic" is not enabled on this host`},
		{name: "undeclared built-in", model: "gemini:gemini-3-flash-preview", wantErr: `provider "gemini" is not enabled on this host`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := catalog.ValidateSpawnModel(tt.model)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSpawnModel(%q) = %v, want nil", tt.model, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSpawnModel(%q) = nil, want error", tt.model)
			}
			for _, want := range []string{tt.wantErr, "use one of chatgpt, claude-bin", `"chatgpt:<model>"`, "omit model"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("ValidateSpawnModel(%q) = %q, want it to contain %q", tt.model, err, want)
				}
			}
		})
	}
}

func TestSpawnModelCatalogWithoutConfigAllowsEverything(t *testing.T) {
	var catalog SpawnModelCatalog
	if got := catalog.SpawnModelProviders(); got != nil {
		t.Fatalf("SpawnModelProviders() = %v, want nil", got)
	}
	if err := catalog.ValidateSpawnModel("codex:gpt-5.4"); err != nil {
		t.Fatalf("ValidateSpawnModel() = %v, want nil without config", err)
	}
}
