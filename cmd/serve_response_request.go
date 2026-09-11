package cmd

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// buildResponsesLLMRequest projects protocol options without taking ownership
// of runtime, persistence, admission, or execution.
func (s *serveServer) buildResponsesLLMRequest(req responsesCreateRequest, runtime *serveRuntime, sessionID string, firstParty bool) llm.Request {
	searchFromTools, ptc, requested, passthrough := parseRequestedTools(req.Tools)
	toolChoice := parseToolChoice(req.ToolChoice)
	tools := appendResponsePassthroughTools(responseServerTools(runtime, requested, req.IncludeServerTools || firstParty), passthrough, runtime.toolMap)
	if len(tools) == 0 {
		toolChoice = llm.ToolChoice{}
	}
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}
	effort := normalizeReasoningEffort(req.ReasoningEffort)
	options := &llm.ResponsesOptions{}
	if req.Reasoning != nil {
		if nested := normalizeReasoningEffort(req.Reasoning.Effort); nested != "" {
			effort = nested
		}
		options.ReasoningMode = req.Reasoning.Mode
		options.ReasoningContext = req.Reasoning.Context
	}
	if req.MultiAgent != nil {
		options.MultiAgent = llm.MultiAgentOptions{Enabled: req.MultiAgent.Enabled, EnabledSet: true, MaxConcurrentSubagents: req.MultiAgent.MaxConcurrentSubagents}
	}
	if req.PromptCacheOptions != nil {
		options.PromptCache = llm.PromptCacheOptions{Mode: req.PromptCacheOptions.Mode, TTL: req.PromptCacheOptions.TTL}
	}
	if ptc {
		options.ProgrammaticToolCalling.Enabled = true
		options.ProgrammaticToolCalling.EnabledSet = true
		for _, tool := range tools {
			for _, caller := range tool.AllowedCallers {
				if caller == "programmatic" {
					options.ProgrammaticToolCalling.Tools = append(options.ProgrammaticToolCalling.Tools, tool.Name)
					break
				}
			}
		}
	}
	if options.IsZero() {
		options = nil
	}
	out := llm.Request{SessionID: sessionID, Model: strings.TrimSpace(req.Model), ReasoningEffort: effort, Responses: options, Tools: tools, ToolChoice: toolChoice, ParallelToolCalls: parallel, Search: runtime.search || searchFromTools, ForceExternalSearch: runtime.forceExternalSearch, MaxTurns: runtime.maxTurns, ToolMap: runtime.toolMap, Debug: runtime.debug, DebugRaw: runtime.debugRaw}
	if req.ServiceTier != nil {
		out.ServiceTier = llm.NormalizeServiceTier(*req.ServiceTier)
		out.ServiceTierSet = true
	}
	if req.MaxOutputTokens > 0 {
		out.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out.Temperature = *req.Temperature
		out.TemperatureSet = true
	}
	if req.TopP != nil {
		out.TopP = *req.TopP
		out.TopPSet = true
	}
	return out
}
