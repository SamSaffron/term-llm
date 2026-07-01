package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

const RunErrorEventType = "run_error"

// RunErrorMarker is a durable non-LLM transcript event describing a failed
// response run. It is shown to users in session history but filtered from model
// context via RoleEvent.
type RunErrorMarker struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id,omitempty"`
	ErrorType  string `json:"error_type,omitempty"`
	Message    string `json:"message,omitempty"`
}

// RunErrorEventMessage returns a RoleEvent message suitable for durable
// transcript storage. It intentionally stores structured JSON in a text part so
// existing session storage schemas can persist it without migration.
func RunErrorEventMessage(marker RunErrorMarker) Message {
	marker.Type = RunErrorEventType
	if strings.TrimSpace(marker.Message) == "" {
		marker.Message = "The response failed."
	}
	data, err := json.Marshal(marker)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"type":"%s","message":"%s"}`, RunErrorEventType, strings.ReplaceAll(marker.Message, `"`, `'`)))
	}
	return Message{Role: RoleEvent, Parts: []Part{{Type: PartText, Text: string(data)}}}
}

// ParseRunErrorMarker extracts a run-error marker from a durable event message.
func ParseRunErrorMarker(msg Message) (RunErrorMarker, bool) {
	if msg.Role != RoleEvent {
		return RunErrorMarker{}, false
	}
	for _, part := range msg.Parts {
		if part.Type != PartText || strings.TrimSpace(part.Text) == "" {
			continue
		}
		var marker RunErrorMarker
		if err := json.Unmarshal([]byte(part.Text), &marker); err != nil {
			continue
		}
		if marker.Type != RunErrorEventType {
			continue
		}
		if strings.TrimSpace(marker.Message) == "" {
			marker.Message = "The response failed."
		}
		return marker, true
	}
	return RunErrorMarker{}, false
}
