package llm

import (
	"context"
	"net/http"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

// The Responses API client, wire types, request builders and WebSocket
// transport live in providers/responses. These aliases and wrappers keep the
// original llm exports and the unexported names used by the remaining root
// providers (OpenAI, ChatGPT, Copilot, Grok, ...).

type (
	ResponsesClient                = responses.ResponsesClient
	ResponsesContentPart           = responses.ResponsesContentPart
	ResponsesImageGenerationTool   = responses.ResponsesImageGenerationTool
	ResponsesInputItem             = responses.ResponsesInputItem
	ResponsesMultiAgent            = responses.ResponsesMultiAgent
	ResponsesNamespace             = responses.ResponsesNamespace
	ResponsesNamespaceFunctionTool = responses.ResponsesNamespaceFunctionTool
	ResponsesPromptCacheOptions    = responses.ResponsesPromptCacheOptions
	ResponsesReasoning             = responses.ResponsesReasoning
	ResponsesRequest               = responses.ResponsesRequest
	ResponsesStreamOptions         = responses.ResponsesStreamOptions
	ResponsesText                  = responses.ResponsesText
	ResponsesTool                  = responses.ResponsesTool
	ResponsesToolSearchTool        = responses.ResponsesToolSearchTool
	ResponsesWebSearchTool         = responses.ResponsesWebSearchTool
	NonRecoverableStreamError      = responses.NonRecoverableStreamError
	responsesHistoryFacts          = responses.HistoryFacts
)

func init() {
	responses.ParentHooks = responses.Hooks{
		DebugRawSection:                DebugRawSection,
		EmitDebugDiagnostic:            emitDebugDiagnostic,
		HTTPClient:                     func() *http.Client { return defaultHTTPClient },
		OpenAIParametersFromToolSchema: openAIParametersFromToolSchema,
		SupportsReasoningMode:          SupportsReasoningMode,
		BaseModelAndEffortForProvider:  BaseModelAndEffortForProvider,
		IsEmbeddedFileText:             IsEmbeddedFileText,
		FormatUploadedFileNotice:       FormatUploadedFileNotice,
		CanonicalUploadMediaType:       CanonicalUploadMediaType,
		CanonicalTextUploadMediaType:   CanonicalTextUploadMediaType,
		IsTextLikeMediaType:            IsTextLikeMediaType,
		DefaultFileUploadPolicy: func() responses.FileUploadPolicy {
			return responses.FileUploadPolicy(DefaultOpenAIResponsesFileUploadPolicy())
		},
	}
}

func BuildResponsesInputWithInstructions(messages []Message) (string, []ResponsesInputItem) {
	return responses.BuildResponsesInputWithInstructions(messages)
}

func BuildResponsesInputWithInstructionsAndFilePolicy(messages []Message, policy responses.FilePolicy) (string, []ResponsesInputItem) {
	return responses.BuildResponsesInputWithInstructionsAndFilePolicy(messages, policy)
}

func BuildResponsesInput(messages []Message) []ResponsesInputItem {
	return responses.BuildResponsesInput(messages)
}

func BuildResponsesInputWithFilePolicy(messages []Message, policy responses.FilePolicy) []ResponsesInputItem {
	return responses.BuildResponsesInputWithFilePolicy(messages, policy)
}

func BuildResponsesContinuationInput(messages []Message) []ResponsesInputItem {
	return responses.BuildResponsesContinuationInput(messages)
}

func BuildResponsesContinuationInputWithFilePolicy(messages []Message, policy responses.FilePolicy) []ResponsesInputItem {
	return responses.BuildResponsesContinuationInputWithFilePolicy(messages, policy)
}

func BuildResponsesTools(specs []ToolSpec) []any { return responses.BuildResponsesTools(specs) }

func BuildResponsesToolsWithOptions(specs []ToolSpec, ptc ProgrammaticToolCallingOptions) []any {
	return responses.BuildResponsesToolsWithOptions(specs, ptc)
}

func BuildResponsesToolChoice(choice ToolChoice) interface{} {
	return responses.BuildResponsesToolChoice(choice)
}

func scanResponsesHistory(messages []Message) responsesHistoryFacts {
	return responses.ScanHistory(messages)
}

func guardReplayedWebSearch(req *ResponsesRequest, facts responsesHistoryFacts) {
	responses.GuardReplayedWebSearch(req, facts)
}

func mergeResponsesOptions(defaults ResponsesOptions, override *ResponsesOptions, ephemeral bool) ResponsesOptions {
	return responses.MergeOptions(defaults, override, ephemeral)
}

func validateResponsesOptions(provider, model string, opts *ResponsesOptions, tools []ToolSpec) (ResponsesOptions, error) {
	return responses.ValidateOptions(provider, model, opts, tools)
}

func responsesOptionsFromConfig(cfg config.ResponsesConfig) ResponsesOptions {
	return responses.OptionsFromConfig(cfg)
}

func parseModelEffortForProvider(provider, model string) (string, string) {
	return responses.ParseModelEffortForProvider(provider, model)
}

func cloneResponsesClientFreshConversation(c *ResponsesClient) *ResponsesClient {
	return responses.CloneClientFreshConversation(c)
}

func cloneResponsesClientForkConversation(c *ResponsesClient) (*ResponsesClient, bool) {
	return responses.CloneClientForkConversation(c)
}

func withResponsesWebSocketContinuationLifetime(ctx context.Context) context.Context {
	return responses.WithWebSocketContinuationLifetime(ctx)
}
