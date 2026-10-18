package anthropic

import (
	"context"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/cache"
)

// The curated fallback must mirror the recorded live catalog so completion is
// correct before the first refresh.
func withAnthropicCache(t *testing.T, models []core.ModelInfo) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if models != nil {
		if err := cache.WriteModelInfoCache(anthropicModelCacheKey, modelInfosToCache(models)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnthropicListModelsPaginatesAndCachesOnlyPublicAPI(t *testing.T) {
	withAnthropicCache(t, nil)
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","max_input_tokens":1000000,"max_tokens":128000,"capabilities":{"effort":{"supported":true,"low":{"supported":true},"max":{"supported":false}},"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}}}}],"has_more":true,"last_id":"claude-a"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-b","max_input_tokens":200000,"max_tokens":64000}],"has_more":false,"last_id":"claude-b"}`))
	}))
	defer ts.Close()

	p, err := NewAnthropicProviderWithOptions("test-key", "claude-a", AnthropicCredAPIKey, AnthropicProviderOptions{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].InputLimit != 980_000 || models[0].OutputLimit != 128_000 || !equalSlice(models[0].ReasoningEfforts, []string{"low"}) || !equalSlice(models[0].ReasoningModes, []string{"adaptive"}) || models[1].InputLimit != 180_000 {
		t.Fatalf("models = %+v", models)
	}
	if len(queries) != 2 || queries[0] != "limit=1000" || queries[1] != "limit=1000&after_id=claude-a" {
		t.Fatalf("queries = %v", queries)
	}
	// A custom base URL must not overwrite the shared public-API cache.
	if ids := GetCachedAnthropicModels(); len(ids) > 0 {
		t.Fatalf("custom endpoint populated public cache: %v", ids)
	}

	// The public API host does populate it.
	p.client.baseURL.Scheme, p.client.baseURL.Host = "https", "api.anthropic.com"
	if !p.client.usesPublicAPI() {
		t.Fatal("api.anthropic.com not treated as public API")
	}
}

func TestAnthropicAliasPrefersNewestSnapshot(t *testing.T) {
	infos := []core.ModelInfo{{ID: "claude-x-1-20250101", InputLimit: 1}, {ID: "claude-x-1-20251231", InputLimit: 2}, {ID: "claude-x-1-20250601", InputLimit: 3}}
	if got, ok := findAnthropicModelInfo(infos, "claude-x-1"); !ok || got.InputLimit != 2 {
		t.Fatalf("alias match = %+v, %v; want newest snapshot", got, ok)
	}
}

func TestBedrockQualifiedIDsKeepThinkingMode(t *testing.T) {
	withAnthropicCache(t, nil)
	for model, wantAdaptive := range map[string]bool{
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":                   false,
		"eu.anthropic.claude-sonnet-4-5-20250929-v1:0":                  false,
		"anthropic.claude-opus-4-5-20251101-v1:0":                       false,
		"us.anthropic.claude-opus-4-7-v1":                               true,
		"global.anthropic.claude-sonnet-4-6":                            true,
		"arn:aws:bedrock:us-west-2:1:application-inference-profile/abc": false,
		"claude-sonnet-4-5-20250929-1m":                                 false,
		"claude-haiku-4-5-20251001-1m":                                  false,
		"claude-sonnet-4-6-1m":                                          true,
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0-1m":               false,
	} {
		_, budget, adaptive := parseModelThinking(model + "-thinking")
		if adaptive != wantAdaptive || (budget > 0) == wantAdaptive {
			t.Errorf("parseModelThinking(%q-thinking) = budget %d adaptive %v, want adaptive %v", model, budget, adaptive, wantAdaptive)
		}
	}
}

func TestAnthropicStreamErrorStatusIgnoresExceptionCase(t *testing.T) {
	for _, errType := range []string{"throttlingException", "ThrottlingException", "InternalServerException", "overloaded_error", "rate_limit_error"} {
		if !isRetryable(newAnthropicStreamError(errType, "x")) {
			t.Errorf("%s should be retryable", errType)
		}
	}
	if isRetryable(newAnthropicStreamError("ValidationException", "bad input")) {
		t.Error("ValidationException should not be retryable")
	}
}

func TestAnthropicPublicAPIRequiresRootPath(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://api.anthropic.com":          true,
		"https://api.anthropic.com:443/":     true,
		"https://api.anthropic.com/gateway/": false,
		"http://api.anthropic.com":           false,
		"https://api.anthropic.com:8443":     false,
		"https://user:pw@api.anthropic.com":  false,
		"https://gateway.example.com":        false,
	} {
		c, err := newAnthropicClient("k", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.usesPublicAPI(); got != want {
			t.Errorf("usesPublicAPI(%s) = %v, want %v", raw, got, want)
		}
	}
}

// Curated rows mirror the API but must not shadow a newer live catalog.
func TestAnthropicCachedNoThinkingIsNotAdaptive(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{{ID: "claude-next-plain", ReasoningModes: nil}})
	if isAdaptiveModel("claude-next-plain") {
		t.Fatal("catalog without thinking support treated as adaptive")
	}
}

func TestAnthropicModelListRejectsHasMoreWithoutCursor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-a"}],"has_more":true,"last_id":""}`))
	}))
	defer ts.Close()
	c, err := newAnthropicClient("k", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.listModels(context.Background()); err == nil {
		t.Fatal("partial catalog accepted")
	}
}

func TestAnthropicModelListRejectsEndlessPagination(t *testing.T) {
	page := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		fmt.Fprintf(w, `{"data":[{"id":"claude-%d"}],"has_more":true,"last_id":"claude-%d"}`, page, page)
	}))
	defer ts.Close()
	c, err := newAnthropicClient("k", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.listModels(context.Background()); err == nil {
		t.Fatal("truncated catalog accepted after the page cap")
	}
}

func TestAnthropicRequestModelSuffixesAreDecoded(t *testing.T) {
	withAnthropicCache(t, nil)
	p := &AnthropicProvider{model: "claude-sonnet-4-6"}
	for requested, want := range map[string]anthropicModelSpec{
		"claude-sonnet-4-6-thinking":      {model: "claude-sonnet-4-6", adaptive: true},
		"claude-sonnet-4-6-1m":            {model: "claude-sonnet-4-6", use1m: true},
		"claude-haiku-4-5-thinking":       {model: "claude-haiku-4-5", thinkingBudget: 10000},
		"claude-opus-4-7-1m-thinking":     {model: "claude-opus-4-7", adaptive: true, use1m: true},
		"claude-sonnet-4-6-thinking-high": {model: "claude-sonnet-4-6", adaptive: true, effort: "high"},
	} {
		if got := p.requestSpec(core.Request{Model: requested}); got != want {
			t.Errorf("requestSpec(%q) = %+v, want %+v", requested, got, want)
		}
	}
	// Budget thinking only adds effort "max" where the model accepts it.
	if anthropicSupportsEffort("claude-haiku-4-5", "max") || anthropicSupportsEffort("claude-opus-4-5", "max") {
		t.Error("budget-only models must not receive effort max")
	}
}
