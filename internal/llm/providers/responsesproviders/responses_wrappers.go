package responsesproviders

import (
	"context"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

func BuildResponsesInputWithFilePolicy(messages []Message, policy responses.FilePolicy) []responses.ResponsesInputItem {
	return responses.BuildResponsesInputWithFilePolicy(messages, policy)
}

func BuildResponsesTools(specs []ToolSpec) []any { return responses.BuildResponsesTools(specs) }

func BuildResponsesToolsWithOptions(specs []ToolSpec, ptc core.ProgrammaticToolCallingOptions) []any {
	return responses.BuildResponsesToolsWithOptions(specs, ptc)
}

func BuildResponsesToolChoice(choice ToolChoice) interface{} {
	return responses.BuildResponsesToolChoice(choice)
}

func scanResponsesHistory(messages []Message) responses.HistoryFacts {
	return responses.ScanHistory(messages)
}

func guardReplayedWebSearch(req *ResponsesRequest, facts responses.HistoryFacts) {
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
