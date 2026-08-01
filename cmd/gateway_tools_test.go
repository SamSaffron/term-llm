package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestRequiredGatewayFetchKeepsLegibleReadURLStub(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: "https://gateway.invalid", Required: true}}
	if tool := newReadURLToolForConfig(cfg); tool == nil {
		t.Fatal("gateway outage silently removed read_url")
	}
	_, err := (unavailableGatewayFetcher{err: context.DeadlineExceeded}).FetchURL(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "gateway read_url unavailable") || !strings.Contains(err.Error(), "gateway.fetch: false") {
		t.Fatalf("gateway read_url stub error = %v", err)
	}
}

func TestGatewaySearchFailureDoesNotSilentlyFallBack(t *testing.T) {
	searcher := unavailableGatewaySearcher{err: context.DeadlineExceeded}
	_, err := searcher.Search(context.Background(), "query", 10)
	if err == nil || !strings.Contains(err.Error(), "gateway search unavailable") || !strings.Contains(err.Error(), "gateway.search: false") {
		t.Fatalf("gateway search stub error = %v", err)
	}
}
