package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
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

// NewBedrockProvider creates a new AWS Bedrock provider for Anthropic Claude models.
func NewBedrockProvider(model, region, profile, accessKey, secretKey, sessionToken string, modelMap map[string]string) (*BedrockProvider, error) {
	// Parse model suffixes using the same chain as the Anthropic provider:
	// strip -thinking first (may leave -1m), then strip -1m.
	afterThinking, thinkingBudget, adaptive := parseModelThinking(model)
	baseModel, use1m := parseModel1m(afterThinking)

	// Build AWS config
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if accessKey != "" && secretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			awscreds.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Apply region fallback so the Bedrock client actually uses it
	if awsCfg.Region == "" {
		awsCfg.Region = "us-east-1"
	}
	resolvedRegion := awsCfg.Region

	// Resolve the base model name to a Bedrock model ID.
	// Skip resolution if the model is already a qualified Bedrock ID/ARN.
	// Uses the resolved region to derive the geographic prefix (us./eu./ap.).
	bedrockID := baseModel
	if !isQualifiedBedrockID(baseModel) {
		bedrockID = resolveBedrockModelID(baseModel, modelMap, resolvedRegion)
	}

	// Create Anthropic client routed through Bedrock
	client := anthropic.NewClient(bedrock.WithConfig(awsCfg))

	inner := &AnthropicProvider{
		client:         &client,
		model:          bedrockID,
		thinkingBudget: thinkingBudget,
		useAdaptive:    adaptive,
		use1m:          use1m,
		credential:     "aws",
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
