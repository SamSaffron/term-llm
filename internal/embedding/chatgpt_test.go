package embedding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTProviderUsesOAuthTokenAndModel(t *testing.T) {
	creds := &credentials.ChatGPTCredentials{AccessToken: "oauth-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	provider := NewChatGPTProvider()
	provider.model = "text-embedding-3-large"
	provider.loadCredentials = func() (*credentials.ChatGPTCredentials, error) { return creds, nil }
	provider.refresh = func(*credentials.ChatGPTCredentials) error {
		t.Fatal("unexpected token refresh")
		return nil
	}
	provider.embed = func(_ context.Context, token, model string, req EmbedRequest) (*EmbeddingResult, error) {
		if token != "oauth-token" {
			t.Fatalf("token = %q", token)
		}
		if model != "text-embedding-3-large" {
			t.Fatalf("model = %q", model)
		}
		if len(req.Texts) != 1 || req.Texts[0] != "hello" {
			t.Fatalf("request = %#v", req)
		}
		return &EmbeddingResult{Model: model, Dimensions: 3072}, nil
	}

	result, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"hello"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if result.Model != "text-embedding-3-large" || result.Dimensions != 3072 {
		t.Fatalf("result = %#v", result)
	}
}

func TestChatGPTProviderRefreshesExpiredToken(t *testing.T) {
	creds := &credentials.ChatGPTCredentials{AccessToken: "expired", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	provider := NewChatGPTProvider()
	provider.loadCredentials = func() (*credentials.ChatGPTCredentials, error) { return creds, nil }
	provider.refresh = func(got *credentials.ChatGPTCredentials) error {
		if got != creds {
			t.Fatal("refresh received different credentials")
		}
		got.AccessToken = "refreshed"
		got.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}
	provider.embed = func(_ context.Context, token, _ string, _ EmbedRequest) (*EmbeddingResult, error) {
		if token != "refreshed" {
			t.Fatalf("token = %q", token)
		}
		return &EmbeddingResult{}, nil
	}

	if _, err := provider.Embed(context.Background(), EmbedRequest{Texts: []string{"hello"}}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
}

func TestChatGPTProviderReportsCredentialFailure(t *testing.T) {
	provider := NewChatGPTProvider()
	provider.loadCredentials = func() (*credentials.ChatGPTCredentials, error) {
		return nil, errors.New("missing")
	}
	if _, err := provider.Embed(context.Background(), EmbedRequest{}); err == nil {
		t.Fatal("expected credential error")
	}
}
