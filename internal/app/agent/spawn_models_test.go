package agent

import (
	"fmt"
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
	got := configDiscoveryCatalog().spawnModelProviders()
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

func TestValidateSpawnModelRejectsExplicitlyDisabledDebug(t *testing.T) {
	catalog := configDiscoveryCatalog()
	disabled := false
	catalog.cfg.Providers["debug"] = config.ProviderConfig{Enabled: &disabled}
	err := catalog.ValidateSpawnModel("debug:fast")
	if err == nil || !strings.Contains(err.Error(), `provider "debug" is not enabled on this host`) {
		t.Fatalf("ValidateSpawnModel(disabled debug) = %v, want early rejection", err)
	}
}

func TestValidateSpawnModelBoundsRecoverySuggestions(t *testing.T) {
	for _, count := range []int{0, 1, 3, 4, 40} {
		t.Run(fmt.Sprintf("%d providers", count), func(t *testing.T) {
			cfg := &config.Config{
				ProviderDiscovery: config.ProviderDiscoveryConfig,
				Providers:         make(map[string]config.ProviderConfig),
			}
			models := make([]string, 500)
			for i := range models {
				models[i] = fmt.Sprintf("catalog-model-%03d", i)
			}
			for i := range count {
				cfg.Providers[fmt.Sprintf("custom-%02d", i)] = config.ProviderConfig{Models: models}
			}
			if count > 0 {
				// Prefer the default even when it sorts last in a large catalog.
				cfg.DefaultProvider = fmt.Sprintf("custom-%02d", count-1)
			}
			err := NewSpawnModelCatalog(cfg).ValidateSpawnModel("codex:guessed-model")
			if err == nil {
				t.Fatal("expected unknown provider error")
			}
			message := err.Error()
			if len(message) > 500 || strings.Contains(message, "catalog-model-") {
				t.Fatalf("error includes an oversized catalog: %q", message)
			}
			if !strings.Contains(message, "omit model") {
				t.Fatalf("error lacks default fallback: %q", message)
			}
			if count == 0 {
				if strings.Contains(message, "use one of") {
					t.Fatalf("error suggests providers when none are enabled: %q", message)
				}
				return
			}
			if !strings.Contains(message, "use one of "+cfg.DefaultProvider) ||
				!strings.Contains(message, fmt.Sprintf("%q", cfg.DefaultProvider+":<model>")) {
				t.Fatalf("error does not prefer the default: %q", message)
			}
			for i := 2; i < count-1; i++ {
				if strings.Contains(message, fmt.Sprintf("custom-%02d", i)) {
					t.Fatalf("error includes more than three provider suggestions: %q", message)
				}
			}
			if count > 3 {
				for _, want := range []string{fmt.Sprintf("and %d more", count-3), "term-llm providers --configured"} {
					if !strings.Contains(message, want) {
						t.Fatalf("error %q lacks %q", message, want)
					}
				}
			} else if strings.Contains(message, "more") {
				t.Fatalf("error implies omitted providers: %q", message)
			}
		})
	}
}

func TestSpawnModelCatalogWithoutConfigAllowsEverything(t *testing.T) {
	var catalog SpawnModelCatalog
	if got := catalog.spawnModelProviders(); got != nil {
		t.Fatalf("SpawnModelProviders() = %v, want nil", got)
	}
	if err := catalog.ValidateSpawnModel("codex:gpt-5.4"); err != nil {
		t.Fatalf("ValidateSpawnModel() = %v, want nil without config", err)
	}
}
