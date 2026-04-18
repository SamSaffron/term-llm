package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
)

func TestGetCachedOpenRouterModelsStartsSingleBackgroundRefreshForStaleCache(t *testing.T) {
	origClient := defaultHTTPClient
	openRouterCacheRefreshInFlight.Store(false)
	defer func() {
		defaultHTTPClient = origClient
		openRouterCacheRefreshInFlight.Store(false)
	}()

	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	cacheDir := filepath.Join(cacheHome, "term-llm")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	staleCache := cache.ModelCache{
		Models:    []string{"stale-model"},
		FetchedAt: time.Now().Add(-cache.ModelCacheTTL - time.Minute),
	}
	cacheData, err := json.Marshal(staleCache)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	cachePath := filepath.Join(cacheDir, openRouterCacheKey+"-models.json")
	if err := os.WriteFile(cachePath, cacheData, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var requests int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		atomic.AddInt32(&requests, 1)
		select {
		case started <- struct{}{}:
		default:
		}

		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"fresh-model"}]}`)
	}))
	defer ts.Close()

	serverURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			urlCopy := *req.URL
			cloned.URL = &urlCopy
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			return ts.Client().Transport.RoundTrip(cloned)
		}),
	}

	const callers = 25
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := GetCachedOpenRouterModels("test-key")
			if len(got) != 1 || got[0] != "stale-model" {
				t.Errorf("GetCachedOpenRouterModels returned %v, want stale cache", got)
			}
		}()
	}
	wg.Wait()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background refresh request")
	}

	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("background refresh requests = %d, want 1", got)
	}

	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !openRouterCacheRefreshInFlight.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("background refresh guard remained in-flight after refresh completed")
}
