package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func responseRunBoolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func interactionKindForRecoveryEvent(pending responseRunRecoveryEvent) string {
	switch pending.Event {
	case "response.ask_user.prompt":
		return "ask_user"
	case "response.approval.prompt":
		switch {
		case responseRunBoolValue(pending.Payload["is_workspace"]):
			return "approval.workspace"
		case responseRunBoolValue(pending.Payload["is_shell"]):
			return "approval.shell"
		case responseRunBoolValue(pending.Payload["is_write"]):
			return "approval.file_write"
		default:
			return "approval"
		}
	default:
		return ""
	}
}

func (r *responseRun) interactionStateLocked() session.ResponseRunInteractionState {
	state := session.ResponseRunInteractionState{
		ResponseID:      r.id,
		OwnerInstanceID: r.ownerInstanceID,
		FencingToken:    r.fencingToken,
		Revision:        r.lastSequenceNumber,
	}
	kindSet := make(map[string]struct{})
	for _, pending := range r.recoveryEvents {
		kind := interactionKindForRecoveryEvent(pending)
		if kind == "" {
			continue
		}
		state.Count++
		kindSet[kind] = struct{}{}
		createdAt := responseRunInt64Value(pending.Payload["created_at"], 0)
		if createdAt > 0 && (state.RequiredSince.IsZero() || createdAt < state.RequiredSince.UnixMilli()) {
			state.RequiredSince = time.UnixMilli(createdAt).UTC()
		}
	}
	state.Kinds = make([]string, 0, len(kindSet))
	for kind := range kindSet {
		state.Kinds = append(state.Kinds, kind)
	}
	sort.Strings(state.Kinds)
	return state
}

func (r *responseRun) interactionState() session.ResponseRunInteractionState {
	if r == nil {
		return session.ResponseRunInteractionState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interactionStateLocked()
}

func (r *responseRun) resolvePendingInteractionsLocked(outcome string) {
	if len(r.recoveryEvents) == 0 {
		return
	}
	if r.resolvedInteractions == nil {
		r.resolvedInteractions = make(map[string]responseRunResolvedInteraction)
	}
	now := time.Now().UnixMilli()
	for _, pending := range r.recoveryEvents {
		kind, id, event, idField := "", "", "", ""
		switch pending.Event {
		case "response.approval.prompt":
			kind, id = "approval", stringValue(pending.Payload["approval_id"])
			event, idField = "response.approval.resolved", "approval_id"
		case "response.ask_user.prompt":
			kind, id = "ask_user", stringValue(pending.Payload["call_id"])
			event, idField = "response.ask_user.resolved", "call_id"
		}
		if id == "" {
			continue
		}
		key := kind + ":" + id
		if _, resolved := r.resolvedInteractions[key]; resolved {
			continue
		}
		r.resolvedInteractions[key] = responseRunResolvedInteraction{Outcome: outcome, ResolvedAt: now}
		r.lastSequenceNumber++
		payload := map[string]any{
			"response_id": r.id, "run_epoch": r.runEpoch, "sequence_number": r.lastSequenceNumber,
			idField: id, "outcome": outcome, "resolved_at": now,
		}
		data, err := json.Marshal(payload)
		if err == nil {
			r.storeEventLocked(responseRunEvent{Sequence: r.lastSequenceNumber, Event: event, Data: data}, false)
		} else {
			r.lastSequenceNumber--
		}
	}
	clear(r.recoveryEvents)
	r.recoveryEvents = nil
}

func (r *responseRun) applyRecoveryEventLocked(event string, payload map[string]any) {
	event = steeringWireEvent(nil, event)
	payload = normalizeSteeringObject(payload, true).(map[string]any)
	if event == "response.completed" || event == "response.cancelled" || event == "response.failed" {
		r.flushPendingGuardianReviewsLocked()
	}
	switch event {
	case "response.ask_user.prompt":
		r.applyRecoveryResponseAskUserPrompt(event, payload)
	case "response.approval.prompt":
		r.applyRecoveryResponseApprovalPrompt(event, payload)
	case "response.guardian.review":
		r.applyRecoveryResponseGuardianReview(event, payload)
	case "response.steering":
		r.applyRecoveryResponseSteering(event, payload)
	case "response.attempt.discard":
		r.applyRecoveryResponseAttemptDiscard(event, payload)
	case "response.output_text.delta":
		r.applyRecoveryResponseOutputTextDelta(event, payload)
	case "response.output_text.new_segment":
		r.applyRecoveryResponseOutputTextNewSegment(event, payload)
	case "response.compaction":
		r.applyRecoveryResponseCompaction(event, payload)
	case "response.model_switch":
		r.applyRecoveryResponseModelSwitch(event, payload)
	case "response.output_item.added":
		r.applyRecoveryResponseOutputItemAdded(event, payload)
	case "response.output_item.done":
		r.applyRecoveryResponseOutputItemDone(event, payload)
	case "response.tool_exec.start":
		r.applyRecoveryResponseToolExecStart(event, payload)
	case "response.tool_exec.end":
		r.applyRecoveryResponseToolExecEnd(event, payload)
	case "response.completed":
		r.applyRecoveryResponseCompleted(event, payload)
	case "response.cancelled":
		r.applyRecoveryResponseCancelled(event, payload)
	case "response.failed":
		r.applyRecoveryResponseFailed(event, payload)
	}
}
func (r *responseRun) nextRecoveryMessageIDLocked(kind string) string {
	r.nextMessageOrdinal++
	return fmt.Sprintf("%s_%s_%d", r.id, kind, r.nextMessageOrdinal)
}

func (r *responseRun) ensureAssistantMessageLocked(segmentOrdinal int) int {
	if r.currentAssistant >= 0 && r.currentAssistant < len(r.recoveryMessages) {
		current := &r.recoveryMessages[r.currentAssistant]
		if current.AssistantSegmentOrdinal == segmentOrdinal {
			return r.currentAssistant
		}
		r.currentAssistant = -1
	}
	rangeValue := r.segmentRanges[segmentOrdinal]
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:                      r.nextRecoveryMessageIDLocked("assistant"),
		Role:                    "assistant",
		Created:                 time.Now().UnixMilli(),
		ResponseID:              r.id,
		AssistantSegmentOrdinal: segmentOrdinal,
		SegmentStartSequence:    rangeValue.Start,
		SegmentEndSequence:      rangeValue.End,
	})
	r.currentAssistant = len(r.recoveryMessages) - 1
	return r.currentAssistant
}

func (r *responseRun) closeToolGroupLocked() {
	if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
		return
	}
	group := &r.recoveryMessages[r.currentToolGroup]
	if group.Role != "tool-group" {
		r.currentToolGroup = -1
		return
	}
	for i := range group.Tools {
		group.Tools[i].Status = "done"
	}
	group.Status = "done"
	r.currentToolGroup = -1
}

func (r *responseRun) subscribe(after int64) responseRunSubscribeResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	if after < r.minReplayAfter {
		return responseRunSubscribeResult{
			status:           r.status,
			snapshotRequired: true,
			minReplayAfter:   r.minReplayAfter,
		}
	}

	replayEvents := r.activeEventsLocked()
	replay := make([]responseRunEvent, 0, len(replayEvents))
	for _, ev := range replayEvents {
		if ev.Sequence > after {
			replay = append(replay, ev)
		}
	}

	if r.status != "in_progress" {
		return responseRunSubscribeResult{replay: replay, status: r.status}
	}

	id := r.nextSubscriberID
	r.nextSubscriberID++
	ch := make(chan responseRunEvent, defaultResponseRunSubscriberBuffer)
	r.subscribers[id] = ch
	return responseRunSubscribeResult{id: id, replay: replay, ch: ch, status: r.status}
}

func (r *responseRun) subscriberWasDropped(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.subscriberDropped[id] {
		return false
	}
	delete(r.subscriberDropped, id)
	return true
}

func (r *responseRun) droppedSubscriberTerminalEvent() (responseRunEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"error": map[string]any{
			"type":    "stream_buffer_overflow",
			"message": "response event stream subscriber fell behind; reconnect using the recovery payload to resume",
		},
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
		"recovery":         r.recoveryPayloadLocked(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return responseRunEvent{}, err
	}
	return responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.stream_error",
		Data:     data,
	}, nil
}

func (r *responseRun) unsubscribe(ch <-chan responseRunEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.subscribers {
		if existing == ch {
			// Terminal/buffer-overflow paths own channel closing.
			// Explicit unsubscribe only detaches the subscriber to avoid
			// coupling normal teardown to a specific close ordering.
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
			return
		}
	}
}

func (r *responseRun) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"id":                   r.id,
		"object":               "response",
		"created":              r.created,
		"started_at":           r.created * 1000,
		"model":                r.model,
		"status":               r.status,
		"session_id":           r.sessionID,
		"previous_response_id": r.previousResponseID,
		"last_sequence_number": r.lastSequenceNumber,
		"run_epoch":            r.runEpoch,
		"started_rev":          r.startedRev,
	}
	if r.clientMessageID != "" {
		payload["client_message_id"] = r.clientMessageID
	}
	if r.continuationResponseID != "" {
		payload["continuation_response_id"] = r.continuationResponseID
	}
	if r.anchorRowID > 0 {
		payload["anchor_row_id"] = r.anchorRowID
	}
	if r.status != "in_progress" {
		if r.endedAt > 0 {
			payload["ended_at"] = r.endedAt
		}
		payload["final_rev"] = r.finalRev
		payload["durable_handoff"] = r.durableHandoff
		payload["durable_output_count"] = r.durableOutputCount
		payload["handoff_compaction_seq"] = r.startedCompactionSeq
		payload["handoff_compaction_count"] = r.startedCompactionCount
		if r.durableHandoffErr != "" {
			payload["durable_handoff_error"] = r.durableHandoffErr
		}
	}
	if r.reasoningEffortSet {
		payload["reasoning_effort"] = r.reasoningEffort
	}
	if r.status == "completed" {
		payload["usage"] = usagePayload(r.usage)
		payload["session_usage"] = usagePayload(r.sessionUsage)
	}
	if r.errorMessage != "" {
		payload["error"] = map[string]any{
			"type":    r.errorType,
			"message": r.errorMessage,
		}
	}
	payload["recovery"] = r.recoveryPayloadLocked()
	return payload
}

func (r *responseRun) recoveryPayloadLocked() map[string]any {
	recovery := map[string]any{
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
	}
	if len(r.recoveryMessages) == 0 && len(r.recoveryEvents) == 0 && len(r.resolvedInteractions) == 0 {
		return recovery
	}

	messages := make([]map[string]any, 0, len(r.recoveryMessages))
	for _, msg := range r.recoveryMessages {
		responseID := msg.ResponseID
		if responseID == "" {
			responseID = r.id
		}
		entry := map[string]any{
			"id":          msg.ID,
			"role":        msg.Role,
			"created":     msg.Created,
			"responseId":  responseID,
			"response_id": responseID,
		}
		if msg.Role == "assistant" {
			entry["assistantSegmentOrdinal"] = msg.AssistantSegmentOrdinal
			entry["assistant_segment_ordinal"] = msg.AssistantSegmentOrdinal
			if msg.SegmentStartSequence > 0 {
				entry["segment_start_sequence"] = msg.SegmentStartSequence
			}
			if msg.SegmentEndSequence > 0 {
				entry["segment_end_sequence"] = msg.SegmentEndSequence
			}
		}
		if msg.Role == "compaction-boundary" {
			if msg.CompactionEventSequence > 0 {
				entry["compaction_sequence"] = msg.CompactionEventSequence
			}
			if msg.DurableCompactionSeq >= 0 {
				entry["compaction_seq"] = msg.DurableCompactionSeq
				entry["compaction_count"] = msg.CompactionCount
			}
		}
		if msg.Role == "model-swap" && msg.ModelSwap != nil {
			entry["event_sequence"] = msg.EventSequence
			entry["boundary_id"] = msg.ModelSwap.BoundaryID
			entry["from_provider"] = msg.ModelSwap.FromProvider
			entry["from_model"] = msg.ModelSwap.FromModel
			entry["from_reasoning_effort"] = msg.ModelSwap.FromEffort
			entry["to_provider"] = msg.ModelSwap.ToProvider
			entry["to_model"] = msg.ModelSwap.ToModel
			entry["to_reasoning_effort"] = msg.ModelSwap.ToEffort
			entry["swap_status"] = msg.ModelSwap.Status
			entry["swap_strategy"] = msg.ModelSwap.Strategy
		}
		if len(msg.Content) > 0 {
			entry["content"] = string(msg.Content)
		}
		if msg.Status != "" {
			entry["status"] = msg.Status
		}
		if msg.InterruptState != "" {
			entry["interruptState"] = msg.InterruptState
			entry["interrupt_state"] = msg.InterruptState
		}
		if msg.ClientMessageID != "" {
			entry["clientMessageId"] = msg.ClientMessageID
			entry["client_message_id"] = msg.ClientMessageID
		}
		if msg.Expanded {
			entry["expanded"] = msg.Expanded
		}
		if len(msg.Tools) > 0 {
			toolsPayload := make([]map[string]any, 0, len(msg.Tools))
			for _, tool := range msg.Tools {
				toolEntry := map[string]any{
					"id":      tool.ID,
					"name":    tool.Name,
					"status":  tool.Status,
					"created": tool.Created,
				}
				if tool.Arguments != "" {
					toolEntry["arguments"] = tool.Arguments
				}
				if tool.ArgumentsFinalized {
					toolEntry["argumentsFinalized"] = true
				}
				if tool.StartedAt > 0 {
					toolEntry["startedAt"] = tool.StartedAt
					durationMs := tool.DurationMs
					if durationMs == 0 && tool.EndedAt > 0 {
						durationMs = max(int64(0), tool.EndedAt-tool.StartedAt)
					} else if durationMs == 0 && tool.Status == "running" {
						durationMs = max(int64(0), time.Now().UnixMilli()-tool.StartedAt)
					}
					toolEntry["durationMs"] = durationMs
				}
				if tool.EndedAt > 0 {
					toolEntry["endedAt"] = tool.EndedAt
				}
				if tool.ResultStatus != "" {
					toolEntry["resultStatus"] = tool.ResultStatus
				}
				if tool.AskUserAnswer != "" {
					toolEntry["askUserAnswer"] = tool.AskUserAnswer
				}
				if len(tool.GuardianReviews) > 0 {
					reviews := make([]map[string]any, 0, len(tool.GuardianReviews))
					for _, review := range tool.GuardianReviews {
						reviews = append(reviews, cloneJSONMap(review))
					}
					toolEntry["guardianReviews"] = reviews
				}
				if len(tool.Images) > 0 {
					images := make([]string, len(tool.Images))
					copy(images, tool.Images)
					toolEntry["images"] = images
				}
				if len(tool.Media) > 0 {
					toolEntry["media"] = append([]webMediaEntry(nil), tool.Media...)
				}
				toolsPayload = append(toolsPayload, toolEntry)
			}
			entry["tools"] = toolsPayload
		}
		if len(msg.Attachments) > 0 {
			atts := make([]map[string]any, 0, len(msg.Attachments))
			for _, att := range msg.Attachments {
				atts = append(atts, cloneJSONMap(att))
			}
			entry["attachments"] = atts
		}
		if len(msg.Usage) > 0 {
			entry["usage"] = cloneJSONMap(msg.Usage)
		}
		messages = append(messages, entry)
	}
	recovery["messages"] = messages
	if len(r.recoveryEvents) > 0 {
		events := make([]map[string]any, 0, len(r.recoveryEvents))
		for _, ev := range r.recoveryEvents {
			entry := map[string]any{"event": ev.Event}
			if payload := cloneJSONMap(ev.Payload); len(payload) > 0 {
				entry["payload"] = payload
			}
			events = append(events, entry)
		}
		recovery["events"] = events
	}
	if len(r.resolvedInteractions) > 0 {
		resolved := make([]map[string]any, 0, len(r.resolvedInteractions))
		for key, record := range r.resolvedInteractions {
			kind, id, ok := strings.Cut(key, ":")
			if !ok || id == "" {
				continue
			}
			resolved = append(resolved, map[string]any{
				"kind": kind, "request_id": id, "outcome": record.Outcome, "resolved_at": record.ResolvedAt,
			})
		}
		sort.Slice(resolved, func(i, j int) bool {
			return responseRunInt64Value(resolved[i]["resolved_at"], 0) < responseRunInt64Value(resolved[j]["resolved_at"], 0)
		})
		if len(resolved) > 100 {
			resolved = resolved[len(resolved)-100:]
		}
		recovery["resolved_interactions"] = resolved
	}
	return recovery
}

func (r *responseRun) resolvedInteractionsSnapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved := make([]map[string]any, 0, len(r.resolvedInteractions))
	for key, record := range r.resolvedInteractions {
		kind, id, ok := strings.Cut(key, ":")
		if !ok || id == "" {
			continue
		}
		resolved = append(resolved, map[string]any{
			"kind": kind, "request_id": id, "outcome": record.Outcome, "resolved_at": record.ResolvedAt,
		})
	}
	sort.Slice(resolved, func(i, j int) bool {
		return responseRunInt64Value(resolved[i]["resolved_at"], 0) < responseRunInt64Value(resolved[j]["resolved_at"], 0)
	})
	if len(resolved) > 100 {
		resolved = resolved[len(resolved)-100:]
	}
	return resolved
}

func (r *responseRun) cancelPendingInteractions(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvePendingInteractionsLocked(outcome)
}

func (r *responseRun) requestCancel() (context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "in_progress" {
		return nil, false
	}
	cancel := r.cancel
	if cancel == nil && !r.cancelRequested {
		return nil, false
	}
	r.cancelRequested = true
	r.cancel = nil
	return cancel, true
}

func (r *responseRun) cancelRun() bool {
	cancel, ok := r.requestCancel()
	if !ok {
		return false
	}
	if cancel != nil {
		cancel()
	}
	return true
}

func responseRunRecoveryEventMatches(ev responseRunRecoveryEvent, event, key, value string) bool {
	if ev.Event != event || key == "" || value == "" {
		return false
	}
	return stringValue(ev.Payload[key]) == value
}

func (r *responseRun) resolveRecoveryEvent(event, key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recoveryEvents) == 0 {
		return
	}
	kept := r.recoveryEvents[:0]
	for _, ev := range r.recoveryEvents {
		if responseRunRecoveryEventMatches(ev, event, key, value) {
			continue
		}
		kept = append(kept, ev)
	}
	for i := len(kept); i < len(r.recoveryEvents); i++ {
		r.recoveryEvents[i] = responseRunRecoveryEvent{}
	}
	r.recoveryEvents = kept
}

func (r *responseRun) resolveAskUserRecovery(callID string) {
	r.resolveRecoveryEvent("response.ask_user.prompt", "call_id", strings.TrimSpace(callID))
}

func (r *responseRun) resolveApprovalRecovery(approvalID string) {
	r.resolveRecoveryEvent("response.approval.prompt", "approval_id", strings.TrimSpace(approvalID))
}

func (r *responseRun) hasPendingInteraction(kind, id string) bool {
	if r == nil {
		return false
	}
	id = strings.TrimSpace(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	promptEvent, idField := "response.ask_user.prompt", "call_id"
	if kind == "approval" {
		promptEvent, idField = "response.approval.prompt", "approval_id"
	}
	for _, pending := range r.recoveryEvents {
		if pending.Event == promptEvent && strings.TrimSpace(stringValue(pending.Payload[idField])) == id {
			return true
		}
	}
	return false
}

func (r *responseRun) resolvedInteraction(kind, id string) (responseRunResolvedInteraction, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.resolvedInteractions[kind+":"+strings.TrimSpace(id)]
	return value, ok
}

func (r *responseRun) recordResolvedInteraction(kind, id, outcome string) responseRunResolvedInteraction {
	id = strings.TrimSpace(id)
	key := kind + ":" + id
	r.mu.Lock()
	if existing, ok := r.resolvedInteractions[key]; ok {
		r.mu.Unlock()
		return existing
	}
	if r.resolvedInteractions == nil {
		r.resolvedInteractions = make(map[string]responseRunResolvedInteraction)
	}
	resolved := responseRunResolvedInteraction{Outcome: outcome, ResolvedAt: time.Now().UnixMilli()}
	r.resolvedInteractions[key] = resolved
	// Remove the actionable recovery prompt under the same lock as recording
	// the outcome. Terminalization uses this lock too, so it cannot overwrite a
	// successful decision while the prompt is still visible in recoveryEvents.
	promptEvent, idField := "response.ask_user.prompt", "call_id"
	if kind == "approval" {
		promptEvent, idField = "response.approval.prompt", "approval_id"
	}
	kept := r.recoveryEvents[:0]
	for _, pending := range r.recoveryEvents {
		if pending.Event == promptEvent && strings.TrimSpace(stringValue(pending.Payload[idField])) == id {
			continue
		}
		kept = append(kept, pending)
	}
	for i := len(kept); i < len(r.recoveryEvents); i++ {
		r.recoveryEvents[i] = responseRunRecoveryEvent{}
	}
	r.recoveryEvents = kept
	// Keep this bounded to the live response recovery window, evicting the
	// oldest decision rather than a random still-relevant map entry.
	if len(r.resolvedInteractions) > 128 {
		oldestKey, oldestAt := "", int64(0)
		for candidate, value := range r.resolvedInteractions {
			if candidate == key {
				continue
			}
			if oldestKey == "" || value.ResolvedAt < oldestAt {
				oldestKey, oldestAt = candidate, value.ResolvedAt
			}
		}
		delete(r.resolvedInteractions, oldestKey)
	}
	r.mu.Unlock()

	payload := map[string]any{"outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt}
	if kind == "approval" {
		payload["approval_id"] = id
	} else {
		payload["call_id"] = id
	}
	_ = r.appendEvent("response."+kind+".resolved", payload)
	return resolved
}

func (m *responseRunManager) resolvedInteractionForSession(sessionID, kind, id string) (responseRunResolvedInteraction, bool) {
	if m == nil {
		return responseRunResolvedInteraction{}, false
	}
	m.mu.Lock()
	runs := make([]*responseRun, 0, len(m.runs))
	for _, run := range m.runs {
		if run != nil && run.sessionID == sessionID {
			runs = append(runs, run)
		}
	}
	m.mu.Unlock()
	for _, run := range runs {
		if resolved, ok := run.resolvedInteraction(kind, id); ok {
			return resolved, true
		}
	}
	return responseRunResolvedInteraction{}, false
}

func (m *responseRunManager) activeRun(sessionID string) *responseRun {
	if m == nil {
		return nil
	}
	if id := m.activeRunID(sessionID); id != "" {
		if run, ok := m.get(id); ok {
			return run
		}
	}
	return nil
}

func (m *responseRunManager) latestRun(sessionID string) *responseRun {
	if m == nil {
		return nil
	}
	if run := m.activeRun(sessionID); run != nil {
		return run
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *responseRun
	for _, run := range m.runs {
		if run != nil && run.sessionID == sessionID && (latest == nil || run.created > latest.created) {
			latest = run
		}
	}
	return latest
}
