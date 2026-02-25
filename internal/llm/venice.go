package llm

import "context"

const veniceBaseURL = "https://api.venice.ai/api/v1"

type VeniceProvider struct {
	*OpenAICompatProvider
}

func NewVeniceProvider(apiKey, model string) *VeniceProvider {
	if model == "" {
		model = "venice-uncensored"
	}
	return &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(veniceBaseURL, apiKey, model, "Venice")}
}

func (p *VeniceProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.ParallelToolCalls = false
	return p.OpenAICompatProvider.Stream(ctx, req)
}
