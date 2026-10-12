package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/awsauth"
)

// BedrockProvider implements Provider using AWS Bedrock with Anthropic Claude models.
// It delegates all streaming/message logic to an embedded AnthropicProvider,
// only differing in client creation (AWS credentials instead of API key).
type BedrockProvider struct {
	inner  *AnthropicProvider
	region string // resolved AWS region for display
}

// bedrockBaseModelMap maps friendly model names to Bedrock model IDs without
// a geographic prefix. The prefix (us., eu., ap.) is derived from the
// configured region at provider creation time.
var bedrockBaseModelMap = map[string]string{
	// Current generation
	"claude-opus-4-7":   "anthropic.claude-opus-4-7-v1",
	"claude-opus-4-6":   "anthropic.claude-opus-4-6-v1",
	"claude-sonnet-4-6": "anthropic.claude-sonnet-4-6",
	"claude-haiku-4-5":  "anthropic.claude-haiku-4-5-20251001-v1:0",
	// Previous generation
	"claude-sonnet-4-5": "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-opus-4-5":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-sonnet-4":   "anthropic.claude-sonnet-4-20250514-v1:0",
}

// bedrockGeoPrefix returns the cross-region inference prefix for the given
// AWS region. This determines data residency for cross-region inference.
func bedrockGeoPrefix(region string) string {
	switch {
	case strings.HasPrefix(region, "eu-"):
		return "eu"
	case strings.HasPrefix(region, "ap-"):
		return "ap"
	default:
		return "us"
	}
}

// resolveBedrockModelID translates a friendly model name to a Bedrock model ID.
// Resolution order: userMap (config) -> built-in map with geo prefix -> passthrough.
// The model should already have -thinking/-1m suffixes stripped.
func resolveBedrockModelID(model string, userMap map[string]string, region string) string {
	// 1. User-defined model_map from config
	if userMap != nil {
		if id, ok := userMap[model]; ok {
			return id
		}
	}

	// 2. Built-in translation table with geography-appropriate prefix
	if base, ok := bedrockBaseModelMap[model]; ok {
		return bedrockGeoPrefix(region) + "." + base
	}

	// 3. Passthrough
	return model
}

// isQualifiedBedrockID returns true if the model string is already a
// fully-qualified Bedrock model ID or ARN that should not be translated.
func isQualifiedBedrockID(model string) bool {
	for _, prefix := range []string{
		"anthropic.",
		"us.anthropic.",
		"eu.anthropic.",
		"ap.anthropic.",
		"global.anthropic.",
		"arn:aws:bedrock:",
	} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// resolveBedrockAuth resolves the region and SigV4 credential chain. With a
// Bedrock API key (AWS_BEARER_TOKEN_BEDROCK) the credential chain is unused,
// so a broken shared profile must not prevent provider creation; the region
// then falls back to config and environment.
func resolveBedrockAuth(region, profile, accessKey, secretKey, sessionToken, bearerToken string) (string, awsauth.CredentialsProvider, error) {
	resolved, err := awsauth.Resolve(context.Background(), awsauth.Options{
		Region:          region,
		Profile:         profile,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		SessionToken:    sessionToken,
	})
	var resolvedRegion string
	var credentials awsauth.CredentialsProvider
	switch {
	case err == nil:
		resolvedRegion, credentials = resolved.Region, resolved.Credentials
	case bearerToken != "":
		resolvedRegion = firstNonEmpty(region, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"))
	default:
		return "", nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	// Apply region fallback so the Bedrock client actually uses it
	if resolvedRegion == "" {
		resolvedRegion = "us-east-1"
	}
	return resolvedRegion, credentials, nil
}

// NewBedrockProvider creates a new AWS Bedrock provider for Anthropic Claude models.
// headers are extra HTTP headers; anthropic-beta values are moved into the
// request body as Bedrock requires, and the rest are SigV4-signed.
func NewBedrockProvider(model, region, profile, accessKey, secretKey, sessionToken string, modelMap, headers map[string]string) (*BedrockProvider, error) {
	extraHeaders, err := newAnthropicExtraHeaders(headers)
	if err != nil {
		return nil, err
	}
	bearerToken := os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	resolvedRegion, credentials, err := resolveBedrockAuth(region, profile, accessKey, secretKey, sessionToken, bearerToken)
	if err != nil {
		return nil, err
	}

	// Friendly names resolve through model_map, then the built-in table with
	// a geographic prefix from the resolved region; qualified IDs/ARNs pass
	// through. The same resolver decodes per-request models, so the engine's
	// request model (the configured friendly name) never reaches Bedrock.
	resolveID := func(name string) string {
		if isQualifiedBedrockID(name) {
			return name
		}
		return resolveBedrockModelID(name, modelMap, resolvedRegion)
	}
	spec := parseAnthropicModelSpec(model, resolveID)

	client := &anthropicClient{
		headers:    extraHeaders,
		httpClient: newAnthropicHTTPClient(),
		bedrock: &bedrockTransport{
			region:      resolvedRegion,
			credentials: credentials,
			bearerToken: bearerToken,
			now:         time.Now,
		},
	}
	inner := &AnthropicProvider{
		client:          client,
		model:           spec.model,
		thinkingBudget:  spec.thinkingBudget,
		useAdaptive:     spec.adaptive,
		use1m:           spec.use1m,
		reasoningEffort: spec.effort,
		credential:      "aws",
		resolveModelID:  resolveID,
	}

	return &BedrockProvider{
		inner:  inner,
		region: resolvedRegion,
	}, nil
}

func (p *BedrockProvider) Name() string {
	model := p.inner.model
	suffix := ""
	if p.inner.use1m {
		suffix = ", 1m"
	}
	if p.inner.useAdaptive {
		return fmt.Sprintf("Bedrock (%s, adaptive%s, %s)", model, suffix, p.region)
	}
	if p.inner.thinkingBudget > 0 {
		return fmt.Sprintf("Bedrock (%s, thinking=%dk%s, %s)", model, p.inner.thinkingBudget/1000, suffix, p.region)
	}
	if p.inner.use1m {
		return fmt.Sprintf("Bedrock (%s, 1m, %s)", model, p.region)
	}
	return fmt.Sprintf("Bedrock (%s, %s)", model, p.region)
}

func (p *BedrockProvider) Credential() string {
	return "aws"
}

func (p *BedrockProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     true,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *BedrockProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return p.inner.Stream(ctx, req)
}
