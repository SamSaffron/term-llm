package cmd

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func (s *serveServer) appendResponseTextDelta(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	segmentOrdinal := state.assistantSegmentOrdinal
	if ev.ProviderTurnIndexSet && ev.ProviderTurnIndex > segmentOrdinal {
		// Durable assistant rows are keyed by the engine's provider turn index.
		// Tool-only turns emit no text, so counting only text-after-tool
		// boundaries makes the live projection lag those durable identities.
		segmentOrdinal = ev.ProviderTurnIndex
	} else if state.toolsSeen || state.assistantBoundaryPending {
		// Preserve an ordered boundary for inline tools and steering that
		// continue within one provider turn.
		segmentOrdinal++
	}
	if segmentOrdinal != state.assistantSegmentOrdinal {
		state.assistantSegmentOrdinal = segmentOrdinal
		if err := run.appendEvent("response.output_text.new_segment", map[string]any{
			"output_index":              state.outputIndex,
			"assistant_segment_ordinal": state.assistantSegmentOrdinal,
		}); err != nil {
			return err
		}
		state.toolsSeen = false
		state.assistantBoundaryPending = false
	}
	return run.appendTextDeltaSegmentEvent(state.outputIndex, state.assistantSegmentOrdinal, ev.Text)
}

func (s *serveServer) appendResponseAttemptDiscard(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	state.toolsSeen = false
	return run.appendEvent("response.attempt.discard", map[string]any{
		"output_index": state.outputIndex,
	})
}

func (s *serveServer) appendResponseToolCall(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	if ev.Tool == nil {
		return nil
	}
	// Suppress tool call metadata for server-executed tools in API mode, but
	// retain the assistant boundary so persisted callback turn ordinals and
	// streamed segment identities remain aligned end to end.
	if s.suppressResponseRunServerToolEvent(runtime, ev.Tool.Name) {
		state.toolsSeen = true
		return nil
	}
	state.toolsSeen = true
	itemID := "fc_" + ev.Tool.ID
	item := map[string]any{
		"id":        itemID,
		"type":      "function_call",
		"call_id":   ev.Tool.ID,
		"name":      ev.Tool.Name,
		"arguments": string(ev.Tool.Arguments),
	}
	identity := map[string]any{
		"output_index":              state.outputIndex,
		"assistant_segment_ordinal": state.assistantSegmentOrdinal,
		"call_id":                   ev.Tool.ID,
		"item_id":                   itemID,
	}
	if ev.ProviderTurnIndexSet {
		identity["provider_turn_index"] = ev.ProviderTurnIndex
	}
	added := maps.Clone(identity)
	added["item"] = item
	if err := run.appendEvent("response.output_item.added", added); err != nil {
		return err
	}
	delta := maps.Clone(identity)
	delta["delta"] = string(ev.Tool.Arguments)
	if err := run.appendEvent("response.function_call_arguments.delta", delta); err != nil {
		return err
	}
	done := maps.Clone(identity)
	done["item"] = item
	if err := run.appendEvent("response.output_item.done", done); err != nil {
		return err
	}
	state.outputIndex++
	return nil
}

func (s *serveServer) appendResponseToolExecStart(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	if runtime != nil {
		runtime.beginSubagentProgress(ev.ToolCallID, ev.ToolName)
	}
	// ask_user is a user-facing control event, not tool metadata. Emit its
	// prompt even when server-executed tool details are hidden; otherwise the
	// live web stream stalls until a reload recovers the pending prompt.
	if ev.ToolName == tools.AskUserToolName && runtime != nil {
		if prompt, err := runtime.prepareAskUserFromToolArgs(ev.ToolCallID, ev.ToolArgs); err == nil {
			if err := run.appendEvent("response.ask_user.prompt", map[string]any{
				"call_id":    prompt.CallID,
				"questions":  prompt.Questions,
				"created_at": prompt.CreatedAt,
			}); err != nil {
				return err
			}
		}
	}
	if s.suppressResponseRunServerToolEvent(runtime, ev.ToolName) {
		return nil
	}
	startedAt := time.Now().UnixMilli()
	if state.toolStartedAt == nil {
		state.toolStartedAt = make(map[string]int64)
	}
	if previous := state.toolStartedAt[ev.ToolCallID]; previous > 0 {
		startedAt = previous
	} else {
		state.toolStartedAt[ev.ToolCallID] = startedAt
	}
	return run.appendEvent("response.tool_exec.start", map[string]any{
		"call_id":        ev.ToolCallID,
		"tool_name":      ev.ToolName,
		"tool_info":      ev.ToolInfo,
		"tool_arguments": string(ev.ToolArgs),
		"started_at":     startedAt,
	})
}

func (s *serveServer) appendResponseToolExecEnd(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	runtime.finishSubagentProgress(ev.ToolCallID, ev.ToolSuccess)
	if ev.ToolName == tools.AskUserToolName && runtime != nil {
		runtime.clearPendingAskUser(ev.ToolCallID)
	}
	suppressed := s.suppressResponseRunServerToolEvent(runtime, ev.ToolName)
	if suppressed && ev.ToolName != tools.AskUserToolName {
		return nil
	}
	payload := map[string]any{
		"call_id":   ev.ToolCallID,
		"tool_name": ev.ToolName,
		"success":   ev.ToolSuccess,
	}
	if startedAt := state.toolStartedAt[ev.ToolCallID]; startedAt > 0 {
		endedAt := time.Now().UnixMilli()
		payload["started_at"] = startedAt
		payload["ended_at"] = endedAt
		payload["duration_ms"] = max(int64(0), endedAt-startedAt)
		delete(state.toolStartedAt, ev.ToolCallID)
	}
	if !suppressed && ev.ToolInfo != "" {
		payload["tool_info"] = ev.ToolInfo
	}
	if !suppressed && len(ev.ToolArgs) > 0 {
		payload["tool_arguments"] = string(ev.ToolArgs)
	}
	if ev.ToolSuccess && ev.ToolName == tools.AskUserToolName {
		if summary := askUserResultSummary(ev.ToolOutput); summary != "" {
			payload["ask_user_summary"] = summary
		}
	}
	if !suppressed && len(ev.ToolImages) > 0 {
		if imageURLs := s.toolImageURLs(ev.ToolImages); len(imageURLs) > 0 {
			payload["images"] = imageURLs
		}
	}
	if !suppressed && len(ev.ToolMedia) > 0 {
		if media := s.toolMediaEntries(ev.ToolMedia); len(media) > 0 {
			payload["media"] = media
		}
	}
	if err := run.appendEvent("response.tool_exec.end", payload); err != nil {
		return err
	}
	if suppressed {
		return nil
	}
	// Metadata only — diff content is served by the session
	// file-changes endpoints on demand.
	for _, fc := range ev.ToolFileChanges {
		if !fc.TrustedPersisted {
			if err := run.appendEvent("response.output_claim_diagnostic", map[string]any{
				"reason": "metadata_unverified", "coverage_status": "unavailable",
				"message": "unverified tool file metadata was not attributed", "tool_call_id": ev.ToolCallID,
			}); err != nil {
				return err
			}
			continue
		}
		if err := run.appendEvent("response.file_change", map[string]any{
			"path": fc.Path, "kind": fc.Kind, "adds": fc.Adds, "dels": fc.Dels,
			"seq": fc.Seq, "event_seq": fc.EventSeq, "truncated": fc.Truncated,
			"provenance": fc.Provenance, "provenances": fc.Provenances,
			"baseline_state": fc.BaselineState, "content_status": fc.ContentStatus,
			"content_available": fc.ContentAvailable, "claim_coverage": fc.ClaimCoverage,
			"tool_call_id": ev.ToolCallID,
		}); err != nil {
			return err
		}
	}
	for _, observation := range ev.ToolFilesystemObservations {
		if err := run.appendEvent("response.filesystem_observation", map[string]any{
			"id": observation.ID, "classification": observation.Classification, "root": observation.Root,
			"created_count": observation.CreatedCount, "modified_count": observation.ModifiedCount, "deleted_count": observation.DeletedCount,
			"sampled_paths": observation.SampledPaths, "samples_truncated": observation.SamplesTruncated,
			"coverage_status": observation.CoverageStatus, "event_seq": observation.EventSeq,
		}); err != nil {
			return err
		}
	}
	for _, diagnostic := range ev.ToolOutputClaimDiagnostics {
		if err := run.appendEvent("response.output_claim_diagnostic", map[string]any{
			"normalized_pattern": diagnostic.NormalizedPattern, "claim_kind": diagnostic.ClaimKind,
			"reason": diagnostic.Reason, "coverage_status": diagnostic.CoverageStatus,
			"matching_path_count": diagnostic.MatchingPathCount, "message": diagnostic.Message,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *serveServer) appendResponseHeartbeat(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	return run.appendEvent("response.heartbeat", map[string]any{
		"call_id":   ev.ToolCallID,
		"tool_name": ev.ToolName,
	})
}

func (s *serveServer) appendResponsePhase(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	if ev.Text == "" {
		return nil
	}
	return run.appendEvent("response.phase", map[string]any{
		"text": ev.Text,
	})
}

func (s *serveServer) appendResponseCompaction(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	payload := map[string]any{}
	if sequence, count, ok := runtime.takePendingCompactionIdentity(); ok {
		// The durable sequence is the stable identity shared by the persisted
		// summary and this ordered stream boundary. The web projection uses it
		// to adopt the durable row at this exact event position.
		payload["compaction_seq"] = sequence
		payload["compaction_count"] = count
	}
	return run.appendEvent("response.compaction", payload)
}

func (s *serveServer) appendResponseRetry(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	payload := map[string]any{
		"attempt":      ev.RetryAttempt,
		"max_attempts": ev.RetryMaxAttempts,
		"wait_seconds": ev.RetryWaitSecs,
	}
	if ev.Err != nil {
		payload["error"] = ev.Err.Error()
	}
	if ev.RetryMaxAttempts > 0 {
		payload["message"] = fmt.Sprintf("Model stream interrupted; reconnecting (%d/%d)…", ev.RetryAttempt, ev.RetryMaxAttempts)
	} else {
		payload["message"] = "Model stream interrupted; reconnecting…"
	}
	return run.appendEvent("response.retry", payload)
}

func (s *serveServer) appendResponseSteering(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	if strings.TrimSpace(ev.SteeringID) == "" {
		return errors.New("steering event is missing client_message_id")
	}
	// One or more committed steering form a single user boundary before
	// the next assistant text. Defer the ordinal bump until text arrives so a
	// batch of steering cannot create skipped segment identities.
	state.assistantBoundaryPending = true
	payload := map[string]any{
		"text":              ev.Text,
		"client_message_id": ev.SteeringID,
	}
	if ev.SteeringStatus != "" {
		payload["status"] = string(ev.SteeringStatus)
	}
	if atts := s.steeringAttachmentsForEvent(ev.Message); len(atts) > 0 {
		payload["attachments"] = atts
	}
	return run.appendEvent("response.steering", payload)
}

func (s *serveServer) appendResponseModelSwitch(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	fromModel := strings.TrimSpace(ev.PreviousModel)
	toModel := strings.TrimSpace(ev.Model)
	if toModel == "" {
		toModel = strings.TrimSpace(ev.Text)
	}
	boundaryID := strings.TrimSpace(ev.ModelSwitchBoundaryID)
	if fromModel == "" || toModel == "" || boundaryID == "" || !ev.ProviderTurnIndexSet {
		return fmt.Errorf("model switch event is missing authoritative runtime identity")
	}
	fromEffort := strings.TrimSpace(ev.PreviousReasoningEffort)
	toEffort := strings.TrimSpace(ev.ReasoningEffort)
	provider := runtimeProviderKey(runtime)
	marker := llm.ModelSwapMarker{
		FromProvider: provider,
		FromModel:    fromModel,
		FromEffort:   fromEffort,
		ToProvider:   provider,
		ToModel:      toModel,
		ToEffort:     toEffort,
		BoundaryID:   boundaryID,
		Status:       "succeeded",
	}
	message := llm.FormatModelSwapMarker(marker)
	if state != nil {
		state.model = toModel
		state.reasoningEffort = toEffort
		state.reasoningEffortSet = true
	}
	return run.appendEvent("response.model_switch", map[string]any{
		"model":                 toModel,
		"reasoning_effort":      toEffort,
		"from_provider":         provider,
		"from_model":            fromModel,
		"from_reasoning_effort": fromEffort,
		"to_provider":           provider,
		"to_model":              toModel,
		"to_reasoning_effort":   toEffort,
		"provider_turn_index":   ev.ProviderTurnIndex,
		"boundary_id":           boundaryID,
		"swap_status":           "succeeded",
		"message":               message,
	})
}
