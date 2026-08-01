package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

func resetGatewayCatalogProcessCacheForTest() {
	gatewayCatalogProcessCache.Lock()
	gatewayCatalogProcessCache.entries = make(map[string]gatewayCatalogCache)
	gatewayCatalogProcessCache.Unlock()
}

func TestGatewayCatalogCacheETagAndStaleOnError(t *testing.T) {
	resetGatewayCatalogProcessCacheForTest()
	t.Cleanup(resetGatewayCatalogProcessCacheForTest)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requests := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") == `"catalog-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"catalog-v1"`)
		_ = json.NewEncoder(w).Encode(protocol.Catalog{Version: 1, GeneratedAt: time.Now(), Providers: []protocol.CatalogEntry{{Key: "remote", Models: []protocol.Model{{ID: "m"}}}}})
	}))
	defer server.Close()
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: server.URL, Token: "token", CatalogTTL: "1ms", ConnectTimeout: "1s", ResponseTimeout: "1s"}}
	client, err := NewGatewayProviderForCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := client.loadCatalog(t.Context())
	if err != nil || len(catalog.Providers) != 1 {
		t.Fatalf("first catalog = %+v, %v", catalog, err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := client.loadCatalog(t.Context()); err != nil {
		t.Fatalf("ETag refresh: %v", err)
	}
	if requests < 2 {
		t.Fatalf("requests = %d, want ETag revalidation", requests)
	}
	cachePath := gatewayCatalogCachePath(server.URL, "token")
	if cachePath == gatewayCatalogCachePath(server.URL, "other-client-token") {
		t.Fatal("gateway catalog cache is not client-scoped")
	}
	cache, err := readGatewayCatalogCache(cachePath)
	if err != nil || cache.ETag != `"catalog-v1"` {
		t.Fatalf("cache = %+v, %v", cache, err)
	}
	cache.FetchedAt = time.Now().Add(-time.Hour)
	if err := writeGatewayCatalogCache(cachePath, cache); err != nil {
		t.Fatal(err)
	}
	gatewayCatalogProcessCache.Lock()
	gatewayCatalogProcessCache.entries[gatewayCatalogIdentity(server.URL, "token")] = cache
	gatewayCatalogProcessCache.Unlock()
	fail = true
	stale, err := client.loadCatalog(t.Context())
	if err != nil || len(stale.Providers) != 1 || stale.Providers[0].Key != "remote" {
		t.Fatalf("stale fallback = %+v, %v", stale, err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayCatalogProcessMemoizationSingleflightAndCacheOnlyCompletion(t *testing.T) {
	resetGatewayCatalogProcessCacheForTest()
	t.Cleanup(resetGatewayCatalogProcessCacheForTest)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		_ = json.NewEncoder(w).Encode(protocol.Catalog{Version: 1, Providers: []protocol.CatalogEntry{{Key: "remote", Models: []protocol.Model{{ID: "model-a"}}}}})
	}))
	defer server.Close()
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: server.URL, Token: "token", CatalogTTL: "1m", ConnectTimeout: "1s", ResponseTimeout: "1s"}}
	provider, err := NewGatewayProviderForCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := provider.loadCatalog(t.Context())
			errs <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("catalog request did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent catalog requests = %d, want one", got)
	}
	close(release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	before := requests.Load()
	completions := GetProviderCompletions("remote:", false, cfg)
	if requests.Load() != before {
		t.Fatal("shell completion performed gateway network I/O")
	}
	if len(completions) != 1 || completions[0] != "remote:model-a" {
		t.Fatalf("cache-only completions = %v", completions)
	}
	providers := GetProviderCompletions("", false, cfg)
	for _, provider := range providers {
		if provider == "openai" || provider == "anthropic" {
			t.Fatalf("gateway completion advertised unconfigured built-in %q: %v", provider, providers)
		}
	}
}

func TestGatewayCompletionWithHungGatewayIsImmediateAndNetworkFree(t *testing.T) {
	resetGatewayCatalogProcessCacheForTest()
	t.Cleanup(resetGatewayCatalogProcessCacheForTest)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
		select {}
	}))
	defer server.Close()
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: server.URL, Token: "token", ConnectTimeout: "10s", ResponseTimeout: "30s"}}
	started := time.Now()
	_ = GetProviderCompletions("remote", false, cfg)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cache-only completion took %s", elapsed)
	}
	if requests.Load() != 0 {
		t.Fatalf("cache-only completion contacted hung gateway %d time(s)", requests.Load())
	}
}
