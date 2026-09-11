package cmd

import (
	"context"
	"strings"
)

type preparedResponseRuntimePlan struct {
	requested       responseRuntimeSettings
	persisted       responseRuntimeSettings
	swap            responseModelSwapPlan
	provider        string
	defaultProvider string
}

// prepareResponseRuntimePlan resolves requested and durable identities without
// acquiring a runtime or taking ownership of cleanup.
func (s *serveServer) prepareResponseRuntimePlan(ctx context.Context, req *responsesCreateRequest, sessionID string, fresh bool) preparedResponseRuntimePlan {
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	requested := responseRequestedRuntime(*req, defaultProvider)
	req.Model, req.ReasoningEffort = requested.model, requested.effort
	persisted := requested
	if !fresh {
		persisted = s.persistedRuntimeSettings(ctx, sessionID, defaultProvider)
		if requested.reasoningMode == "" && persisted.reasoningMode != "" {
			if req.Reasoning == nil {
				req.Reasoning = &responsesReasoningRequest{}
			}
			req.Reasoning.Mode = persisted.reasoningMode
		}
	}
	swap := responseModelSwapPlan{}
	if !fresh {
		swap = buildResponseModelSwapPlan(*req, persisted, requested)
	}
	provider := strings.TrimSpace(req.Provider)
	if !fresh && !swap.enabled && s.store != nil && persisted.provider != "" {
		provider = persisted.provider
		if persisted.model != "" {
			req.Model = persisted.model
		}
		req.ReasoningEffort = persisted.effort
	}
	if swap.enabled {
		provider = swap.requestedProvider
		req.Model = swap.requestedModel
		req.ReasoningEffort = swap.requestedEffort
	}
	return preparedResponseRuntimePlan{requested: requested, persisted: persisted, swap: swap, provider: provider, defaultProvider: defaultProvider}
}
