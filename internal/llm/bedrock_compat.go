package llm

import "github.com/samsaffron/term-llm/internal/llm/providers/bedrock"

// BedrockProvider is the AWS Bedrock provider implementation.
type BedrockProvider = bedrock.Provider

// NewBedrockProvider creates a new AWS Bedrock provider for Anthropic Claude models.
func NewBedrockProvider(model, region, profile, accessKey, secretKey, sessionToken string, modelMap, headers map[string]string) (*BedrockProvider, error) {
	return bedrock.New(model, region, profile, accessKey, secretKey, sessionToken, modelMap, headers)
}
