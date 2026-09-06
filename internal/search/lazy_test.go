package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestLazySearcherRetriesCredentialFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brave-key")
	cfg := &config.Config{}
	cfg.Search.Provider = "brave"
	cfg.Search.Brave.APIKey = "file://" + path
	searcher := NewLazySearcher(cfg)
	if err := Available(cfg); err != nil {
		t.Fatalf("availability resolved a secret: %v", err)
	}
	// An empty query avoids network access even if a regression falls back to DDG.
	if _, err := searcher.Search(context.Background(), "", 1); err == nil || !strings.Contains(err.Error(), "search.brave.api_key") {
		t.Fatalf("expected credential error instead of fallback, got %v", err)
	}
	if err := os.WriteFile(path, []byte("unlocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := searcher.(*lazySearcher).searcher()
	if err != nil {
		t.Fatal(err)
	}
	brave, ok := resolved.(*BraveSearcher)
	if !ok || brave.apiKey != "unlocked" {
		t.Fatalf("retry did not construct configured provider: %#v", resolved)
	}
}

func TestLazyExaMCPRetriesCredentialFailure(t *testing.T) {
	var calls atomic.Int32
	locked := errors.New("vault locked")
	client := NewLazyExaMCPClient(func() (string, string, error) {
		if calls.Add(1) == 1 {
			return "", "", locked
		}
		return "", "unlocked", nil
	})
	if _, err := client.FetchURL(context.Background(), "https://example.test"); !errors.Is(err, locked) {
		t.Fatalf("first fetch = %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.ensureResolved(); err != nil {
				t.Errorf("retry = %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 || client.apiKey != "unlocked" || client.url != defaultExaMCPURL {
		t.Fatalf("retry calls=%d key=%q url=%q", calls.Load(), client.apiKey, client.url)
	}
}

func TestExaMCPFallbackUsesResolvedEndpoint(t *testing.T) {
	t.Setenv("EXA_API_KEY", "hosted-key")
	for _, endpoint := range []string{config.DefaultSearchExaMCPURL, "https://custom.example.test/mcp"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("TERM_LLM_EXA_URL", endpoint)
			path := filepath.Join(t.TempDir(), "endpoint")
			if err := os.WriteFile(path, []byte(endpoint), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{endpoint, "${TERM_LLM_EXA_URL}", "file://" + path} {
				cfg := &config.Config{}
				cfg.Search.Provider = "exa_mcp"
				cfg.Search.ExaMCP.URL = raw
				searcher, err := NewSearcher(cfg)
				if err != nil {
					t.Fatal(err)
				}
				client := searcher.(*ExaMCPClient)
				wantKey := ""
				if endpoint == config.DefaultSearchExaMCPURL {
					wantKey = "hosted-key"
				}
				if client.url != endpoint || client.apiKey != wantKey {
					t.Fatalf("%s resolved url=%q key=%q", raw, client.url, client.apiKey)
				}
			}
		})
	}
}
