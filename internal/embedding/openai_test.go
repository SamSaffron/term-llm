package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/openaiapi"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestOpenAIProviderEmbedRequestAndResponse(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]},{"object":"embedding","index":1,"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":6,"total_tokens":6}}`))
	}))
	defer server.Close()

	provider := NewOpenAIProvider("test-key")
	provider.endpoint = openaiapi.Endpoint{BaseURL: server.URL + "/v1"}
	result, err := provider.Embed(context.Background(), EmbedRequest{
		Texts:      []string{"alpha", "beta"},
		Model:      "text-embedding-3-small",
		Dimensions: 2,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	wantReq := map[string]any{
		"model":           "text-embedding-3-small",
		"input":           []any{"alpha", "beta"},
		"encoding_format": "float",
		"dimensions":      float64(2),
	}
	if !reflect.DeepEqual(got, wantReq) {
		t.Fatalf("request body = %#v, want %#v", got, wantReq)
	}
	if result.Model != "text-embedding-3-small" || result.Dimensions != 2 || len(result.Embeddings) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Embeddings[1].Index != 1 || result.Embeddings[1].Text != "beta" || !reflect.DeepEqual(result.Embeddings[1].Vector, []float64{0.3, 0.4}) {
		t.Fatalf("embedding[1] = %#v", result.Embeddings[1])
	}
	if result.Usage == nil || result.Usage.PromptTokens != 6 || result.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestOpenAIProviderEmbedOmitsZeroDimensions(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"model":"m","data":[]}`))
	}))
	defer server.Close()

	provider := NewOpenAIProvider("test-key")
	provider.endpoint = openaiapi.Endpoint{BaseURL: server.URL}
	result, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, ok := got["dimensions"]; ok {
		t.Fatalf("dimensions should be omitted: %#v", got)
	}
	if got["model"] != openaiDefaultModel {
		t.Fatalf("model = %v, want default %q", got["model"], openaiDefaultModel)
	}
	if result.Usage != nil || result.Dimensions != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestOpenAIProviderEmbedReturnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer server.Close()

	provider := NewOpenAIProvider("bad-key")
	provider.endpoint = openaiapi.Endpoint{BaseURL: server.URL}
	_, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"x"}})
	var statusErr *providerhttp.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 StatusError", err)
	}
}

func TestOpenAIProviderEmbedSendsOrganizationAndProject(t *testing.T) {
	var gotOrg, gotProject, gotPath, gotAuth, gotType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		gotOrg = r.Header.Get("OpenAI-Organization")
		gotProject = r.Header.Get("OpenAI-Project")
		_, _ = w.Write([]byte(`{"model":"m","data":[]}`))
	}))
	defer server.Close()

	provider := NewOpenAIProviderWithEndpoint("test-key", openaiapi.Endpoint{BaseURL: server.URL + "/proxy/v1", Headers: map[string]string{"OpenAI-Organization": "org-1", "OpenAI-Project": "proj-1", "Authorization": "Bearer other", "Content-Type": "text/plain"}})
	if _, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"x"}}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/proxy/v1/embeddings" || gotOrg != "org-1" || gotProject != "proj-1" || gotAuth != "Bearer test-key" || gotType != "application/json" {
		t.Fatalf("path/org/project/auth/type = %q/%q/%q/%q/%q", gotPath, gotOrg, gotProject, gotAuth, gotType)
	}
}

func TestNewEmbeddingProviderOpenAIUsesSharedEndpoint(t *testing.T) {
	t.Setenv(openaiapi.EnvBaseURL, "https://env.example/v1")
	t.Setenv(openaiapi.EnvOrganization, "org-env")
	t.Setenv(openaiapi.EnvProject, "proj-env")

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {Headers: map[string]string{"OpenAI-Organization": "$(printf org-config)"}},
	}}
	cfg.Embed.OpenAI.APIKey = "embed-key"
	provider, err := NewEmbeddingProvider(cfg, "openai")
	if err != nil {
		t.Fatalf("NewEmbeddingProvider: %v", err)
	}
	openai, ok := provider.(*OpenAIProvider)
	if !ok {
		t.Fatalf("provider type = %T", provider)
	}
	want := openaiapi.Endpoint{BaseURL: "https://env.example/v1", Headers: map[string]string{"OpenAI-Organization": "org-config", "OpenAI-Project": "proj-env"}}
	if !reflect.DeepEqual(openai.endpoint, want) {
		t.Fatalf("endpoint = %#v, want %#v", openai.endpoint, want)
	}
}
