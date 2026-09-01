package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestVeniceProviderEmbedsWithQwenQueryInstruction(t *testing.T) {
	var got veniceEmbedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"text-embedding-qwen3-8b","data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`))
	}))
	defer server.Close()

	provider := NewVeniceProvider("test-key", server.URL)
	result, err := provider.Embed(context.Background(), EmbedRequest{
		Texts:    []string{"Where is the deploy note?"},
		TaskType: "RETRIEVAL_QUERY",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	wantInput := []string{"Instruct: Given a user question, retrieve relevant passages that answer it\nQuery: Where is the deploy note?"}
	if !reflect.DeepEqual(got.Input, wantInput) {
		t.Fatalf("input = %#v, want %#v", got.Input, wantInput)
	}
	if got.Model != "text-embedding-qwen3-8b" || got.EncodingFormat != "float" {
		t.Fatalf("request = %#v", got)
	}
	if result.Model != got.Model || result.Dimensions != 3 || result.Usage == nil || result.Usage.TotalTokens != 4 {
		t.Fatalf("result = %#v", result)
	}
}

func TestVeniceProviderLeavesNonQwenDocumentsUnchanged(t *testing.T) {
	input := []string{"document"}
	got := prepareQwen3EmbeddingTexts("text-embedding-bge-en-icl", "RETRIEVAL_DOCUMENT", input)
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("input = %#v", got)
	}
}
