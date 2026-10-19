package engine

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"io"
	"strings"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func (e *Engine) runSimpleScratchpad(ctx context.Context, req core.Request, send corestream.Sender) error {
	turnCallback := e.getTurnCallback()
	snapshotCallback := e.getSnapshotCallback()
	var priorErr error
	for retry := 0; ; retry++ {
		e.applyPendingServiceTier(&req)
		providerReq := e.prepareProviderRequest(req)
		e.ClearInlineFlush()
		stream, err := e.provider.Stream(ctx, providerReq)
		if err != nil {
			return err
		}

		var scratchpadHasDiscardableOutput bool
		var textBuilder strings.Builder
		var reasoningBuilder strings.Builder
		var reasoningTextItemID string
		var reasoningItemID string
		var reasoningEncryptedContent string
		var reasoningSummaryParts []string
		var reasoningKind core.ReasoningKind
		var providerReplayParts []core.Part
		var metrics core.TurnMetrics
		var failed error
		fireSnapshot := func() {
			if snapshotCallback == nil {
				return
			}
			msg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(), nil, reasoningBuilder.String(), reasoningSummaryParts,
				reasoningItemID, reasoningEncryptedContent, reasoningKind,
			)
			msg = attachProviderReplayParts(msg, providerReplayParts)
			if len(msg.Parts) == 0 {
				return
			}
			cbCtx, cancel := callbackContext(ctx)
			_ = snapshotCallback(cbCtx, 0, msg)
			cancel()
		}

		for {
			if err := e.consumeChaosFailure(); err != nil {
				failed = err
				break
			}
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				failed = err
				break
			}
			if event.Type == core.EventError && event.Err != nil {
				failed = event.Err
				break
			}
			if req.DebugRaw {
				env.debugRawEvent(true, event)
			}
			switch event.Type {
			case core.EventTextDelta:
				if event.Text != "" {
					textBuilder.WriteString(event.Text)
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case core.EventReasoningDelta:
				internalreasoning.AppendStreamItemText(&reasoningBuilder, &reasoningTextItemID, event.Text, event.ReasoningItemID)
				if event.Text != "" {
					reasoningKind = core.MergeReasoningKind(reasoningKind, event.ReasoningKind)
				}
				if event.ReasoningItemID != "" {
					reasoningItemID = event.ReasoningItemID
				}
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
					reasoningKind = core.MergeReasoningKind(reasoningKind, core.ReasoningKindSummary)
				}
				if event.ReasoningEncryptedContent != "" {
					reasoningEncryptedContent = event.ReasoningEncryptedContent
					reasoningKind = core.MergeReasoningKind(reasoningKind, event.ReasoningKind)
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case core.EventUsage:
				if event.Use != nil {
					metrics.InputTokens += event.Use.InputTokens
					metrics.OutputTokens += event.Use.OutputTokens
					metrics.CachedInputTokens += event.Use.CachedInputTokens
					metrics.CacheWriteTokens += event.Use.CacheWriteTokens
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case core.EventImageGenerated:
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case core.EventToolActivity:
				if event.ToolActivity != nil {
					providerReplayParts = upsertToolActivityPart(providerReplayParts, event.ToolActivity)
					scratchpadHasDiscardableOutput = true
					fireSnapshot()
				}
			case core.EventProviderReplay:
				if event.ProviderReplay != nil && len(event.ProviderReplay.Raw) > 0 {
					replay := &core.ProviderReplayItem{Raw: append(json.RawMessage(nil), event.ProviderReplay.Raw...)}
					providerReplayParts = append(providerReplayParts, core.Part{Type: core.PartProviderReplay, ProviderReplay: replay})
					scratchpadHasDiscardableOutput = true
				}
			case core.EventDone:
				// The engine emits one done event after committing the scratchpad.
			default:
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			}
		}
		_ = stream.Close()

		if failed != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			priorErr = failed
			if retry >= defaultUncommittedStreamMaxRetries || !isUncommittedReplayableStreamError(failed) {
				return failed
			}
			attempt := retry + 1
			if scratchpadHasDiscardableOutput {
				if err := send.Send(core.Event{Type: core.EventAttemptDiscard}); err != nil {
					return err
				}
			}
			if err := send.Send(core.Event{Type: core.EventRetry, RetryAttempt: attempt, RetryMaxAttempts: defaultUncommittedStreamMaxRetries, RetryWaitSecs: 0}); err != nil {
				return err
			}
			runtimeoutput.Debug("retrying failed uncommitted model stream", "attempt", attempt, "error", failed)
			continue
		}

		if textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(reasoningSummaryParts) == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && len(providerReplayParts) == 0 && priorErr != nil {
			return priorErr
		}
		if turnCallback != nil && (textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" || len(providerReplayParts) > 0) {
			reasoningText := reasoningBuilder.String()
			if reasoningText == "" && len(reasoningSummaryParts) > 0 {
				reasoningText = strings.Join(reasoningSummaryParts, "\n\n")
			}
			if reasoningText != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
				reasoningKind = core.NormalizeReasoningKind(reasoningKind)
			}
			reasoningTitle := ""
			if reasoningKind == core.ReasoningKindSummary {
				reasoningTitle = internalreasoning.ParseReasoningSummary(reasoningText).Title
			}
			finalMsg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
				Type:                      core.PartText,
				Text:                      textBuilder.String(),
				ReasoningContent:          reasoningText,
				ReasoningSummaryParts:     append([]string(nil), reasoningSummaryParts...),
				ReasoningItemID:           reasoningItemID,
				ReasoningEncryptedContent: reasoningEncryptedContent,
				ReasoningKind:             reasoningKind,
				ReasoningSummaryTitle:     reasoningTitle,
			}}}
			finalMsg = attachProviderReplayParts(finalMsg, providerReplayParts)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, 0, []core.Message{finalMsg}, metrics)
			cancel()
		}
		return send.Send(core.Event{Type: core.EventDone})
	}
}

func attachProviderReplayParts(msg core.Message, parts []core.Part) core.Message {
	seenActivities := make(map[string]struct{})
	for _, part := range parts {
		if part.Type != core.PartToolActivity || part.ToolActivity == nil {
			continue
		}
		activity := core.CloneToolActivity(part.ToolActivity)
		key := activity.ID + "\x00" + activity.Name
		if _, seen := seenActivities[key]; seen {
			continue
		}
		seenActivities[key] = struct{}{}
		msg.Parts = append(msg.Parts, core.Part{Type: core.PartToolActivity, ToolActivity: activity})
	}
	msg.Parts = append(msg.Parts, cloneProviderReplayParts(parts)...)
	return msg
}

func upsertToolActivityPart(parts []core.Part, activity *core.ToolActivity) []core.Part {
	if activity == nil {
		return parts
	}
	for i := range parts {
		if parts[i].Type == core.PartToolActivity && parts[i].ToolActivity != nil &&
			parts[i].ToolActivity.ID == activity.ID && parts[i].ToolActivity.Name == activity.Name {
			parts[i].ToolActivity = core.CloneToolActivity(activity)
			return parts
		}
	}
	return append(parts, core.Part{Type: core.PartToolActivity, ToolActivity: core.CloneToolActivity(activity)})
}

func cloneProviderReplayParts(parts []core.Part) []core.Part {
	if len(parts) == 0 {
		return nil
	}
	out := make([]core.Part, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case core.PartProviderReplay:
			if part.ProviderReplay != nil && len(part.ProviderReplay.Raw) > 0 {
				out = append(out, core.Part{Type: core.PartProviderReplay, ProviderReplay: &core.ProviderReplayItem{Raw: append(json.RawMessage(nil), part.ProviderReplay.Raw...)}})
			}
		case core.PartDiscoveryCall:
			if part.DiscoveryCall != nil {
				call := *part.DiscoveryCall
				call.Arguments = append(json.RawMessage(nil), part.DiscoveryCall.Arguments...)
				out = append(out, core.Part{Type: core.PartDiscoveryCall, DiscoveryCall: &call})
			}
		case core.PartDiscoveryOutput:
			if part.DiscoveryOutput != nil {
				out = append(out, core.Part{Type: core.PartDiscoveryOutput, DiscoveryOutput: core.CloneToolDiscoveryOutput(part.DiscoveryOutput)})
			}
		}
	}
	return out
}

func collectToolDiscoveryReplay(messages []core.Message) []core.Part {
	var replay []core.Part
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type != core.PartDiscoveryCall && part.Type != core.PartDiscoveryOutput {
				continue
			}
			if cloned, ok := core.ClonePart(part); ok {
				replay = append(replay, cloned)
			}
		}
	}
	return replay
}

func restoreToolDiscoveryReplay(messages []core.Message, replay []core.Part) []core.Message {
	if len(replay) == 0 {
		return messages
	}
	cleaned := make([]core.Message, 0, len(messages)+1)
	for _, message := range messages {
		copyMessage := message
		copyMessage.Parts = copyMessage.Parts[:0]
		for _, part := range message.Parts {
			if part.Type != core.PartDiscoveryCall && part.Type != core.PartDiscoveryOutput {
				copyMessage.Parts = append(copyMessage.Parts, part)
			}
		}
		if len(copyMessage.Parts) > 0 {
			cleaned = append(cleaned, copyMessage)
		}
	}
	insertAt := len(cleaned)
	for i := len(cleaned) - 1; i >= 0; i-- {
		if cleaned[i].Role == core.RoleUser {
			insertAt = i
			break
		}
	}
	replayMessage := core.Message{Role: core.RoleAssistant, Parts: core.CloneParts(replay)}
	cleaned = append(cleaned, core.Message{})
	copy(cleaned[insertAt+1:], cleaned[insertAt:])
	cleaned[insertAt] = replayMessage
	return cleaned
}
