package llm

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/venice"
)

// from venice_test.go: TestNewProviderByName_ResolvesConfiguredVeniceCredentialsOnDemand
func TestNewProviderByName_ResolvesConfiguredVeniceCredentialsOnDemand(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"venice": {
				APIKey: "  test-key\n",
				Model:  "venice-uncensored",
			},
		},
	}

	provider, err := NewProviderByName(cfg, "venice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}
	if got := strings.TrimSpace(cfg.Providers["venice"].ResolvedAPIKey); got != "test-key" {
		t.Fatalf("ResolvedAPIKey = %q, want %q", got, "test-key")
	}
}

// from venice_test.go: TestCreateProviderFromConfig_VeniceRequiresAPIKey
func TestCreateProviderFromConfig_VeniceRequiresAPIKey(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	_, err := createProviderFromConfig("venice", &config.ProviderConfig{Model: "venice-uncensored"})
	if err == nil {
		t.Fatal("expected missing Venice API key to return an error")
	}
	if !strings.Contains(err.Error(), "VENICE_API_KEY") {
		t.Fatalf("expected VENICE_API_KEY guidance, got %v", err)
	}
}

// from venice_test.go: TestCreateProviderFromConfig_VeniceTrimsConfiguredAPIKey
func TestCreateProviderFromConfig_VeniceTrimsConfiguredAPIKey(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	provider, err := createProviderFromConfig("venice", &config.ProviderConfig{
		ResolvedAPIKey: "  test-key\n",
		Model:          "venice-uncensored",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	venice, ok := provider.(*VeniceProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *VeniceProvider", provider)
	}
	if venice.APIKey() != "test-key" {
		t.Fatalf("apiKey = %q, want %q", venice.APIKey(), "test-key")
	}
}

// from venice_test.go: TestVeniceProvider429IsRetried
func TestVeniceProvider429IsRetried(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"The model is currently overloaded. Please try again later."}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	inner := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	provider := WrapWithRetry(inner, RetryConfig{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond})

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var text string
	for {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventError {
			t.Fatalf("unexpected EventError after retry: %v", ev.Err)
		}
		if ev.Type == EventDone {
			break
		}
		if ev.Type == EventTextDelta {
			text += ev.Text
		}
	}

	if callCount != 3 {
		t.Fatalf("callCount = %d, want 3 (2 × 429 then success)", callCount)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want %q", text, "ok")
	}
}

// from nearai_test.go: TestNewProviderByName_ResolvesConfiguredNearAICredentialsOnDemand
func TestNewProviderByName_ResolvesConfiguredNearAICredentialsOnDemand(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"nearai": {
				APIKey: "  test-key\n",
				Model:  "zai-org/GLM-5.1-FP8",
			},
		},
	}

	provider, err := NewProviderByName(cfg, "nearai", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}
	if got := strings.TrimSpace(cfg.Providers["nearai"].ResolvedAPIKey); got != "test-key" {
		t.Fatalf("ResolvedAPIKey = %q, want %q", got, "test-key")
	}
}

// from nearai_test.go: TestCreateProviderFromConfig_NearAIRequiresAPIKey
func TestCreateProviderFromConfig_NearAIRequiresAPIKey(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	_, err := createProviderFromConfig("nearai", &config.ProviderConfig{Model: "zai-org/GLM-5.1-FP8"})
	if err == nil {
		t.Fatal("expected missing NEAR AI API key to return an error")
	}
	if !strings.Contains(err.Error(), "NEARAI_API_KEY") {
		t.Fatalf("expected NEARAI_API_KEY guidance, got %v", err)
	}
}

// from nearai_test.go: TestCreateProviderFromConfig_NearAITrimsConfiguredAPIKey
func TestCreateProviderFromConfig_NearAITrimsConfiguredAPIKey(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	provider, err := createProviderFromConfig("nearai", &config.ProviderConfig{
		ResolvedAPIKey: "  test-key\n",
		Model:          "zai-org/GLM-5.1-FP8",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nearai, ok := provider.(*NearAIProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *NearAIProvider", provider)
	}
	if nearai.APIKey() != "test-key" {
		t.Fatalf("apiKey = %q, want %q", nearai.APIKey(), "test-key")
	}
}

// from openai_compat_test.go: TestProviderHTTPClients_UseHTTP2WithLargerIdlePool
func TestProviderHTTPClients_UseHTTP2WithLargerIdlePool(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	serverTransport := server.Client().Transport.(*http.Transport)
	clients := map[string]*http.Client{
		"default": defaultHTTPClient,
	}
	for name, providerClient := range clients {
		t.Run(name, func(t *testing.T) {
			providerTransport, ok := providerClient.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", providerClient.Transport)
			}
			if !providerTransport.ForceAttemptHTTP2 {
				t.Fatal("HTTP/2 is not enabled")
			}
			if providerTransport.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
				t.Fatalf("MaxIdleConnsPerHost = %d, want more than default %d", providerTransport.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
			}

			transport := providerTransport.Clone()
			transport.TLSClientConfig = serverTransport.TLSClientConfig.Clone()
			defer transport.CloseIdleConnections()

			response, err := (&http.Client{Transport: transport}).Get(server.URL)
			if err != nil {
				t.Fatalf("HTTP/2 request: %v", err)
			}
			defer response.Body.Close()
			if response.ProtoMajor != 2 {
				t.Fatalf("response protocol = %s, want HTTP/2", response.Proto)
			}
		})
	}
}

// from openai_compat_test.go: TestNormalizeSchemaForOpenAI_FreeFormMapProperty
func TestNormalizeSchemaForOpenAI_FreeFormMapProperty(t *testing.T) {
	// Regression: env parameter uses additionalProperties: {type: string} to represent
	// a free-form string map. The normalizer must not clobber it with false.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"env": map[string]interface{}{
				"type":                 "object",
				"description":          "Environment variables",
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}

	result := normalizeSchemaForOpenAI(schema)

	props, ok := result["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected properties map")
	}

	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be a map")
	}

	// additionalProperties on env must remain a schema map, not false
	ap := envSchema["additionalProperties"]
	apMap, ok := ap.(map[string]interface{})
	if !ok {
		t.Fatalf("expected env.additionalProperties to remain a schema map, got %T (%v)", ap, ap)
	}
	if apMap["type"] != "string" {
		t.Errorf("expected env.additionalProperties.type = string, got %v", apMap["type"])
	}

	// Outer object must still have additionalProperties: false
	if result["additionalProperties"] != false {
		t.Errorf("expected outer additionalProperties = false, got %v", result["additionalProperties"])
	}

	// All properties must appear in required (OpenAI strict mode)
	required, ok := result["required"].([]string)
	if !ok {
		t.Fatal("expected required to be []string")
	}
	requiredSet := make(map[string]bool)
	for _, k := range required {
		requiredSet[k] = true
	}
	for k := range props {
		if !requiredSet[k] {
			t.Errorf("expected %q in required array, not found", k)
		}
	}
}

// from openai_compat_test.go: TestNormalizeSchemaForOpenAI_RegularObjectGetsAdditionalPropertiesFalse
func TestNormalizeSchemaForOpenAI_RegularObjectGetsAdditionalPropertiesFalse(t *testing.T) {
	// Regular nested object (no additionalProperties set) should get additionalProperties: false
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"options": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"verbose": map[string]interface{}{"type": "boolean"},
				},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)

	props := result["properties"].(map[string]interface{})
	optionsSchema := props["options"].(map[string]interface{})

	if optionsSchema["additionalProperties"] != false {
		t.Errorf("expected nested object to get additionalProperties: false, got %v", optionsSchema["additionalProperties"])
	}
}

// from openai_compat_test.go: TestNormalizeSchemaForOpenAI_MCPTypeArrayBecomesAnyOf
func TestNormalizeSchemaForOpenAI_MCPTypeArrayBecomesAnyOf(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type":        []interface{}{"array", "null"},
				"description": "Dropped data payload",
				"items":       map[string]interface{}{},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)
	props := result["properties"].(map[string]interface{})
	dataSchema := props["data"].(map[string]interface{})

	if _, ok := dataSchema["type"]; ok {
		t.Fatalf("expected data.type to be removed in favor of anyOf, got %#v", dataSchema["type"])
	}
	anyOf, ok := dataSchema["anyOf"].([]interface{})
	if !ok || len(anyOf) != 2 {
		t.Fatalf("expected data.anyOf with 2 branches, got %#v", dataSchema["anyOf"])
	}
	arrayBranch, ok := anyOf[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first anyOf branch to be object, got %T", anyOf[0])
	}
	if arrayBranch["type"] != "array" {
		t.Fatalf("expected first branch type array, got %v", arrayBranch["type"])
	}
	items, ok := arrayBranch["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected array branch to preserve and normalize items, got %#v", arrayBranch["items"])
	}
	if items["type"] != "string" {
		t.Fatalf("expected empty items schema to be normalized to string, got %#v", items["type"])
	}
	nullBranch, ok := anyOf[1].(map[string]interface{})
	if !ok || nullBranch["type"] != "null" {
		t.Fatalf("expected second anyOf branch to be null, got %#v", anyOf[1])
	}
}

// from openai_compat_test.go: TestNormalizeSchemaForOpenAI_InfersInvalidMCPPropertyType
func TestNormalizeSchemaForOpenAI_InfersInvalidMCPPropertyType(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type": []interface{}{},
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"fallback": map[string]interface{}{
				"type": map[string]interface{}{"not": "a valid type"},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)
	props := result["properties"].(map[string]interface{})
	dataSchema := props["data"].(map[string]interface{})
	if dataSchema["type"] != "array" {
		t.Fatalf("expected data type to be inferred as array, got %#v", dataSchema["type"])
	}
	fallbackSchema := props["fallback"].(map[string]interface{})
	if fallbackSchema["type"] != "string" {
		t.Fatalf("expected uninformative invalid type to fall back to string, got %#v", fallbackSchema["type"])
	}
}

// from openai_compat_test.go: TestBuildResponsesTools_DefaultsEmptyParametersToObject
func TestBuildResponsesTools_DefaultsEmptyParametersToObject(t *testing.T) {
	tools := BuildResponsesTools([]ToolSpec{{Name: "empty", Schema: map[string]interface{}{}}})
	tool := tools[0].(ResponsesTool)
	if tool.Parameters["type"] != "object" {
		t.Fatalf("expected empty tool schema to default to object parameters, got %#v", tool.Parameters)
	}
	props, ok := tool.Parameters["properties"].(map[string]interface{})
	if !ok || len(props) != 0 {
		t.Fatalf("expected empty properties map, got %#v", tool.Parameters["properties"])
	}
}

// from openai_compat_test.go: TestBuildResponsesTools_NormalizesFreeFormMapProperty
func TestBuildResponsesTools_NormalizesFreeFormMapProperty(t *testing.T) {
	specs := []ToolSpec{{
		Name:   "shell",
		Strict: true,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string"},
				"env": map[string]interface{}{
					"type":                 "object",
					"description":          "Environment variables",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}}
	tools := BuildResponsesTools(specs)
	tool := tools[0].(ResponsesTool)
	props := tool.Parameters["properties"].(map[string]interface{})

	// command must remain unchanged
	if _, ok := props["command"]; !ok {
		t.Error("expected command to remain in strict schema")
	}

	// env must be present but transformed to an array of key/value objects
	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be present and a map")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env to be transformed to array type, got %v", envSchema["type"])
	}
	if envSchema["description"] != "Environment variables" {
		t.Errorf("expected description to be preserved, got %v", envSchema["description"])
	}
	items, ok := envSchema["items"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items to be a map")
	}
	if items["type"] != "object" {
		t.Errorf("expected env.items.type to be object, got %v", items["type"])
	}
	itemProps, ok := items["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties to be a map")
	}
	if _, ok := itemProps["key"]; !ok {
		t.Error("expected env.items.properties.key to exist")
	}
	valueSchema, ok := itemProps["value"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties.value to be a map")
	}
	if valueSchema["type"] != "string" {
		t.Errorf("expected env.items.properties.value.type to be string (original additionalProperties schema), got %v", valueSchema["type"])
	}
	if items["additionalProperties"] != false {
		t.Errorf("expected env.items.additionalProperties to be false, got %v", items["additionalProperties"])
	}
}

// from openai_compat_test.go: TestNormalizeFreeFormMapProperties_PreservesNonStringValueType
func TestNormalizeFreeFormMapProperties_PreservesNonStringValueType(t *testing.T) {
	// A free-form map whose values are integers, not strings.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"counts": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "integer"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	countsSchema := props["counts"].(map[string]interface{})
	if countsSchema["type"] != "array" {
		t.Fatalf("expected counts to be transformed to array, got %v", countsSchema["type"])
	}
	items := countsSchema["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	valueSchema := itemProps["value"].(map[string]interface{})
	if valueSchema["type"] != "integer" {
		t.Errorf("expected value type to preserve original additionalProperties type 'integer', got %v", valueSchema["type"])
	}
}

// from openai_compat_test.go: TestNormalizeFreeFormMapProperties_TraversesItems
func TestNormalizeFreeFormMapProperties_TraversesItems(t *testing.T) {
	// A free-form map nested inside an array's items schema.
	schema := map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	items := result["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	envSchema, ok := itemProps["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to exist inside items.properties")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env inside items to be transformed to array, got %v", envSchema["type"])
	}
}

// from openai_compat_test.go: TestNormalizeFreeFormMapProperties_AnyOfFreeFormMap
func TestNormalizeFreeFormMapProperties_AnyOfFreeFormMap(t *testing.T) {
	// A property whose schema is an anyOf where one branch is a free-form map.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"meta": map[string]interface{}{
				"anyOf": []interface{}{
					map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					map[string]interface{}{"type": "null"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	metaSchema := props["meta"].(map[string]interface{})
	anyOf := metaSchema["anyOf"].([]interface{})

	// First anyOf branch (was a free-form map) must be converted to array.
	firstBranch, ok := anyOf[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected first anyOf branch to be a map")
	}
	if firstBranch["type"] != "array" {
		t.Errorf("expected free-form map in anyOf to be converted to array, got %v", firstBranch["type"])
	}

	// Second anyOf branch (null) must be unchanged.
	secondBranch, ok := anyOf[1].(map[string]interface{})
	if !ok {
		t.Fatal("expected second anyOf branch to be a map")
	}
	if secondBranch["type"] != "null" {
		t.Errorf("expected null branch to be unchanged, got %v", secondBranch["type"])
	}
}

// from openai_compat_test.go: TestNormalizeFreeFormMapProperties_PreservesMetadata
func TestNormalizeFreeFormMapProperties_PreservesMetadata(t *testing.T) {
	// Metadata fields beyond description (title, default) must survive conversion.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tags": map[string]interface{}{
				"type":                 "object",
				"description":          "Tag values",
				"title":                "Tags",
				"default":              map[string]interface{}{},
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	tagsSchema := props["tags"].(map[string]interface{})
	if tagsSchema["type"] != "array" {
		t.Fatalf("expected tags to be converted to array, got %v", tagsSchema["type"])
	}
	if tagsSchema["description"] != "Tag values" {
		t.Errorf("expected description to be preserved, got %v", tagsSchema["description"])
	}
	if tagsSchema["title"] != "Tags" {
		t.Errorf("expected title to be preserved, got %v", tagsSchema["title"])
	}
	if tagsSchema["default"] == nil {
		t.Error("expected default to be preserved")
	}
}

// from openrouter_defaults_test.go: TestOpenRouterDefaultRequiresKey
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
				func() (Provider, error) {
					p, err := NewProvider(cfg)
					if retry, ok := p.(*RetryProvider); ok {
						return retry.inner, err
					}
					return p, err
				},
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
				if !ok || router.Model() != "openrouter/free" || router.APIKey() != key {
					t.Fatalf("configured=%v: wrong OpenRouter defaults", configured)
				}
			}
		}
	}
}

// from sambanova_test.go: TestNewProviderByName_ResolvesConfiguredSambaNovaCredentialsOnDemand
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

// from sambanova_test.go: TestCreateProviderFromConfig_SambaNovaRequiresAPIKey
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

// from sambanova_test.go: TestCreateProviderFromConfig_SambaNovaTrimsConfiguredAPIKey
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
	if sn.APIKey() != "test-key" {
		t.Fatalf("apiKey = %q, want %q", sn.APIKey(), "test-key")
	}
}

// from venice_image_models_test.go: TestVeniceImageModelDiscovery
func TestVeniceImageModelDiscovery(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("VENICE_API_KEY", "")
	old := defaultHTTPClient
	t.Cleanup(func() { defaultHTTPClient = old })
	calls := 0
	defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != veniceBaseURL+"/models?type=all" || r.Header.Get("Authorization") != "Bearer image-key" {
			t.Errorf("unexpected request: %s, auth %q", r.URL, r.Header.Get("Authorization"))
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 2*time.Second {
			t.Error("completion request lacks short deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"new-image","type":"image","model_spec":{"constraints":{"resolutions":["1K","2K"]}}},{"id":"new-edit","type":"inpaint","model_spec":{"constraints":{"aspectRatios":["1:1"]}}},{"id":"chat","type":"text"},{"id":"video","type":"video"},{"id":"new-image","type":"image"},{"id":" ","type":"image"}]}`))}, nil
	})}
	cfg := &config.Config{}
	cfg.Image.Venice.APIKey = " Bearer image-key "
	if got := GetImageModelIDs("venice", cfg); !reflect.DeepEqual(got, []string{"new-edit", "new-image"}) {
		t.Fatalf("models = %v", got)
	}
	if got := GetProviderCompletions("venice:new", true, cfg); !reflect.DeepEqual(got, []string{"venice:new-edit", "venice:new-image"}) {
		t.Fatalf("completions = %v", got)
	}
	for kind, want := range map[string][]string{"image": {"new-image"}, "inpaint": {"new-edit"}} {
		if got := GetVeniceImageModelIDs(cfg, kind); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s models = %v, want %v", kind, got, want)
		}
	}
	cfg.Image.Venice.Model = "custom"
	if got := GetVeniceImageModelIDs(cfg, "image"); !reflect.DeepEqual(got, []string{"custom", "new-image"}) {
		t.Fatalf("configured model missing: %v", got)
	}
	if calls != 1 {
		t.Fatalf("fresh cache fetched again: %d calls", calls)
	}
	stored, err := cache.ReadModelCache(veniceImageCacheKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range stored.ModelInfos {
		if model.ImageConstraints == nil {
			t.Fatalf("discovery lost constraints for %s", model.ID)
		}
		if model.ID == "new-image" && len(model.ImageConstraints.Resolutions) != 2 {
			t.Fatalf("lost resolution tiers: %+v", model)
		}
		if model.ID == "new-edit" && len(model.ImageConstraints.Resolutions) != 0 {
			t.Fatalf("native edit model gained tiers: %+v", model)
		}
	}

	if _, err := cache.ReadModelCache(veniceCacheKey); !os.IsNotExist(err) {
		t.Fatalf("image discovery touched text cache: %v", err)
	}
}

// from venice_image_models_test.go: TestVeniceImageModelStaleRefresh
func TestVeniceImageModelStaleRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		err              error
	}{
		{name: "success", body: `{"data":[{"id":"fresh","type":"image"}]}`, status: 200, want: "fresh"},
		{name: "empty", body: `{"data":[]}`, status: 200, want: "stale"},
		{name: "invalid_json", body: `invalid`, status: 200, want: "stale"},
		{name: "unauthorized", status: 401, want: "stale"},
		{name: "server_error", status: 500, want: "stale"},
		{name: "transport_error", err: errors.New("offline"), want: "stale"},
		{name: "timeout", err: context.DeadlineExceeded, want: "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", home)
			t.Setenv("VENICE_API_KEY", "key")
			dir := filepath.Join(home, "term-llm")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, veniceImageCacheKey+"-models.json")
			if err := os.WriteFile(path, []byte(`{"models":["stale"],"model_infos":[{"id":"stale","type":"image"}],"fetched_at":"2000-01-01T00:00:00Z"}`), 0600); err != nil {
				t.Fatal(err)
			}
			old := defaultHTTPClient
			t.Cleanup(func() { defaultHTTPClient = old })
			calls := 0
			defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			// No background waits: the cache must be written before completion returns
			// so it survives the completion process exiting immediately afterwards.
			if got := GetImageModelIDs("venice", nil); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("models = %v, want %s", got, tc.want)
			}
			if calls != 1 {
				t.Fatalf("refresh requests = %d", calls)
			}
			stored, err := cache.ReadModelCache(veniceImageCacheKey)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Models, []string{tc.want}) {
				t.Fatalf("cached models = %v, want %s", stored.Models, tc.want)
			}
		})
	}
}

// from venice_image_models_test.go: TestVeniceImageModelConfiguredFallback
func TestVeniceImageModelConfiguredFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("VENICE_API_KEY", "")
	cfg := &config.Config{}
	cfg.Image.Venice.Model = "custom"
	cfg.Image.Venice.EditModel = "custom-edit"
	if got := GetImageModelIDs("venice", cfg); !reflect.DeepEqual(got, []string{"custom", "custom-edit"}) {
		t.Fatalf("fallback = %v", got)
	}
	if got := GetImageModelIDs("venice", nil); len(got) != 0 {
		t.Fatalf("unexpected hardcoded models: %v", got)
	}
}

// from nearai_test.go: TestNearAIProviderListModelsUsesCatalogEndpointAndFiltersNonChatModels
func TestNearAIProviderListModelsUsesCatalogEndpointAndFiltersNonChatModels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/list" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want Bearer test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{
					"modelId": "zai-org/GLM-5.1-FP8",
					"inputCostPerToken": {"amount": 850, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 3300, "scale": 9, "currency": "USD"},
					"metadata": {
						"contextLength": 202752,
						"modelDisplayName": "GLM 5.1",
						"ownedBy": "nearai",
						"architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}
					}
				},
				{
					"modelId": "openai/gpt-oss-120b",
					"inputCostPerToken": {"amount": 150, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 550, "scale": 9, "currency": "USD"},
					"metadata": {"contextLength": 131000, "modelDisplayName": "GPT OSS 120B", "ownedBy": "nearai"}
				},
				{
					"modelId": "Qwen/Qwen3.6-35B-A3B-FP8",
					"metadata": {"contextLength": 262144, "architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}}
				},
				{
					"modelId": "unknown/text-model",
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}}
				},
				{
					"modelId": "black-forest-labs/FLUX.2-klein-4B",
					"inputCostPerToken": {"amount": 1000, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 1000, "scale": 9, "currency": "USD"},
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["image"]}}
				},
				{
					"modelId": "Qwen/Qwen3-Embedding-0.6B",
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["embedding"]}}
				},
				{
					"modelId": "openai/whisper-large-v3",
					"metadata": {"architecture": {"inputModalities": ["audio"], "outputModalities": ["text"]}}
				},
				{"modelId": "Qwen/Qwen3-Reranker-0.6B"},
				{"modelId": "openai/privacy-filter"}
			]
		}`))
	}))
	defer ts.Close()

	provider := &NearAIProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "zai-org/GLM-5.1-FP8", "NEAR AI")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("ListModels() returned %d models, want 4: %#v", len(models), models)
	}
	if models[0].ID != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("first model = %q, want zai-org/GLM-5.1-FP8", models[0].ID)
	}
	if models[0].DisplayName != "GLM 5.1" || models[0].OwnedBy != "nearai" {
		t.Fatalf("first model metadata = %#v", models[0])
	}
	if models[0].InputLimit != 202_752 {
		t.Fatalf("GLM InputLimit = %d, want 202752", models[0].InputLimit)
	}
	if !nearPriceEqual(models[0].InputPrice, 0.85) || !nearPriceEqual(models[0].OutputPrice, 3.30) {
		t.Fatalf("GLM pricing = %g/%g, want 0.85/3.30", models[0].InputPrice, models[0].OutputPrice)
	}
	if models[1].ID != "openai/gpt-oss-120b" {
		t.Fatalf("second model = %q, want openai/gpt-oss-120b", models[1].ID)
	}
	if !nearPriceEqual(models[1].InputPrice, 0.15) || !nearPriceEqual(models[1].OutputPrice, 0.55) {
		t.Fatalf("gpt-oss pricing = %g/%g, want 0.15/0.55", models[1].InputPrice, models[1].OutputPrice)
	}
	if models[2].ID != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Fatalf("third model = %q, want Qwen/Qwen3.6-35B-A3B-FP8", models[2].ID)
	}
	if !nearPriceEqual(models[2].InputPrice, 0.17) || !nearPriceEqual(models[2].OutputPrice, 1.10) {
		t.Fatalf("curated fallback pricing = %g/%g, want 0.17/1.10", models[2].InputPrice, models[2].OutputPrice)
	}
	if models[3].ID != "unknown/text-model" {
		t.Fatalf("fourth model = %q, want unknown/text-model", models[3].ID)
	}
	if models[3].InputPrice != -1 || models[3].OutputPrice != -1 {
		t.Fatalf("missing pricing = %g/%g, want -1/-1", models[3].InputPrice, models[3].OutputPrice)
	}
}

// from openrouter_models_test.go: TestOpenRouterCachedLimitFeedsProviderModelLookup
func TestOpenRouterCachedLimitFeedsProviderModelLookup(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	RefreshOpenRouterCacheSync("", []ModelInfo{{ID: "minimax/minimax-m3", InputLimit: 1048576}})

	if got := InputLimitForProviderModel("openrouter", "minimax/minimax-m3"); got != 1048576 {
		t.Fatalf("InputLimitForProviderModel(openrouter, minimax/minimax-m3) = %d, want 1048576", got)
	}
}

// from sambanova_models_test.go: TestPricingForProviderModelSambaNova
func TestPricingForProviderModelSambaNova(t *testing.T) {
	input, output, ok := PricingForProviderModel("sambanova", "gpt-oss-120b")
	if !ok {
		t.Fatal("expected SambaNova pricing")
	}
	if input != 0.22 || output != 0.59 {
		t.Fatalf("pricing = %g/%g, want 0.22/0.59", input, output)
	}
}

// from sambanova_models_test.go: TestSambaNovaCuratedModelsUseCurrentLimits
func TestSambaNovaCuratedModelsUseCurrentLimits(t *testing.T) {
	if got := InputLimitForProviderModel("sambanova", "DeepSeek-V3.2"); got != 32_000 {
		t.Fatalf("DeepSeek-V3.2 limit = %d, want 32000", got)
	}
	if got := InputLimitForProviderModel("sambanova", "MiniMax-M2.7"); got != 192_000 {
		t.Fatalf("MiniMax-M2.7 limit = %d, want 192000", got)
	}
}

// from sambanova_test.go: TestSambaNovaProviderListModelsUsesProviderScopedLimitsAndPricing
func TestSambaNovaProviderListModelsUsesProviderScopedLimitsAndPricing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-oss-120b","object":"model","created":1,"owned_by":"sambanova"},{"id":"DeepSeek-V3.2","object":"model","created":1,"owned_by":"sambanova"}]}`))
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
	if models[0].InputPrice != 0.22 || models[0].OutputPrice != 0.59 {
		t.Fatalf("gpt-oss-120b pricing = %g/%g, want 0.22/0.59", models[0].InputPrice, models[0].OutputPrice)
	}
	if models[1].InputLimit != 32_000 {
		t.Fatalf("DeepSeek-V3.2 InputLimit = %d, want 32000", models[1].InputLimit)
	}
	if models[1].InputPrice != 3.00 || models[1].OutputPrice != 4.50 {
		t.Fatalf("DeepSeek-V3.2 pricing = %g/%g, want 3/4.5", models[1].InputPrice, models[1].OutputPrice)
	}
}

// from venice_test.go: TestVeniceListModelsKeepsLiveContextLengthForUncuratedModels
func TestVeniceListModelsKeepsLiveContextLengthForUncuratedModels(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"brand-new-venice-model","created":123,"owned_by":"venice.ai","context_length":777000}]}`)
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "", "Venice")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1", len(models))
	}
	if models[0].InputLimit != 777000 {
		t.Fatalf("InputLimit = %d, want live context_length", models[0].InputLimit)
	}
	if got := InputLimitForProviderModel("venice", "brand-new-venice-model"); got != 777000 {
		t.Fatalf("cached InputLimitForProviderModel = %d, want 777000", got)
	}
}

const veniceBaseURL = "https://api.venice.ai/api/v1"

const (
	veniceCacheKey      = "venice"
	veniceImageCacheKey = venice.ImageCacheKey
)

func nearPriceEqual(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
