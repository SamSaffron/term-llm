package llm

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestOpenRouterDefaultRequiresKey(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, key := range []string{"", "test-key"} {
			t.Setenv("OPENROUTER_API_KEY", key)
			cfg := &config.Config{DefaultProvider: "openrouter", Providers: map[string]config.ProviderConfig{}}
			if configured {
				cfg.Providers["openrouter"] = config.ProviderConfig{}
			}
			for _, build := range []func() (Provider, error){
				func() (Provider, error) { return NewProviderByNameNoRetry(cfg, "openrouter", "") },
				func() (Provider, error) { return newProviderInternal(cfg) },
			} {
				p, err := build()
				if key == "" {
					if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
						t.Fatalf("configured=%v: want key error, got %v", configured, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				router, ok := p.(*OpenAICompatProvider)
				if !ok || router.model != "openrouter/free" || router.apiKey != key {
					t.Fatalf("configured=%v: wrong OpenRouter defaults", configured)
				}
			}
		}
	}
}
