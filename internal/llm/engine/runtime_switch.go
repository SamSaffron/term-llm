package engine

import (
	"context"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"strings"
)

// applyPendingRequestModelSwitch applies one queued runtime change at the
// provider-turn boundary. The run loop retains turn and ordering ownership.
func (e *Engine) applyPendingRequestModelSwitch(ctx context.Context, req *core.Request, send corestream.Sender, runID string, ordinal *int, attempt int) error {
	pending := e.drainPendingRequestRuntimeSwitch()
	if pending.model == "" && pending.reasoningEffort == "" {
		return nil
	}
	targetModel := pending.model
	if targetModel == "" {
		targetModel = req.Model
	}
	targetEffort := pending.reasoningEffort
	previousModel := strings.TrimSpace(req.Model)
	previousEffort := strings.TrimSpace(req.ReasoningEffort)
	if targetModel == previousModel && targetEffort == previousEffort {
		return nil
	}
	if previousModel == "" || targetModel == "" {
		return fmt.Errorf("model switch requires complete runtime identity: %q -> %q", previousModel, targetModel)
	}
	req.Model = targetModel
	req.ReasoningEffort = targetEffort
	*ordinal++
	change := RuntimeSwitch{
		PreviousModel:           previousModel,
		PreviousReasoningEffort: previousEffort,
		Model:                   targetModel,
		ReasoningEffort:         targetEffort,
		ProviderTurnIndex:       attempt,
		BoundaryID:              fmt.Sprintf("%s:model-switch:%d", runID, *ordinal),
	}
	if callback := e.getRuntimeSwitchCallback(); callback != nil {
		callbackCtx, cancel := callbackContext(ctx)
		err := callback(callbackCtx, change)
		cancel()
		if err != nil {
			return fmt.Errorf("persist model switch boundary: %w", err)
		}
	}
	if err := send.Send(core.Event{
		Type:                    core.EventModelSwitch,
		Text:                    targetModel,
		Model:                   targetModel,
		ReasoningEffort:         targetEffort,
		PreviousModel:           previousModel,
		PreviousReasoningEffort: previousEffort,
		ProviderTurnIndex:       attempt,
		ProviderTurnIndexSet:    true,
		ModelSwitchBoundaryID:   change.BoundaryID,
	}); err != nil {
		return err
	}
	if req.DebugRaw {
		env.debugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), *req, fmt.Sprintf("Request model switched before turn %d", attempt))
	}
	return nil
}
