package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

func gatewayCatalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/g1/catalog" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Catalog{Version: protocol.Version, GeneratedAt: time.Now(), Providers: []protocol.CatalogEntry{{Key: "remote", Models: []protocol.Model{{ID: "model-a"}}}}})
	}))
}

func TestGatewayRoutingPrecedence(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := gatewayCatalogServer(t)
	defer server.Close()
	gatewayCfg := config.GatewayConfig{URL: server.URL, Token: "token", CatalogTTL: "1ms", ConnectTimeout: "1s", ResponseTimeout: "1s"}

	remoteCfg := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{}}
	provider, err := NewProviderByName(remoteCfg, "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); !ok {
		t.Fatalf("catalog provider routed to %T, want GatewayProvider", provider)
	}
	if got, _, err := ParseProviderModel("remote:model-a", remoteCfg); err != nil || got != "remote" {
		t.Fatalf("ParseProviderModel remote = %q, %v", got, err)
	}

	explicit := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{"remote": {Type: config.ProviderTypeZen, Model: "model-a"}}}
	provider, err = NewProviderByName(explicit, "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatalf("explicit local provider was routed remotely")
	}

	localList := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{"remote": {Type: config.ProviderTypeZen, Model: "model-a"}}}
	localList.Gateway.LocalProviders = []string{"remote"}
	provider, err = NewProviderByName(localList, "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatalf("gateway.local_providers did not win")
	}
}

func TestGatewayAdvertisedDebugRoutesRemoteUnlessExplicitlyLocal(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Catalog{Version: protocol.Version, Providers: []protocol.CatalogEntry{{Key: "debug", Type: string(config.ProviderTypeDebug), Models: []protocol.Model{{ID: "fast"}}}}})
	}))
	defer server.Close()
	gatewayCfg := config.GatewayConfig{URL: server.URL, Token: "token", CatalogTTL: "1ms", ConnectTimeout: "1s", ResponseTimeout: "1s"}

	remoteCfg := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{}}
	provider, err := NewProviderByName(remoteCfg, "debug", "fast")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); !ok {
		t.Fatalf("gateway-advertised debug routed to %T, want GatewayProvider", provider)
	}

	explicitCfg := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{"debug": {Model: "fast", Models: []string{"fast"}}}}
	provider, err = NewProviderByName(explicitCfg, "debug", "fast")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatal("explicitly configured local debug was routed remotely")
	}
	if provider.Name() != "debug:fast" {
		t.Fatalf("explicit local debug model = %q, want debug:fast", provider.Name())
	}

	listedCfg := &config.Config{Gateway: gatewayCfg, Providers: map[string]config.ProviderConfig{}}
	listedCfg.Gateway.LocalProviders = []string{"debug"}
	provider, err = NewProviderByName(listedCfg, "debug", "fast")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatal("gateway.local_providers debug exception was routed remotely")
	}
}

func TestGatewayFailsClosedByDefaultAndNoGatewayCompatibility(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: "http://127.0.0.1:1", Token: "token", ConnectTimeout: "10ms", ResponseTimeout: "10ms"}, Providers: map[string]config.ProviderConfig{}}
	if _, err := NewProviderByName(cfg, "remote", "model"); err == nil || !strings.Contains(err.Error(), "gateway unavailable") || !strings.Contains(err.Error(), "URL/network/token") {
		t.Fatalf("default gateway outage error = %v", err)
	}
	localOverride := &config.Config{
		Gateway:   config.GatewayConfig{URL: "http://127.0.0.1:1", Token: "token", LocalProviders: []string{"zen"}, ConnectTimeout: "10ms", ResponseTimeout: "10ms"},
		Providers: map[string]config.ProviderConfig{"zen": {Type: config.ProviderTypeZen, Model: "minimax-m2.5-free"}},
	}
	provider, err := NewProviderByName(localOverride, "zen", "minimax-m2.5-free")
	if err != nil {
		t.Fatalf("explicit local provider failed during gateway outage: %v", err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatal("gateway outage overrode explicit local provider")
	}
	local := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast"}}}
	provider, err = NewProvider(local)
	if err != nil {
		t.Fatalf("no-gateway behavior failed: %v", err)
	}
	if _, ok := provider.(*GatewayProvider); ok {
		t.Fatalf("no-gateway config unexpectedly routed remotely")
	}
	if provider.Name() != "debug:fast" {
		t.Fatalf("local debug model override = %q, want debug:fast", provider.Name())
	}
}
