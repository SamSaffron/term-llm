package live

import (
	"encoding/json"
	"fmt"
	"strings"
)

type openAIClientEvent struct {
	Type string `json:"type"`
}

type openAIConversationItemEvent struct {
	Type string                 `json:"type"`
	Item openAIConversationItem `json:"item"`
}

type openAIConversationItem struct {
	Type    string                 `json:"type"`
	Role    string                 `json:"role,omitempty"`
	Content []openAIMessageContent `json:"content,omitempty"`
	CallID  string                 `json:"call_id,omitempty"`
	Output  string                 `json:"output,omitempty"`
}

type openAIMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newOpenAIConversationItemEvent(item openAIConversationItem) openAIConversationItemEvent {
	return openAIConversationItemEvent{Type: "conversation.item.create", Item: item}
}

func openAIMessageEvent(role, text string) openAIConversationItemEvent {
	contentType := "input_text"
	if role == RoleAssistant {
		contentType = "output_text"
	}
	return newOpenAIConversationItemEvent(openAIConversationItem{
		Type: "message",
		Role: role,
		Content: []openAIMessageContent{{
			Type: contentType,
			Text: text,
		}},
	})
}

type openAIWireEvent struct {
	Type       string            `json:"type"`
	Delta      string            `json:"delta"`
	Text       string            `json:"text"`
	Transcript string            `json:"transcript"`
	CallID     string            `json:"call_id"`
	Name       string            `json:"name"`
	Arguments  string            `json:"arguments"`
	Session    openAIWireSession `json:"session"`
	Error      openAIWireError   `json:"error"`
}

type openAIWireSession struct {
	Audio struct {
		Output struct {
			Voice string `json:"voice"`
		} `json:"output"`
	} `json:"audio"`
}

type openAIWireError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type openAIDelegationArguments struct {
	Input string `json:"input"`
}

// parseOpenAIEvent translates only public GA Realtime events. It deliberately
// does not pass frames through ParseEvent, which implements ChatGPT's distinct
// frameless delegation protocol.
func parseOpenAIEvent(data []byte) (Event, error) {
	var wire openAIWireEvent
	if err := json.Unmarshal(data, &wire); err != nil {
		return Event{Kind: EventUnknown}, err
	}
	event := Event{Kind: EventUnknown, RawType: wire.Type}
	switch wire.Type {
	case "session.created":
		// Start reports this deterministically after sideband attachment because
		// attaching to an existing call need not replay session.created.
	case "session.updated":
		event.Kind = EventSessionUpdated
		event.Voice = strings.TrimSpace(wire.Session.Audio.Output.Voice)
	case "conversation.item.input_audio_transcription.delta":
		event.Kind = EventUserTranscript
		event.Role = RoleUser
		event.Text = wire.Delta
	case "conversation.item.input_audio_transcription.completed":
		event.Kind = EventTurnDone
		event.Role = RoleUser
		event.Text = wire.Transcript
	case "response.output_audio_transcript.delta", "response.output_text.delta":
		event.Kind = EventAssistantTranscript
		event.Role = RoleAssistant
		event.Text = wire.Delta
	case "response.output_audio_transcript.done", "response.output_text.done":
		event.Kind = EventTurnDone
		event.Role = RoleAssistant
		event.Text = wire.Transcript
		if event.Text == "" {
			event.Text = wire.Text
		}
		if event.Text == "" {
			event.Text = wire.Delta
		}
	case "response.function_call_arguments.done":
		if wire.Name != openAIDelegationTool {
			return event, nil
		}
		callID := strings.TrimSpace(wire.CallID)
		if callID == "" {
			return Event{Kind: EventError, RawType: wire.Type, Text: "OpenAI Realtime delegation is missing a call id"}, nil
		}
		var arguments openAIDelegationArguments
		if err := json.Unmarshal([]byte(wire.Arguments), &arguments); err != nil {
			return Event{Kind: EventError, RawType: wire.Type, Text: fmt.Sprintf("invalid OpenAI Realtime delegation arguments: %v", err)}, nil
		}
		if strings.TrimSpace(arguments.Input) == "" {
			return Event{Kind: EventError, RawType: wire.Type, Text: "OpenAI Realtime delegation input is empty"}, nil
		}
		event.Kind = EventDelegationCreated
		event.DelegationID = callID
		event.Text = arguments.Input
	case "conversation.item.input_audio_transcription.failed":
		event.Kind = EventError
		event.Text = openAIErrorText(wire.Error, "OpenAI input transcription failed")
	case "error":
		event.Kind = EventError
		event.Text = openAIErrorText(wire.Error, "OpenAI Realtime session error")
	case "session.closed":
		event.Kind = EventEnded
	}
	return event, nil
}

const (
	openAILiveInstructionsAppend = "session.instructions.append"
	openAILiveThinkingAppend     = "session.thinking.append"
	openAILiveCommentaryAppend   = "session.commentary.append"
)

type openAILiveAppendEvent struct {
	Type         string  `json:"type"`
	DelegationID *string `json:"delegation_id"`
	Content      string  `json:"content"`
}

type openAILiveWireEvent struct {
	Type       string                 `json:"type"`
	Delta      string                 `json:"delta"`
	Session    openAIWireSession      `json:"session"`
	Delegation openAILiveDelegationID `json:"delegation"`
	Error      openAIWireError        `json:"error"`
}

type openAILiveDelegationID struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Target string `json:"target"`
}

// parseOpenAILiveEvent translates the public GPT-Live event contract. Client
// delegation events intentionally carry no task text; the controller derives
// that text and its context from the transcript deltas delivered separately.
func parseOpenAILiveEvent(data []byte) (Event, error) {
	var wire openAILiveWireEvent
	if err := json.Unmarshal(data, &wire); err != nil {
		return Event{Kind: EventUnknown}, err
	}
	event := Event{Kind: EventUnknown, RawType: wire.Type}
	switch wire.Type {
	case "session.started":
		event.Kind = EventSessionStarted
	case "session.updated":
		event.Kind = EventSessionUpdated
		event.Voice = strings.TrimSpace(wire.Session.Audio.Output.Voice)
	case "session.input_transcript.delta":
		event.Kind = EventUserTranscript
		event.Role = RoleUser
		event.Text = wire.Delta
	case "session.output_transcript.delta":
		event.Kind = EventAssistantTranscript
		event.Role = RoleAssistant
		event.Text = wire.Delta
	case "session.delegation.created":
		if wire.Delegation.Type != "delegation" || wire.Delegation.Target != "client" {
			return event, nil
		}
		if strings.TrimSpace(wire.Delegation.ID) == "" {
			return Event{Kind: EventError, RawType: wire.Type, Text: "OpenAI GPT-Live delegation is missing an id"}, nil
		}
		event.Kind = EventDelegationCreated
		event.DelegationID = wire.Delegation.ID
	case "error":
		event.Kind = EventError
		event.Text = openAIErrorText(wire.Error, "OpenAI GPT-Live session error")
	case "session.closed":
		event.Kind = EventEnded
	}
	return event, nil
}

func openAIErrorText(wire openAIWireError, fallback string) string {
	if text := strings.TrimSpace(wire.Message); text != "" {
		return text
	}
	if text := strings.TrimSpace(wire.Code); text != "" {
		return text
	}
	if text := strings.TrimSpace(wire.Type); text != "" {
		return text
	}
	return fallback
}
