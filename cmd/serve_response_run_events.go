package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

func (r *responseRun) complete(payload map[string]any, usage llm.Usage, sessionUsage llm.Usage) error {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	if r.cancelRequested {
		r.status = "cancelled"
		r.endedAt = time.Now().UnixMilli()
		r.errorType = ""
		r.errorMessage = ""
		r.cancel = nil
		r.cancelRequested = false
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["status"] = "cancelled"
			delete(response, "usage")
			delete(response, "session_usage")
			delete(response, "context_usage")
		}
		if err := r.finalizeLifecycleLocked(session.ResponseRunCancelled); err != nil {
			return r.appendLifecycleFailureLocked(payload, err)
		}
		return r.appendEventLocked("response.cancelled", payload, true)
	}
	r.endedAt = time.Now().UnixMilli()
	r.cancel = nil
	r.cancelRequested = false
	if !handoff.Valid {
		r.status = "failed"
		r.errorType = "server_error"
		r.errorMessage = "response persistence could not be durably verified"
		if handoff.Error != "" {
			r.errorMessage += ": " + handoff.Error
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["status"] = "failed"
			response["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
			delete(response, "usage")
			delete(response, "session_usage")
			delete(response, "context_usage")
		}
		if err := r.finalizeLifecycleLocked(session.ResponseRunFailed); err != nil {
			return r.appendLifecycleFailureLocked(payload, err)
		}
		return r.appendEventLocked("response.failed", payload, true)
	}
	r.status = "completed"
	r.errorType = ""
	r.errorMessage = ""
	r.usage = usage
	r.sessionUsage = sessionUsage
	if err := r.finalizeLifecycleLocked(session.ResponseRunCompleted); err != nil {
		return r.appendLifecycleFailureLocked(payload, err)
	}
	return r.appendEventLocked("response.completed", payload, true)
}

func (r *responseRun) finishCancelled(payload map[string]any) (bool, error) {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	r.mu.Lock()
	if !r.cancelRequested {
		r.mu.Unlock()
		return false, nil
	}
	r.mu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cancelRequested {
		return false, nil
	}
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	r.status = "cancelled"
	r.endedAt = time.Now().UnixMilli()
	r.errorType = ""
	r.errorMessage = ""
	r.cancel = nil
	r.cancelRequested = false
	if err := r.finalizeLifecycleLocked(session.ResponseRunCancelled); err != nil {
		return true, r.appendLifecycleFailureLocked(payload, err)
	}
	return true, r.appendEventLocked("response.cancelled", payload, true)
}

func (r *responseRun) fail(payload map[string]any, errType, errMessage string) (bool, error) {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	hadSubscribers := len(r.subscribers) > 0
	r.status = "failed"
	r.endedAt = time.Now().UnixMilli()
	r.errorType = errType
	r.errorMessage = errMessage
	r.cancel = nil
	r.cancelRequested = false
	if err := r.finalizeLifecycleLocked(session.ResponseRunFailed); err != nil {
		return hadSubscribers, r.appendLifecycleFailureLocked(payload, err)
	}
	return hadSubscribers, r.appendEventLocked("response.failed", payload, true)
}

func (r *responseRun) applyRuntimeMetadataLocked(event string, payload map[string]any) {
	var source map[string]any
	switch event {
	case "response.created", "response.completed", "response.cancelled":
		source = mapValue(payload["response"])
	case "response.model_switch":
		source = payload
	default:
		return
	}
	if len(source) == 0 {
		return
	}
	if model := stringValue(source["model"]); model != "" {
		r.model = model
	}
	if _, ok := source["reasoning_effort"]; ok {
		r.reasoningEffort = stringValue(source["reasoning_effort"])
		r.reasoningEffortSet = true
	}
}

func (r *responseRun) appendEventLocked(event string, payload map[string]any, terminal bool) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["response_id"] = r.id
	payload["run_epoch"] = r.runEpoch
	if event == "response.created" {
		payload["started_rev"] = r.startedRev
		payload["started_at"] = r.created * 1000
		if r.clientMessageID != "" {
			payload["client_message_id"] = r.clientMessageID
		}
		if r.anchorRowID > 0 {
			payload["anchor_row_id"] = r.anchorRowID
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["started_rev"] = r.startedRev
			response["run_epoch"] = r.runEpoch
			if r.clientMessageID != "" {
				response["client_message_id"] = r.clientMessageID
			}
			if r.anchorRowID > 0 {
				response["anchor_row_id"] = r.anchorRowID
			}
		}
	}
	if terminal {
		payload["ended_at"] = r.endedAt
		payload["final_rev"] = r.finalRev
		payload["durable_handoff"] = r.durableHandoff
		payload["durable_output_count"] = r.durableOutputCount
		if r.attentionSeq > 0 {
			payload["attention_seq"] = r.attentionSeq
			payload["attention_final_rev"] = r.finalRev
			payload["attention_response_id"] = r.id
			payload["attention_store_instance_id"] = r.attentionStoreID
		}
		payload["handoff_compaction_seq"] = r.startedCompactionSeq
		payload["handoff_compaction_count"] = r.startedCompactionCount
		if r.durableHandoffErr != "" {
			payload["durable_handoff_error"] = r.durableHandoffErr
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["ended_at"] = r.endedAt
			response["final_rev"] = r.finalRev
			response["durable_handoff"] = r.durableHandoff
			response["durable_output_count"] = r.durableOutputCount
			if r.attentionSeq > 0 {
				response["attention_seq"] = r.attentionSeq
				response["attention_final_rev"] = r.finalRev
				response["attention_response_id"] = r.id
				response["attention_store_instance_id"] = r.attentionStoreID
			}
			response["handoff_compaction_seq"] = r.startedCompactionSeq
			response["handoff_compaction_count"] = r.startedCompactionCount
			if r.durableHandoffErr != "" {
				response["durable_handoff_error"] = r.durableHandoffErr
			}
		}
	}
	if terminal {
		outcome := "cancelled-by-agent"
		if event == "response.failed" {
			outcome = "failed"
		}
		r.resolvePendingInteractionsLocked(outcome)
	}
	r.lastSequenceNumber++
	payload["sequence_number"] = r.lastSequenceNumber
	r.applyRuntimeMetadataLocked(event, payload)

	data, err := json.Marshal(payload)
	if err != nil {
		r.lastSequenceNumber--
		delete(payload, "sequence_number")
		return err
	}

	r.applyRecoveryEventLocked(event, payload)
	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    event,
		Data:     data,
	}, terminal)
	if r.interactionStateChanged != nil {
		switch event {
		case "response.ask_user.prompt", "response.ask_user.resolved",
			"response.approval.prompt", "response.approval.resolved":
			r.interactionStateChanged(r.interactionStateLocked())
		}
	}
	if r.coarseEvent != nil {
		r.coarseEvent(event, payload)
	}
	if terminal && r.terminalNotify != nil && (event == "response.completed" || event == "response.failed") {
		outcome := "completed"
		if event == "response.failed" {
			outcome = "failed"
		}
		r.terminalNotifyOnce.Do(func() {
			_ = restart.Default.Go(context.Background(), func(context.Context) { r.terminalNotify(outcome) })
		})
	}
	return nil
}

// storeEventLocked appends stored to r.events, compacts the buffer, and fans
// out to all live subscribers. Must be called with r.mu held.
func (r *responseRun) storeEventLocked(stored responseRunEvent, terminal bool) {
	r.events = append(r.events, stored)
	r.compactEventsLocked()

	// Fan out to subscribers under the lock to guarantee event ordering.
	// Non-blocking send: the 256-event buffer provides ample headroom.
	// A subscriber that can't accept is truly stalled and gets dropped immediately.
	for id, ch := range r.subscribers {
		select {
		case ch <- stored:
			fill := len(ch)
			threshold := cap(ch) * 3 / 4
			if fill > threshold && !r.subscriberWarned[id] {
				log.Printf("response run %s subscriber %d buffer at %d/%d", r.id, id, fill, cap(ch))
				r.subscriberWarned[id] = true
			} else if fill <= threshold/2 && r.subscriberWarned[id] {
				r.subscriberWarned[id] = false
			}
		default:
			log.Printf("response run %s subscriber fell behind at sequence %d; closing stream", r.id, stored.Sequence)
			r.subscriberDropped[id] = true
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}

	if terminal {
		for id, ch := range r.subscribers {
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}
}

func responseRunInt64Value(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
	}
	return fallback
}

func responseRunIntValue(value any, fallback int) int {
	return int(responseRunInt64Value(value, int64(fallback)))
}

func encodeTextDeltaPayloadWithIdentity(responseID string, runEpoch int64, outputIndex, segmentOrdinal int, segmentStartSequence int64, delta string, sequenceNumber int64) ([]byte, error) {
	data := make([]byte, 0, 160+len(responseID)+len(delta))
	data = append(data, `{"response_id":`...)
	data = appendJSONString(data, responseID)
	data = append(data, `,"run_epoch":`...)
	data = strconv.AppendInt(data, runEpoch, 10)
	data = append(data, `,"assistant_segment_ordinal":`...)
	data = strconv.AppendInt(data, int64(segmentOrdinal), 10)
	data = append(data, `,"segment_start_sequence":`...)
	data = strconv.AppendInt(data, segmentStartSequence, 10)
	data = append(data, `,"output_index":`...)
	data = strconv.AppendInt(data, int64(outputIndex), 10)
	data = append(data, `,"delta":`...)
	if utf8.ValidString(delta) {
		data = appendJSONString(data, delta)
	} else {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return nil, err
		}
		data = append(data, encoded...)
	}
	data = append(data, `,"sequence_number":`...)
	data = strconv.AppendInt(data, sequenceNumber, 10)
	data = append(data, '}')
	return data, nil
}

// appendTextDeltaSegmentEvent is a fast path for response.output_text.delta that avoids
// allocating a map[string]any or a typed payload on every streamed token.
func (r *responseRun) appendTextDeltaSegmentEvent(outputIndex, segmentOrdinal int, delta string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSequenceNumber++
	sequenceNumber := r.lastSequenceNumber
	rangeValue, hadRange := r.segmentRanges[segmentOrdinal]
	previousRange := rangeValue
	if rangeValue.Start == 0 {
		rangeValue.Start = sequenceNumber
	}
	rangeValue.End = sequenceNumber
	r.segmentRanges[segmentOrdinal] = rangeValue

	data, err := encodeTextDeltaPayloadWithIdentity(r.id, r.runEpoch, outputIndex, segmentOrdinal, rangeValue.Start, delta, sequenceNumber)
	if err != nil {
		r.lastSequenceNumber--
		if hadRange {
			r.segmentRanges[segmentOrdinal] = previousRange
		} else {
			delete(r.segmentRanges, segmentOrdinal)
		}
		return err
	}

	if delta != "" {
		r.closeToolGroupLocked()
		idx := r.ensureAssistantMessageLocked(segmentOrdinal)
		r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
		r.recoveryMessages[idx].SegmentStartSequence = rangeValue.Start
		r.recoveryMessages[idx].SegmentEndSequence = rangeValue.End
	}

	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.output_text.delta",
		Data:     data,
	}, false)
	return nil
}

func (r *responseRun) compactEventsLocked() {
	if !r.compactionEnabled || r.maxRetainedEvents <= 0 {
		return
	}

	activeLen := len(r.events) - r.eventStart
	if activeLen <= r.maxRetainedEvents {
		return
	}

	dropCount := activeLen - r.maxRetainedEvents
	firstKept := r.eventStart + dropCount

	nextReplayAfter := r.events[firstKept].Sequence - 1
	if nextReplayAfter > r.minReplayAfter {
		r.minReplayAfter = nextReplayAfter
	}

	for i := r.eventStart; i < firstKept; i++ {
		r.events[i] = responseRunEvent{}
	}
	r.eventStart = firstKept
	r.compactEventStorageLocked()
}

// compactEventStorageLocked reclaims the dropped prefix in batches so steady
// streaming appends avoid copying the replay window on every token while still
// keeping the backing array bounded to roughly twice maxRetainedEvents.
func (r *responseRun) compactEventStorageLocked() {
	if r.eventStart == 0 {
		return
	}
	if r.maxRetainedEvents > 0 && r.eventStart < r.maxRetainedEvents {
		return
	}

	activeLen := len(r.events) - r.eventStart
	copy(r.events, r.events[r.eventStart:])
	tail := r.events[activeLen:]
	for i := range tail {
		tail[i] = responseRunEvent{}
	}
	r.events = r.events[:activeLen]
	r.eventStart = 0
}

func (r *responseRun) activeEventsLocked() []responseRunEvent {
	return r.events[r.eventStart:]
}

func (r *responseRun) attachGuardianReviewLocked(callID string, review map[string]any) bool {
	for messageIndex := range r.recoveryMessages {
		group := &r.recoveryMessages[messageIndex]
		if group.Role != "tool-group" {
			continue
		}
		for toolIndex := range group.Tools {
			if group.Tools[toolIndex].ID == callID {
				group.Tools[toolIndex].GuardianReviews = append(group.Tools[toolIndex].GuardianReviews, cloneJSONMap(review))
				return true
			}
		}
	}
	return false
}

func (r *responseRun) flushPendingGuardianReviewsLocked() {
	callIDs := make([]string, 0, len(r.pendingGuardianByCall))
	for callID := range r.pendingGuardianByCall {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	for _, callID := range callIDs {
		for _, review := range r.pendingGuardianByCall[callID] {
			message := strings.TrimSpace(stringValue(review["message"]))
			if message == "" {
				outcome := strings.TrimSpace(stringValue(review["outcome"]))
				if outcome == "" {
					outcome = "warning"
				}
				message = fmt.Sprintf("Guardian %s review", outcome)
			}
			message = fmt.Sprintf("%s (unmatched tool call %s)", message, callID)
			r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
				ID: r.nextRecoveryMessageIDLocked("guardian_notice"), Role: "guardian-notice",
				Content: []byte(message), Created: time.Now().UnixMilli(),
			})
		}
		delete(r.pendingGuardianByCall, callID)
	}
}
