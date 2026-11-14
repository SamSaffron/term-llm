package llm

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/credentials"
	"github.com/term-llm/term-llm/internal/llm/core"
	rp "github.com/term-llm/term-llm/internal/llm/providers/responsesproviders"
)

func TestCurrentOpenAIDefaultModelMetadata(t *testing.T) {
	for _, tc := range []struct {
		model   string
		efforts []string
	}{
		{"gpt-6.1-sol", []string{"low", "medium", "high", "xhigh", "max"}},
		{"gpt-6-luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if !slices.Contains(ProviderModelIDs("openai"), tc.model) {
				t.Fatalf("default %q missing from fallback catalog", tc.model)
			}
			if got := ReasoningEffortsForProviderModel("openai", tc.model); !slices.Equal(got, tc.efforts) {
				t.Fatalf("efforts=%v want %v", got, tc.efforts)
			}
			if got := InputLimitForProviderModel("openai", tc.model); got != 922_000 {
				t.Fatalf("API input=%d want 1050000-128000=922000", got)
			}
			if got := InputLimitForModel(tc.model + "-high"); got != 922_000 {
				t.Fatalf("canonical suffixed input=%d", got)
			}
			if got := OutputLimitForModel(tc.model); got != 128_000 {
				t.Fatalf("output=%d want 128000", got)
			}
			for _, effort := range tc.efforts {
				base, parsed := BaseModelAndEffortForProvider("openai", tc.model+"-"+effort)
				if base != tc.model || parsed != effort {
					t.Fatalf("parse %s-%s => %q/%q", tc.model, effort, base, parsed)
				}
			}
		})
	}
	if IsModelOrEffortVariant("openai", "gpt-6.1-sol-none", "gpt-6.1-sol") {
		t.Fatal("Sol 6.1 must not inherit Luna's none effort")
	}
}

func TestCurrentProviderDefaultsConstructWithoutNetwork(t *testing.T) {
	for _, provider := range []string{"openai", "chatgpt"} {
		t.Run(provider, func(t *testing.T) {
			model := config.DefaultProviderModel(provider)
			fast := config.DefaultProviderFastModel(provider)
			cfg := &config.Config{DefaultProvider: provider, Providers: map[string]config.ProviderConfig{provider: {Model: model, FastModel: fast, APIKey: "test-key"}}}
			for _, selected := range []string{model, fast + "-high"} {
				var p core.Provider
				if provider == "chatgpt" {
					creds := &credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}
					p = rp.NewChatGPTProviderWithCreds(creds, selected)
				} else {
					var err error
					p, err = NewProviderByNameNoRetry(cfg, provider, selected)
					if err != nil {
						t.Fatal(err)
					}
				}
				base, _ := BaseModelAndEffortForProvider(provider, selected)
				if !strings.Contains(p.Name(), base) {
					t.Fatalf("constructor selected unexpected model: %s", p.Name())
				}
			}
			key, fastModel, ok := ResolveFastTarget(cfg, provider)
			if !ok || key != provider || fastModel != "gpt-6-luna" {
				t.Fatalf("fast target=%s:%s ok=%v", key, fastModel, ok)
			}
		})
	}
}
