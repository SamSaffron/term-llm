package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestGeminiEmbedUsesBatchRESTEndpoint(t *testing.T) {
	var got geminiBatchEmbedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/custom-model:batchEmbedContents" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if key := r.Header.Get("x-goog-api-key"); key != "test-key" {
			t.Fatalf("x-goog-api-key = %q", key)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.25,0.5]},{"values":[0.75,1]}]}`))
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL + "/v1beta/models"
	provider.client = server.Client()
	result, err := provider.Embed(context.Background(), EmbedRequest{Model: "custom-model", Texts: []string{"one", "two"}, TaskType: "RETRIEVAL_DOCUMENT", Dimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != 2 || got.Requests[0].Model != "models/custom-model" || got.Requests[0].Content.Parts[0].Text != "one" {
		t.Fatalf("request = %+v", got)
	}
	if got.Requests[0].TaskType != "RETRIEVAL_DOCUMENT" || got.Requests[0].OutputDimensionality != 2 {
		t.Fatalf("request config = %+v", got.Requests[0])
	}
	if result.Dimensions != 2 || len(result.Embeddings) != 2 || result.Embeddings[1].Text != "two" || result.Embeddings[1].Vector[0] != 0.75 {
		t.Fatalf("result = %+v", result)
	}
}

func TestGeminiEmbedRejectsMismatchedResponseCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[1]}]}`))
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()
	_, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"one", "two"}})
	if err == nil || !strings.Contains(err.Error(), "returned 1 embeddings for 2 inputs") {
		t.Fatalf("error = %v", err)
	}
}

func TestGeminiEmbedReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()
	_, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"one"}})
	var statusErr *providerhttp.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want typed status 429", err)
	}
}

func TestGeminiEmbedReportsDecodeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()
	_, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"one"}})
	if err == nil || !strings.Contains(err.Error(), "decode Gemini embedding response") {
		t.Fatalf("error = %v", err)
	}
}

func TestGeminiEmbedDefaultClientHasTransportTimeouts(t *testing.T) {
	provider := NewGeminiProvider("test-key")
	transport, ok := provider.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", provider.client.Transport)
	}
	if transport.DialContext == nil || transport.TLSHandshakeTimeout == 0 || transport.ResponseHeaderTimeout == 0 || transport.IdleConnTimeout == 0 {
		t.Fatalf("transport timeouts are incomplete: %+v", transport)
	}
}

func TestGeminiEmbedClientHandlesWrappedDefaultTransport(t *testing.T) {
	client := newGeminiEmbedHTTPClient(embeddingRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Fatalf("transport = %T, want fallback *http.Transport", client.Transport)
	}
}

func TestGeminiEmbedHonorsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := provider.Embed(ctx, EmbedRequest{Texts: []string{"one"}})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Embed did not stop after cancellation")
	}
}

type embeddingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f embeddingRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
