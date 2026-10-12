package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/cache"
)

// The curated fallback must mirror the recorded live catalog so completion is
// correct before the first refresh.
func TestAnthropicStaticModelsMatchRecordedCatalog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(anthropicGoldenDir, "list_models", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var live []ModelInfo
	if err := json.Unmarshal(data, &live); err != nil {
		t.Fatal(err)
	}
	static := ProviderModels["anthropic"]
	if len(static) != len(live) {
		t.Fatalf("static list has %d models, recorded catalog has %d", len(static), len(live))
	}
	for i, model := range live {
		entry := static[i]
		if entry.ID != model.ID || entry.InputLimit != model.InputLimit || entry.OutputLimit != model.OutputLimit || !equalSlice(entry.ReasoningEfforts, model.ReasoningEfforts) {
			t.Errorf("static[%d] = %+v, recorded = {ID:%s InputLimit:%d OutputLimit:%d Efforts:%v}", i, entry, model.ID, model.InputLimit, model.OutputLimit, model.ReasoningEfforts)
		}
	}
}

func withAnthropicCache(t *testing.T, models []ModelInfo) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if models != nil {
		if err := cache.WriteModelInfoCache(anthropicModelCacheKey, modelInfosToCache(models)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnthropicLiveCatalogTakesPrecedence(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{
		{ID: "claude-next-1", InputLimit: 480_000, OutputLimit: 96_000, ReasoningEfforts: []string{"low", "high"}, ReasoningModes: []string{"adaptive"}},
		{ID: "claude-old-1-20240101", ReasoningModes: []string{"enabled"}},
	})

	if got := ProviderModelIDs("anthropic"); !equalSlice(got, []string{"claude-next-1", "claude-old-1-20240101"}) {
		t.Fatalf("ProviderModelIDs = %v, want cached catalog", got)
	}
	if got := ReasoningEffortsForProviderModel("anthropic", "claude-next-1"); !equalSlice(got, []string{"low", "high"}) {
		t.Fatalf("efforts = %v", got)
	}
	if base, effort := BaseModelAndEffortForProvider("anthropic", "claude-next-1-high"); base != "claude-next-1" || effort != "high" {
		t.Fatalf("BaseModelAndEffortForProvider = %q, %q", base, effort)
	}
	if got := InputLimitForProviderModel("anthropic", "claude-next-1"); got != 480_000 {
		t.Fatalf("input limit = %d", got)
	}
	if !isAdaptiveModel("claude-next-1") {
		t.Fatal("cached adaptive model treated as budget-only")
	}
	// Undated aliases resolve to their dated snapshot.
	if isAdaptiveModel("claude-old-1") {
		t.Fatal("cached budget-only model treated as adaptive")
	}
}

func TestAnthropicFallbackWithoutCache(t *testing.T) {
	withAnthropicCache(t, nil)
	if got := ProviderModelIDs("anthropic"); len(got) == 0 || got[0] != "claude-haiku-5-5" {
		t.Fatalf("fallback IDs = %v", got)
	}
	for model, want := range map[string]bool{
		"claude-opus-5": true, "claude-sonnet-4-6": true, "claude-opus-6": true,
		"claude-haiku-4-5": false, "claude-opus-4-5-20251101": false, "claude-sonnet-4": false,
	} {
		if got := isAdaptiveModel(model); got != want {
			t.Errorf("isAdaptiveModel(%q) = %v, want %v", model, got, want)
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
	infos := []ModelInfo{{ID: "claude-x-1-20250101", InputLimit: 1}, {ID: "claude-x-1-20251231", InputLimit: 2}, {ID: "claude-x-1-20250601", InputLimit: 3}}
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

func TestAnthropicOneMillionKeepsBudgetWithWarmCache(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{{ID: "claude-sonnet-4-5-20250929", InputLimit: 180_000, ReasoningModes: []string{"enabled"}}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-5"); got != 180_000 {
		t.Fatalf("base model limit = %d, want cached 180000", got)
	}
	for _, model := range []string{"claude-sonnet-4-5-1m", "claude-sonnet-4-5-20250929-1m"} {
		if got := InputLimitForProviderModel("anthropic", model); got != 980_000 {
			t.Errorf("InputLimitForProviderModel(%q) = %d, want 980000", model, got)
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
func TestAnthropicLiveCatalogOverridesCuratedRows(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{{ID: "claude-sonnet-4-6", InputLimit: 400_000, OutputLimit: 32_000, ReasoningEfforts: []string{"low", "xhigh"}, ReasoningModes: []string{"adaptive"}}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-6"); got != 400_000 {
		t.Errorf("input limit = %d, want cached 400000", got)
	}
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-6-xhigh"); got != 400_000 {
		t.Errorf("effort-suffixed input limit = %d, want cached 400000", got)
	}
	if got := ReasoningEffortsForProviderModel("anthropic", "claude-sonnet-4-6"); !equalSlice(got, []string{"low", "xhigh"}) {
		t.Errorf("efforts = %v, want cached", got)
	}
	if base, effort := BaseModelAndEffortForProvider("anthropic", "claude-sonnet-4-6-xhigh"); base != "claude-sonnet-4-6" || effort != "xhigh" {
		t.Errorf("BaseModelAndEffortForProvider = %q, %q", base, effort)
	}
	if got := anthropicClampOutputTokens(100_000, "claude-sonnet-4-6"); got != 32_000 {
		t.Errorf("output clamp = %d, want cached 32000", got)
	}
}

func TestAnthropicCachedNoThinkingIsNotAdaptive(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{{ID: "claude-next-plain", ReasoningModes: nil}})
	if isAdaptiveModel("claude-next-plain") {
		t.Fatal("catalog without thinking support treated as adaptive")
	}
}

func TestAnthropicOneMillionWithEffortSuffix(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{{ID: "claude-sonnet-4-5-20250929", InputLimit: 180_000}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-5-1m-high"); got != 980_000 {
		t.Fatalf("-1m with effort suffix = %d, want 980000", got)
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

func TestAnthropicCachedIDsOnlyForBuiltInProvider(t *testing.T) {
	withAnthropicCache(t, []ModelInfo{{ID: "claude-live-only"}})
	RegisterProviderAliases(map[string]string{"my-gateway": "anthropic"})
	defer RegisterProviderAliases(nil)
	if got := ProviderModelIDs("anthropic"); !equalSlice(got, []string{"claude-live-only"}) {
		t.Fatalf("built-in anthropic IDs = %v", got)
	}
	if got := ProviderModelIDs("my-gateway"); len(got) > 0 && got[0] == "claude-live-only" {
		t.Fatalf("gateway completion used the public catalog: %v", got)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.com")
	if got := ProviderModelIDs("anthropic"); len(got) > 0 && got[0] == "claude-live-only" {
		t.Fatalf("ANTHROPIC_BASE_URL gateway used the public catalog: %v", got)
	}
}

// Suffixed request models (the engine passes the configured string) are
// decoded instead of being sent to the API verbatim.
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
		if got := p.requestSpec(Request{Model: requested}); got != want {
			t.Errorf("requestSpec(%q) = %+v, want %+v", requested, got, want)
		}
	}
	// Budget thinking only adds effort "max" where the model accepts it.
	if anthropicSupportsEffort("claude-haiku-4-5", "max") || anthropicSupportsEffort("claude-opus-4-5", "max") {
		t.Error("budget-only models must not receive effort max")
	}
}
