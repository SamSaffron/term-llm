package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/viper"
)

func TestCatalogCapabilitiesMatchProviderContracts(t *testing.T) {
	tests := []struct {
		provider config.ProviderType
		search   bool
		fetch    bool
		choice   bool
		managed  bool
		inline   bool
		ordered  bool
	}{
		{config.ProviderTypeAnthropic, true, true, true, false, false, false},
		{config.ProviderTypeOpenAI, true, false, true, false, false, false},
		{config.ProviderTypeChatGPT, true, false, false, false, false, false},
		{config.ProviderTypeClaudeBin, false, false, false, true, false, false},
		{config.ProviderTypeGrokBin, true, true, false, true, true, false},
		{config.ProviderTypeCursorBin, false, false, false, true, true, true},
		{config.ProviderTypeGeminiCLI, true, false, false, false, false, false},
	}
	for _, tc := range tests {
		caps := catalogCapabilities(tc.provider)
		if caps.NativeWebSearch != tc.search || caps.NativeWebFetch != tc.fetch || caps.SupportsToolChoice != tc.choice || caps.ManagesOwnContext != tc.managed || caps.InlineToolLoop != tc.inline || caps.OrderedInlineToolEvents != tc.ordered || !caps.ToolCalls {
			t.Errorf("%s capabilities = %+v", tc.provider, caps)
		}
	}
}

type catalogListProvider struct {
	mu     sync.RWMutex
	models []llm.ModelInfo
	err    error
}

func (*catalogListProvider) Name() string                   { return "catalog" }
func (*catalogListProvider) Credential() string             { return "mock" }
func (*catalogListProvider) Capabilities() llm.Capabilities { return llm.Capabilities{ToolCalls: true} }
func (*catalogListProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused")
}
func (p *catalogListProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]llm.ModelInfo(nil), p.models...), p.err
}
func (p *catalogListProvider) setModels(models []llm.ModelInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = append([]llm.ModelInfo(nil), models...)
}
func (p *catalogListProvider) setError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func TestCatalogDebugTypeUsesStrictConfiguredModels(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"debug": {Model: "fast", Models: []string{"fast", "normal"}},
	}}
	catalog, failed := buildCatalog(t.Context(), cfg, llm.NewProviderByName, false, false)
	if len(failed) != 0 || len(catalog.Providers) != 1 {
		t.Fatalf("debug catalog = %+v failed=%v", catalog, failed)
	}
	entry := catalog.Providers[0]
	if entry.Type != string(config.ProviderTypeDebug) || entry.AllowUnlistedModels {
		t.Fatalf("debug policy inferred incorrectly: %+v", entry)
	}
	if len(entry.Models) != 2 || entry.Models[0].ID != "fast" || entry.Models[1].ID != "normal" {
		t.Fatalf("debug models = %+v", entry.Models)
	}
}

func TestCatalogUsesLiveModelsAndExplicitDynamicUnlistedPolicy(t *testing.T) {
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "vendor/new-model", DisplayName: "New", InputLimit: 123, InputPrice: 1.25, OutputPrice: 2.5}}}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"aggregator": {Type: config.ProviderTypeOpenRouter, Model: "stale-static", APIKey: "configured"},
	}}
	catalog, failed := buildCatalog(t.Context(), cfg, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil }, false, false)
	if len(failed) != 0 || len(catalog.Providers) != 1 {
		t.Fatalf("catalog = %+v failed=%v", catalog, failed)
	}
	entry := catalog.Providers[0]
	if len(entry.Models) != 1 || entry.Models[0].ID != "vendor/new-model" || entry.Models[0].InputLimit != 123 || !entry.AllowUnlistedModels {
		t.Fatalf("live catalog entry = %+v", entry)
	}
	if !catalogEntryAllowsModel(entry, "aggregator", "vendor/future-model") {
		t.Fatal("dynamic aggregator unexpectedly denied an unlisted model")
	}
	policy := Policy{DenyModels: []string{"vendor/future-model"}}
	if policy.Allows("aggregator", "vendor/future-model", false) {
		t.Fatal("unlisted-model routing bypassed model policy")
	}
}

func TestCatalogMarksUnknownLivePricingWithoutAdvertisingFree(t *testing.T) {
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "brand-new-model"}}}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {Type: config.ProviderTypeOpenAI, APIKey: "configured"},
	}}
	catalog, failed := buildCatalog(t.Context(), cfg, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil }, false, false)
	if len(failed) != 0 || len(catalog.Providers) != 1 || catalog.Providers[0].Models[0].InputPrice != -1 || catalog.Providers[0].Models[0].OutputPrice != -1 {
		t.Fatalf("unknown live pricing = %+v failed=%v", catalog.Providers, failed)
	}
}

func TestCatalogFallsBackToConfiguredModelsWhenLiveListingFails(t *testing.T) {
	provider := &catalogListProvider{err: errors.New("temporary model endpoint outage")}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"aggregator": {Type: config.ProviderTypeOpenRouter, APIKey: "configured", Models: []string{"configured-model"}},
	}}
	catalog, failed := buildCatalog(t.Context(), cfg, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil }, false, false)
	if failed["aggregator"] == nil || len(catalog.Providers) != 1 || len(catalog.Providers[0].Models) != 1 || catalog.Providers[0].Models[0].ID != "configured-model" {
		t.Fatalf("configured fallback = %+v failed=%v", catalog.Providers, failed)
	}
}

func TestCatalogAdvertisesOnlyExplicitConfiguredProvider(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("default_provider: zen\nproviders:\n  zen:\n    model: minimax-m2.5-free\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "minimax-m2.5-free"}}}
	catalog, failed := buildCatalog(t.Context(), cfg, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil }, false, false)
	if len(failed) != 0 || len(catalog.Providers) != 1 || catalog.Providers[0].Key != "zen" {
		t.Fatalf("catalog advertised Viper defaults: providers=%+v failed=%v explicit=%v", catalog.Providers, failed, cfg.ExplicitProviderNames())
	}
}

func TestCatalogOmitsConfiguredButUnauthenticatedProvider(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"openai": {Type: config.ProviderTypeOpenAI, Model: "gpt"}}}
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "gpt"}}}
	catalog, failed := buildCatalog(t.Context(), cfg, func(*config.Config, string, string) (llm.Provider, error) { return provider, nil }, false, false)
	if len(catalog.Providers) != 0 || failed["openai"] == nil {
		t.Fatalf("unauthenticated provider advertised: providers=%+v failed=%v", catalog.Providers, failed)
	}
}

type hungCatalogProvider struct {
	started chan struct{}
	once    sync.Once
}

func (*hungCatalogProvider) Name() string                   { return "hung-catalog" }
func (*hungCatalogProvider) Credential() string             { return "mock" }
func (*hungCatalogProvider) Capabilities() llm.Capabilities { return llm.Capabilities{ToolCalls: true} }
func (*hungCatalogProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused")
}
func (p *hungCatalogProvider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHungUnrelatedCatalogDoesNotDelayHealthyInference(t *testing.T) {
	dir := t.TempDir()
	clients, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	client, token, _ := clients.Add("timing-client", Policy{MaxConcurrentInference: 1})
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	strict := false
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"healthy": {Type: config.ProviderTypeZen, Model: "healthy-model", Models: []string{"healthy-model"}, AllowUnlistedModels: &strict},
		"broken":  {Type: config.ProviderTypeOpenAICompat, Model: "broken-model", Models: []string{"broken-model"}, AllowUnlistedModels: &strict},
	}}
	healthy := llm.NewMockProvider("healthy").AddTurn(llm.MockTurn{Text: "healthy response"})
	hung := &hungCatalogProvider{started: make(chan struct{})}
	server, err := NewServer(ServerConfig{
		Config: cfg, Clients: clients, Sealer: sealer, CatalogTTL: time.Millisecond, ModelListTimeout: 2 * time.Second,
		ProviderFactory: func(_ *config.Config, name, _ string) (llm.Provider, error) {
			if name == "broken" {
				return hung, nil
			}
			return healthy, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// The full catalog returns configured stale-first entries and starts live
	// per-provider refreshes. Wait until the unrelated broken lister is blocked.
	satellite := &config.Config{Gateway: config.GatewayConfig{URL: ts.URL, Token: token, CatalogTTL: "1ms", ConnectTimeout: "1s", ResponseTimeout: "1s", IdleTimeout: "1s"}, Providers: map[string]config.ProviderConfig{}}
	provider, err := llm.NewGatewayProvider(satellite, "healthy", "healthy-model")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-hung.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("broken catalog refresh did not start")
	}
	started := time.Now()
	stream, err := provider.Stream(t.Context(), llm.Request{Model: "healthy-model", Messages: []llm.Message{llm.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := collectStream(t, stream)
	if text != "healthy response" {
		t.Fatalf("healthy inference text = %q", text)
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("healthy inference waited %s for unrelated catalog; bound is 750ms", elapsed)
	}

	var release func()
	var ok bool
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		release, ok = server.acquireInference(client)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("could not reserve timing client inference slot")
		}
		time.Sleep(time.Millisecond)
	}
	defer release()
	wireRequest, err := llm.EncodeGatewayRequest(llm.Request{Model: "broken-model", Messages: []llm.Message{llm.UserText("limit")}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.InferenceRequest{Version: protocol.Version, RequestID: "req-limit", Provider: "broken", Request: wireRequest})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/g1/inference", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(protocol.VersionHeader, "1")
	started = time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("concurrency response = %d, want 429", resp.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("concurrency response waited %s for hung catalog; bound is 250ms", elapsed)
	}
}

func TestServerCatalogReloadsProviderConfigAfterTTL(t *testing.T) {
	dir := t.TempDir()
	clients, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	current := &config.Config{Providers: map[string]config.ProviderConfig{"first": {Type: config.ProviderTypeZen, Model: "model-a"}}}
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "model-a"}}}
	server, err := NewServer(ServerConfig{
		Config: current, Clients: clients, Sealer: sealer, CatalogTTL: time.Millisecond,
		ConfigLoader:    func() (*config.Config, error) { return current, nil },
		ProviderFactory: func(*config.Config, string, string) (llm.Provider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := server.currentCatalog(t.Context())
	if err != nil || len(first.Providers) != 1 || first.Providers[0].Key != "first" {
		t.Fatalf("first config catalog = %+v, %v", first.Providers, err)
	}
	current = &config.Config{Providers: map[string]config.ProviderConfig{"second": {Type: config.ProviderTypeZen, Model: "model-b"}}}
	provider.setModels([]llm.ModelInfo{{ID: "model-b"}})
	time.Sleep(2 * time.Millisecond)
	second, err := server.currentCatalog(t.Context())
	if err != nil || len(second.Providers) != 1 || second.Providers[0].Key != "second" || second.Providers[0].Models[0].ID != "model-b" {
		t.Fatalf("refreshed config catalog = %+v, %v", second.Providers, err)
	}
}

func TestServerCatalogRefreshesAndFallsBackToStaleLiveModels(t *testing.T) {
	dir := t.TempDir()
	clients, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	provider := &catalogListProvider{models: []llm.ModelInfo{{ID: "model-v1"}}}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"remote": {Type: config.ProviderTypeOpenRouter, APIKey: "configured"}}}
	server, err := NewServer(ServerConfig{
		Config: cfg, Clients: clients, Sealer: sealer, CatalogTTL: time.Millisecond,
		ProviderFactory: func(*config.Config, string, string) (llm.Provider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := server.currentCatalog(t.Context())
	if err != nil || first.Providers[0].Models[0].ID != "model-v1" {
		t.Fatalf("first catalog = %+v, %v", first, err)
	}
	time.Sleep(2 * time.Millisecond)
	provider.setError(errors.New("temporary secret upstream failure"))
	stale, err := server.currentCatalog(t.Context())
	if err != nil || stale.Providers[0].Models[0].ID != "model-v1" {
		t.Fatalf("stale catalog fallback = %+v, %v", stale, err)
	}
}
