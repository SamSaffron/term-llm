package cmd

// This file intentionally contains a small test-only reference fold. It does
// not call responseRun.applyRecoveryEventLocked: the contract under test is
// that folding the public event stream yields the same visible structure as a
// recovery snapshot built by the server.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type responseRecoveryEventFixture struct {
	event   string
	payload map[string]any
}

type responseRecoveryToolShape struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Arguments    string `json:"arguments,omitempty"`
	Status       string `json:"status"`
	ResultStatus string `json:"result_status,omitempty"`
}

type responseRecoveryMessageShape struct {
	Role            string                      `json:"role"`
	Content         string                      `json:"content,omitempty"`
	ClientMessageID string                      `json:"client_message_id,omitempty"`
	SegmentOrdinal  int                         `json:"segment_ordinal,omitempty"`
	Status          string                      `json:"status,omitempty"`
	Tools           []responseRecoveryToolShape `json:"tools,omitempty"`
}

type responseRecoveryShape struct {
	Messages    []responseRecoveryMessageShape `json:"messages,omitempty"`
	Interactive []string                       `json:"interactive,omitempty"`
}

func closeReferenceToolGroup(shape *responseRecoveryShape, currentToolGroup *int) {
	if *currentToolGroup < 0 || *currentToolGroup >= len(shape.Messages) {
		return
	}
	group := &shape.Messages[*currentToolGroup]
	for i := range group.Tools {
		group.Tools[i].Status = "done"
	}
	group.Status = "done"
	*currentToolGroup = -1
}

func foldResponseEventStream(events []responseRunEvent) (responseRecoveryShape, error) {
	shape := responseRecoveryShape{}
	currentAssistant := -1
	currentToolGroup := -1
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return shape, err
		}
		switch event.Event {
		case "response.ask_user.prompt", "response.approval.prompt":
			shape.Interactive = append(shape.Interactive, event.Event)
		case "response.steering":
			text := stringValue(payload["text"])
			clientID := stringValue(payload["client_message_id"])
			if text == "" || clientID == "" {
				continue
			}
			closeReferenceToolGroup(&shape, &currentToolGroup)
			currentAssistant = -1
			shape.Messages = append(shape.Messages, responseRecoveryMessageShape{
				Role: "user", Content: text, ClientMessageID: clientID,
			})
		case "response.attempt.discard":
			kept := shape.Messages[:0]
			for _, message := range shape.Messages {
				if message.Role == "assistant" || message.Role == "tool-group" {
					continue
				}
				kept = append(kept, message)
			}
			shape.Messages = kept
			currentAssistant = -1
			currentToolGroup = -1
		case "response.output_text.delta":
			delta := stringValue(payload["delta"])
			if delta == "" {
				continue
			}
			closeReferenceToolGroup(&shape, &currentToolGroup)
			ordinal := responseRunIntValue(payload["assistant_segment_ordinal"], 0)
			if currentAssistant < 0 || currentAssistant >= len(shape.Messages) || shape.Messages[currentAssistant].SegmentOrdinal != ordinal {
				shape.Messages = append(shape.Messages, responseRecoveryMessageShape{Role: "assistant", SegmentOrdinal: ordinal})
				currentAssistant = len(shape.Messages) - 1
			}
			shape.Messages[currentAssistant].Content += delta
		case "response.output_text.new_segment":
			closeReferenceToolGroup(&shape, &currentToolGroup)
			currentAssistant = -1
		case "response.output_item.added":
			item := mapValue(payload["item"])
			if stringValue(item["type"]) != "function_call" {
				continue
			}
			tool := responseRecoveryToolShape{
				ID: stringValue(item["call_id"]), Name: stringValue(item["name"]),
				Arguments: stringValue(item["arguments"]), Status: "running",
			}
			if currentToolGroup < 0 || currentToolGroup >= len(shape.Messages) {
				shape.Messages = append(shape.Messages, responseRecoveryMessageShape{Role: "tool-group", Status: "running", Tools: []responseRecoveryToolShape{tool}})
				currentToolGroup = len(shape.Messages) - 1
			} else {
				shape.Messages[currentToolGroup].Tools = append(shape.Messages[currentToolGroup].Tools, tool)
				shape.Messages[currentToolGroup].Status = "running"
			}
			currentAssistant = -1
		case "response.output_item.done":
			item := mapValue(payload["item"])
			if stringValue(item["type"]) != "function_call" || currentToolGroup < 0 || currentToolGroup >= len(shape.Messages) {
				continue
			}
			callID := stringValue(item["call_id"])
			for i := range shape.Messages[currentToolGroup].Tools {
				if shape.Messages[currentToolGroup].Tools[i].ID == callID {
					shape.Messages[currentToolGroup].Tools[i].Arguments = stringValue(item["arguments"])
					break
				}
			}
		case "response.tool_exec.end":
			if currentToolGroup < 0 || currentToolGroup >= len(shape.Messages) {
				continue
			}
			callID := stringValue(payload["call_id"])
			success, ok := payload["success"].(bool)
			if !ok {
				success = true
			}
			group := &shape.Messages[currentToolGroup]
			allDone := len(group.Tools) > 0
			for i := range group.Tools {
				if callID == "" || group.Tools[i].ID == callID {
					if success {
						group.Tools[i].Status, group.Tools[i].ResultStatus = "done", "success"
					} else {
						group.Tools[i].Status, group.Tools[i].ResultStatus = "error", "error"
					}
				}
				if group.Tools[i].Status != "done" && group.Tools[i].Status != "error" {
					allDone = false
				}
			}
			if allDone {
				group.Status = "done"
			}
		case "response.completed", "response.cancelled":
			closeReferenceToolGroup(&shape, &currentToolGroup)
		case "response.failed":
			closeReferenceToolGroup(&shape, &currentToolGroup)
			message := stringValue(mapValue(payload["error"])["message"])
			if message != "" {
				shape.Messages = append(shape.Messages, responseRecoveryMessageShape{Role: "error", Content: message})
				currentAssistant = -1
			}
		}
	}
	return shape, nil
}

func normalizeResponseRecovery(recovery map[string]any) responseRecoveryShape {
	shape := responseRecoveryShape{}
	messages, _ := recovery["messages"].([]map[string]any)
	for _, message := range messages {
		entry := responseRecoveryMessageShape{
			Role: stringValue(message["role"]), Content: stringValue(message["content"]),
			ClientMessageID: stringValue(message["client_message_id"]), Status: stringValue(message["status"]),
		}
		if entry.Role == "assistant" {
			entry.SegmentOrdinal = responseRunIntValue(message["assistant_segment_ordinal"], 0)
		}
		tools, _ := message["tools"].([]map[string]any)
		for _, tool := range tools {
			entry.Tools = append(entry.Tools, responseRecoveryToolShape{
				ID: stringValue(tool["id"]), Name: stringValue(tool["name"]), Arguments: stringValue(tool["arguments"]),
				Status: stringValue(tool["status"]), ResultStatus: stringValue(tool["resultStatus"]),
			})
		}
		shape.Messages = append(shape.Messages, entry)
	}
	events, _ := recovery["events"].([]map[string]any)
	for _, event := range events {
		shape.Interactive = append(shape.Interactive, stringValue(event["event"]))
	}
	return shape
}

func TestResponseRunEventFoldStructurallyEqualsRecoveryProjection(t *testing.T) {
	text := func(ordinal int, delta string) responseRecoveryEventFixture {
		return responseRecoveryEventFixture{event: "response.output_text.delta", payload: map[string]any{"assistant_segment_ordinal": ordinal, "delta": delta}}
	}
	toolAdded := responseRecoveryEventFixture{event: "response.output_item.added", payload: map[string]any{"item": map[string]any{"type": "function_call", "call_id": "call-1", "name": "shell"}}}
	cases := []struct {
		name   string
		events []responseRecoveryEventFixture
	}{
		{name: "tools", events: []responseRecoveryEventFixture{toolAdded, {event: "response.output_item.done", payload: map[string]any{"item": map[string]any{"type": "function_call", "call_id": "call-1", "arguments": `{"command":"false"}`}}}, {event: "response.tool_exec.end", payload: map[string]any{"call_id": "call-1", "success": false}}, {event: "response.completed", payload: map[string]any{}}}},
		{name: "multiple assistant segments", events: []responseRecoveryEventFixture{text(0, "first"), {event: "response.output_text.new_segment", payload: map[string]any{}}, text(1, "second"), {event: "response.completed", payload: map[string]any{}}}},
		{name: "steering", events: []responseRecoveryEventFixture{text(0, "before"), {event: "response.steering", payload: map[string]any{"text": "clarify", "client_message_id": "client-steering"}}, text(1, "after")}},
		{name: "discarded attempt", events: []responseRecoveryEventFixture{text(0, "discard me"), toolAdded, {event: "response.attempt.discard", payload: map[string]any{}}, text(0, "replacement")}},
		{name: "ask user", events: []responseRecoveryEventFixture{{event: "response.ask_user.prompt", payload: map[string]any{"question": "continue?"}}}},
		{name: "approval", events: []responseRecoveryEventFixture{{event: "response.approval.prompt", payload: map[string]any{"call_id": "approval-1", "question": "run?"}}}},
		{name: "cancellation after partial output", events: []responseRecoveryEventFixture{text(0, "partial"), toolAdded, {event: "response.cancelled", payload: map[string]any{}}}},
		{name: "failure after partial output", events: []responseRecoveryEventFixture{text(0, "partial"), {event: "response.failed", payload: map[string]any{"error": map[string]any{"message": "provider failed"}}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newResponseRun("resp-fold-"+tc.name, "sess-fold", "", "mock", time.Now().Unix(), nil)
			for _, fixture := range tc.events {
				var err error
				if fixture.event == "response.output_text.delta" {
					ordinal := responseRunIntValue(fixture.payload["assistant_segment_ordinal"], 0)
					err = run.appendTextDeltaSegmentEvent(ordinal, ordinal, stringValue(fixture.payload["delta"]))
				} else {
					err = run.appendEvent(fixture.event, cloneJSONMap(fixture.payload))
				}
				if err != nil {
					t.Fatalf("append %s: %v", fixture.event, err)
				}
			}
			run.mu.Lock()
			events := append([]responseRunEvent(nil), run.activeEventsLocked()...)
			recovery := run.recoveryPayloadLocked()
			run.mu.Unlock()

			folded, err := foldResponseEventStream(events)
			if err != nil {
				t.Fatal(err)
			}
			got := normalizeResponseRecovery(recovery)
			foldedJSON, _ := json.Marshal(folded)
			gotJSON, _ := json.Marshal(got)
			if string(foldedJSON) != string(gotJSON) {
				t.Fatalf("event fold != recovery projection\nfolded: %s\nrecovery: %s\nevents: %s", foldedJSON, gotJSON, fmt.Sprint(tc.events))
			}
		})
	}
}
