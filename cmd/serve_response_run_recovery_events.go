package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func (r *responseRun) applyRecoveryResponseAskUserPrompt(event string, payload map[string]any) {
	r.recoveryEvents = append(r.recoveryEvents, responseRunRecoveryEvent{
		Event:   event,
		Payload: cloneJSONMap(payload),
	})
	callID := stringValue(payload["call_id"])
	if callID == "" {
		return
	}
	arguments := ""
	if encoded, err := json.Marshal(map[string]any{"questions": payload["questions"]}); err == nil {
		arguments = string(encoded)
	}
	if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
		group := &r.recoveryMessages[r.currentToolGroup]
		for i := range group.Tools {
			if group.Tools[i].ID == callID {
				if group.Tools[i].Arguments == "" {
					group.Tools[i].Arguments = arguments
				}
				group.Tools[i].ArgumentsFinalized = true
				return
			}
		}
	}
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:      r.nextRecoveryMessageIDLocked("tool_group"),
		Role:    "tool-group",
		Created: time.Now().UnixMilli(),
		Tools: []responseRunRecoveryTool{{
			ID: callID, Name: tools.AskUserToolName, Arguments: arguments,
			ArgumentsFinalized: true, Status: "running", Created: time.Now().UnixMilli(),
		}},
		Status: "running",
	})
	r.currentToolGroup = len(r.recoveryMessages) - 1
	r.currentAssistant = -1
}

func (r *responseRun) applyRecoveryResponseApprovalPrompt(event string, payload map[string]any) {
	r.recoveryEvents = append(r.recoveryEvents, responseRunRecoveryEvent{
		Event:   event,
		Payload: cloneJSONMap(payload),
	})
}

func (r *responseRun) applyRecoveryResponseGuardianReview(event string, payload map[string]any) {
	callID := stringValue(payload["tool_call_id"])
	review := cloneJSONMap(payload)
	delete(review, "response_id")
	delete(review, "run_epoch")
	delete(review, "sequence_number")
	delete(review, "tool_call_id")
	if callID != "" {
		if !r.attachGuardianReviewLocked(callID, review) {
			if r.pendingGuardianByCall == nil {
				r.pendingGuardianByCall = make(map[string][]map[string]any)
			}
			r.pendingGuardianByCall[callID] = append(r.pendingGuardianByCall[callID], review)
		}
		return
	}
	message := strings.TrimSpace(stringValue(payload["message"]))
	if message == "" {
		return
	}
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID: r.nextRecoveryMessageIDLocked("guardian_notice"), Role: "guardian-notice",
		Content: []byte(message), Created: time.Now().UnixMilli(),
	})
	return
}

func (r *responseRun) applyRecoveryResponseSteering(event string, payload map[string]any) {
	text := stringValue(payload["text"])
	attachments := attachmentsFromPayload(payload["attachments"])
	explicitID := stringValue(payload["client_message_id"])
	if text == "" && len(attachments) == 0 && explicitID == "" {
		return
	}
	r.closeToolGroupLocked()
	r.currentAssistant = -1
	id := explicitID
	if id == "" {
		id = r.nextRecoveryMessageIDLocked("user")
	}
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:              id,
		Role:            "user",
		Content:         []byte(text),
		Created:         time.Now().UnixMilli(),
		Attachments:     attachments,
		InterruptState:  "steer",
		ClientMessageID: id,
	})
	return
}

func (r *responseRun) applyRecoveryResponseAttemptDiscard(event string, payload map[string]any) {
	kept := r.recoveryMessages[:0]
	for _, message := range r.recoveryMessages {
		if message.Role == "assistant" || message.Role == "tool-group" {
			continue
		}
		kept = append(kept, message)
	}
	r.recoveryMessages = kept
	r.currentAssistant = -1
	r.currentToolGroup = -1
	clear(r.pendingGuardianByCall)
}

func (r *responseRun) applyRecoveryResponseOutputTextDelta(event string, payload map[string]any) {
	delta := stringValue(payload["delta"])
	if delta == "" {
		return
	}
	r.closeToolGroupLocked()
	ordinal := responseRunIntValue(payload["assistant_segment_ordinal"], 0)
	idx := r.ensureAssistantMessageLocked(ordinal)
	r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
	r.recoveryMessages[idx].SegmentStartSequence = responseRunInt64Value(payload["segment_start_sequence"], 0)
	r.recoveryMessages[idx].SegmentEndSequence = responseRunInt64Value(payload["sequence_number"], 0)
}

func (r *responseRun) applyRecoveryResponseOutputTextNewSegment(event string, payload map[string]any) {
	r.closeToolGroupLocked()
	r.currentAssistant = -1
}

func (r *responseRun) applyRecoveryResponseCompaction(event string, payload map[string]any) {
	r.closeToolGroupLocked()
	r.currentAssistant = -1
	sequence := responseRunInt64Value(payload["sequence_number"], 0)
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:                      fmt.Sprintf("%s_compaction_%d", r.id, sequence),
		Role:                    "compaction-boundary",
		Created:                 time.Now().UnixMilli(),
		CompactionEventSequence: sequence,
		DurableCompactionSeq:    responseRunIntValue(payload["compaction_seq"], -1),
		CompactionCount:         responseRunIntValue(payload["compaction_count"], 0),
	})
}

func (r *responseRun) applyRecoveryResponseModelSwitch(event string, payload map[string]any) {
	r.closeToolGroupLocked()
	r.currentAssistant = -1
	marker := &llm.ModelSwapMarker{
		FromProvider: strings.TrimSpace(stringValue(payload["from_provider"])),
		FromModel:    strings.TrimSpace(stringValue(payload["from_model"])),
		FromEffort:   strings.TrimSpace(stringValue(payload["from_reasoning_effort"])),
		ToProvider:   strings.TrimSpace(stringValue(payload["to_provider"])),
		ToModel:      strings.TrimSpace(stringValue(payload["to_model"])),
		ToEffort:     strings.TrimSpace(stringValue(payload["to_reasoning_effort"])),
		BoundaryID:   strings.TrimSpace(stringValue(payload["boundary_id"])),
		Status:       strings.TrimSpace(stringValue(payload["swap_status"])),
		Strategy:     strings.TrimSpace(stringValue(payload["swap_strategy"])),
	}
	if marker.Status == "" {
		marker.Status = "succeeded"
	}
	content := strings.TrimSpace(stringValue(payload["message"]))
	if content == "" && marker.FromModel != "" && marker.ToModel != "" {
		content = llm.FormatModelSwapMarker(*marker)
	}
	if content != "" && marker.FromModel != "" && marker.ToModel != "" {
		marker.DisplayText = content
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:            r.nextRecoveryMessageIDLocked("model_switch"),
			Role:          "model-swap",
			Content:       []byte(content),
			Created:       time.Now().UnixMilli(),
			ModelSwap:     marker,
			EventSequence: responseRunInt64Value(payload["sequence_number"], 0),
		})
	}
}

func (r *responseRun) applyRecoveryResponseOutputItemAdded(event string, payload map[string]any) {
	item := mapValue(payload["item"])
	if stringValue(item["type"]) != "function_call" {
		return
	}
	tool := responseRunRecoveryTool{
		ID:        stringValue(item["call_id"]),
		Name:      stringValue(item["name"]),
		Arguments: stringValue(item["arguments"]),
		Status:    "running",
		Created:   time.Now().UnixMilli(),
	}
	if tool.ID == "" {
		tool.ID = fmt.Sprintf("%s_tool_%d", r.id, len(r.recoveryMessages)+1)
	}
	if pending := r.pendingGuardianByCall[tool.ID]; len(pending) > 0 {
		for _, review := range pending {
			tool.GuardianReviews = append(tool.GuardianReviews, cloneJSONMap(review))
		}
		delete(r.pendingGuardianByCall, tool.ID)
	}
	if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:       r.nextRecoveryMessageIDLocked("tool_group"),
			Role:     "tool-group",
			Created:  time.Now().UnixMilli(),
			Tools:    []responseRunRecoveryTool{tool},
			Expanded: false,
			Status:   "running",
		})
		r.currentToolGroup = len(r.recoveryMessages) - 1
	} else {
		group := &r.recoveryMessages[r.currentToolGroup]
		group.Tools = append(group.Tools, tool)
		group.Status = "running"
	}
	r.currentAssistant = -1
}

func (r *responseRun) applyRecoveryResponseOutputItemDone(event string, payload map[string]any) {
	item := mapValue(payload["item"])
	if stringValue(item["type"]) != "function_call" || r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
		return
	}
	callID := stringValue(item["call_id"])
	name := stringValue(item["name"])
	arguments := stringValue(item["arguments"])
	group := &r.recoveryMessages[r.currentToolGroup]
	for i := range group.Tools {
		if callID != "" && group.Tools[i].ID == callID {
			group.Tools[i].Arguments = arguments
			group.Tools[i].ArgumentsFinalized = true
			return
		}
		if callID == "" && name != "" && group.Tools[i].Name == name && group.Tools[i].Status == "running" {
			group.Tools[i].Arguments = arguments
			group.Tools[i].ArgumentsFinalized = true
			return
		}
	}
}

func (r *responseRun) applyRecoveryResponseToolExecStart(event string, payload map[string]any) {
	if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
		group := &r.recoveryMessages[r.currentToolGroup]
		callID := stringValue(payload["call_id"])
		for i := range group.Tools {
			if callID == "" || group.Tools[i].ID == callID {
				if group.Tools[i].StartedAt == 0 {
					group.Tools[i].StartedAt = responseRunInt64Value(payload["started_at"], group.Tools[i].Created)
				}
				if callID != "" {
					break
				}
			}
		}
	}
}

func (r *responseRun) applyRecoveryResponseToolExecProgress(event string, payload map[string]any) {
	callID := stringValue(payload["call_id"])
	if callID == "" {
		return
	}
	for messageIndex := len(r.recoveryMessages) - 1; messageIndex >= 0; messageIndex-- {
		group := &r.recoveryMessages[messageIndex]
		if group.Role != "tool-group" {
			continue
		}
		for toolIndex := range group.Tools {
			if group.Tools[toolIndex].ID == callID {
				currentSeq := responseRunInt64Value(group.Tools[toolIndex].SubagentProgress["seq"], 0)
				if responseRunInt64Value(payload["seq"], 0) > currentSeq {
					group.Tools[toolIndex].SubagentProgress = cloneJSONMap(payload)
				}
				return
			}
		}
	}
}

func (r *responseRun) applyRecoveryResponseToolExecEnd(event string, payload map[string]any) {
	images := stringSliceValue(payload["images"])
	media := mediaEntriesFromValue(payload["media"])
	askUserAnswer := strings.TrimSpace(stringValue(payload["ask_user_summary"]))
	succeeded := true
	if value, ok := payload["success"].(bool); ok {
		succeeded = value
	}
	if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
		group := &r.recoveryMessages[r.currentToolGroup]
		callID := stringValue(payload["call_id"])
		for i := range group.Tools {
			if callID == "" || group.Tools[i].ID == callID {
				if startedAt := responseRunInt64Value(payload["started_at"], 0); startedAt > 0 && group.Tools[i].StartedAt == 0 {
					group.Tools[i].StartedAt = startedAt
				}
				group.Tools[i].EndedAt = responseRunInt64Value(payload["ended_at"], 0)
				group.Tools[i].DurationMs = responseRunInt64Value(payload["duration_ms"], 0)
				if group.Tools[i].DurationMs == 0 && group.Tools[i].StartedAt > 0 && group.Tools[i].EndedAt >= group.Tools[i].StartedAt {
					group.Tools[i].DurationMs = group.Tools[i].EndedAt - group.Tools[i].StartedAt
				}
				if succeeded {
					group.Tools[i].Status = "done"
					group.Tools[i].ResultStatus = "success"
				} else {
					group.Tools[i].Status = "error"
					group.Tools[i].ResultStatus = "error"
				}
				group.Tools[i].Images = appendUniqueStrings(group.Tools[i].Images, images...)
				group.Tools[i].Media = appendUniqueWebMedia(group.Tools[i].Media, media...)
				if succeeded && callID != "" && group.Tools[i].Name == tools.AskUserToolName && askUserAnswer != "" {
					group.Tools[i].AskUserAnswer = askUserAnswer
				}
				if callID != "" {
					break
				}
			}
		}
		allDone := len(group.Tools) > 0
		for _, tool := range group.Tools {
			if tool.Status != "done" && tool.Status != "error" {
				allDone = false
				break
			}
		}
		if allDone {
			group.Status = "done"
		}
	}
}

func (r *responseRun) applyRecoveryResponseCompleted(event string, payload map[string]any) {
	r.closeToolGroupLocked()
	response := mapValue(payload["response"])
	usage := mapValue(response["usage"])
	if len(usage) == 0 {
		return
	}
	for i := len(r.recoveryMessages) - 1; i >= 0; i-- {
		if r.recoveryMessages[i].Role == "assistant" {
			r.recoveryMessages[i].Usage = cloneJSONMap(usage)
			return
		}
	}
}

func (r *responseRun) applyRecoveryResponseCancelled(event string, payload map[string]any) {
	r.closeToolGroupLocked()
}

func (r *responseRun) applyRecoveryResponseFailed(event string, payload map[string]any) {
	r.closeToolGroupLocked()
	errPayload := mapValue(payload["error"])
	message := stringValue(errPayload["message"])
	if message == "" {
		return
	}
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:      r.nextRecoveryMessageIDLocked("error"),
		Role:    "error",
		Content: []byte(message),
		Created: time.Now().UnixMilli(),
	})
	r.currentAssistant = -1
}
