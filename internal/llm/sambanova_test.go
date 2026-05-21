package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewProviderByName_ResolvesConfiguredSambaNovaCredentialsOnDemand(t *testing.T) {
	t.Setenv("SAMBANOVA_API_KEY", "")

	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"sambanova": {
				APIKey: "  test-key\n",
				Model:  "gpt-oss-120b",
			},
		},
	}

	provider, err := NewProviderByName(cfg, "sambanova", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}
	if got := strings.TrimSpace(cfg.Providers["sambanova"].ResolvedAPIKey); got != "test-key" {
		t.Fatalf("ResolvedAPIKey = %q, want %q", got, "test-key")
	}
}

func TestNewSambaNovaProviderTrimsAPIKey(t *testing.T) {
	provider := NewSambaNovaProvider("  test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewSambaNovaProviderStripsBearerPrefix(t *testing.T) {
	provider := NewSambaNovaProvider("  Bearer test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewSambaNovaProviderDefaultsModel(t *testing.T) {
	provider := NewSambaNovaProvider("k", "")
	if provider.model != "gpt-oss-120b" {
		t.Fatalf("model = %q, want gpt-oss-120b", provider.model)
	}
}

func TestCreateProviderFromConfig_SambaNovaRequiresAPIKey(t *testing.T) {
	t.Setenv("SAMBANOVA_API_KEY", "")

	_, err := createProviderFromConfig("sambanova", &config.ProviderConfig{Model: "gpt-oss-120b"})
	if err == nil {
		t.Fatal("expected missing SambaNova API key to return an error")
	}
	if !strings.Contains(err.Error(), "SAMBANOVA_API_KEY") {
		t.Fatalf("expected SAMBANOVA_API_KEY guidance, got %v", err)
	}
}

func TestCreateProviderFromConfig_SambaNovaTrimsConfiguredAPIKey(t *testing.T) {
	t.Setenv("SAMBANOVA_API_KEY", "")

	provider, err := createProviderFromConfig("sambanova", &config.ProviderConfig{
		ResolvedAPIKey: "  test-key\n",
		Model:          "gpt-oss-120b",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sn, ok := provider.(*SambaNovaProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *SambaNovaProvider", provider)
	}
	if sn.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", sn.apiKey, "test-key")
	}
}

func TestSambaNovaProviderCapabilities(t *testing.T) {
	provider := NewSambaNovaProvider("key", "")
	caps := provider.Capabilities()
	if caps.NativeWebSearch {
		t.Fatal("expected NativeWebSearch=false")
	}
	if caps.NativeWebFetch {
		t.Fatal("expected NativeWebFetch=false")
	}
	if !caps.ToolCalls {
		t.Fatal("expected ToolCalls=true")
	}
	if !caps.SupportsToolChoice {
		t.Fatal("expected SupportsToolChoice=true")
	}
}

func TestSambaNovaProviderListModelsUsesProviderScopedLimits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-oss-120b","object":"model","created":1,"owned_by":"sambanova"},{"id":"DeepSeek-V3.1","object":"model","created":1,"owned_by":"sambanova"}]}`))
	}))
	defer ts.Close()

	provider := &SambaNovaProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "gpt-oss-120b", "SambaNova")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ListModels() returned %d models, want 2", len(models))
	}
	if models[0].InputLimit != 131_072 {
		t.Fatalf("gpt-oss-120b InputLimit = %d, want 131072", models[0].InputLimit)
	}
	if models[1].InputLimit != 131_072 {
		t.Fatalf("DeepSeek-V3.1 InputLimit = %d, want 131072", models[1].InputLimit)
	}
}
