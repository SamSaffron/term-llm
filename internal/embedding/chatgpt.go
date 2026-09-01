package embedding

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/credentials"
)

// ChatGPTProvider uses the user's ChatGPT/Codex OAuth session with OpenAI's
// official embeddings endpoint. ChatGPT OAuth access and OpenAI API-key billing
// are separate authentication paths even though the wire API is identical.
type ChatGPTProvider struct {
	model           string
	loadCredentials func() (*credentials.ChatGPTCredentials, error)
	refresh         func(*credentials.ChatGPTCredentials) error
	embed           func(context.Context, string, string, EmbedRequest) (*EmbeddingResult, error)
}

func NewChatGPTProvider() *ChatGPTProvider {
	return &ChatGPTProvider{
		model:           openaiDefaultModel,
		loadCredentials: credentials.GetChatGPTCredentials,
		refresh:         credentials.RefreshChatGPTCredentials,
		embed: func(ctx context.Context, accessToken, model string, req EmbedRequest) (*EmbeddingResult, error) {
			provider := NewOpenAIProvider(accessToken)
			provider.model = model
			return provider.Embed(ctx, req)
		},
	}
}

func (p *ChatGPTProvider) Name() string {
	return "ChatGPT"
}

func (p *ChatGPTProvider) DefaultModel() string {
	return openaiDefaultModel
}

func (p *ChatGPTProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	creds, err := p.loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("ChatGPT embedding requires ChatGPT login: %w", err)
	}
	if creds.IsExpired() {
		if err := p.refresh(creds); err != nil {
			return nil, fmt.Errorf("refresh ChatGPT OAuth token for embedding: %w", err)
		}
	}
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("ChatGPT embedding credentials have no access token")
	}
	return p.embed(ctx, creds.AccessToken, p.model, req)
}
