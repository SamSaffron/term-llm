package live

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// EventKind enumerates the normalised inbound events a live provider reports.
type EventKind string

const (
	// EventSessionStarted reports that the provider accepted the session.
	EventSessionStarted EventKind = "session.started"
	// EventSessionUpdated reports a session object change.
	EventSessionUpdated EventKind = "session.updated"
	// EventUserTranscript carries a delta of the user's speech.
	EventUserTranscript EventKind = "user.transcript"
	// EventUserTranscriptInterim carries a replaceable speech-recognition preview.
	// It must never be appended to authoritative transcript or delegation context.
	EventUserTranscriptInterim EventKind = "user.transcript.interim"
	// EventAssistantTranscript carries a delta of the model's speech.
	EventAssistantTranscript EventKind = "assistant.transcript"
	// EventTurnDone reports a completed turn with its full transcript.
	EventTurnDone EventKind = "turn.done"
	// EventDelegationCreated asks the host to run real work.
	EventDelegationCreated EventKind = "delegation.created"
	// EventInterrupted reports that provider-side barge-in stopped the current
	// model audio response. PCM transports use it to flush queued playback.
	EventInterrupted EventKind = "interrupted"
	// EventError carries a provider-reported error.
	EventError EventKind = "error"
	// EventEnded reports that the provider session finished.
	EventEnded EventKind = "ended"
	// EventUnknown is any event we deliberately ignore.
	EventUnknown EventKind = "unknown"
)

// Roles used by transcript and turn events.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Channels for delegated output. Providers name these lanes differently, and
// the names collide, so ours describe behavior rather than any one wire:
//
//   - ChannelSpeakable is text the voice model should say, paraphrased. ChatGPT:
//     channel "speakable" (also its default when omitted). GPT-Live:
//     session.commentary.append.
//   - ChannelQuiet is context the voice model absorbs without speaking it when it
//     arrives, and may use in a later reply. ChatGPT: channel "commentary".
//     GPT-Live: session.thinking.append.
const (
	ChannelSpeakable = "speakable"
	ChannelQuiet     = "quiet"
)

// chatGPTQuietChannel is ChatGPT's wire name for ChannelQuiet.
const chatGPTQuietChannel = "commentary"

// MaxAppendBytes is the largest UTF-8 payload a single context append carries.
const MaxAppendBytes = 500

// Event is a provider-neutral live event.
type Event struct {
	Kind EventKind
	// RawType preserves the wire type, mainly for Unknown events.
	RawType string
	Role    string
	Text    string
	// Voice is the acknowledged output voice for EventSessionUpdated.
	Voice string
	// ErrorHandled means an operation caller already received this error. Keep
	// it on the event stream without also failing the conversational UI.
	ErrorHandled bool
	// DelegationID identifies the delegation item for EventDelegationCreated.
	DelegationID string
}

type wireContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireItem struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Target  string        `json:"target"`
	Text    string        `json:"text"`
	Content []wireContent `json:"content"`
}

type wireTurn struct {
	Role       string `json:"role"`
	Transcript string `json:"transcript"`
}

type wireSession struct {
	Audio struct {
		Output struct {
			Voice string `json:"voice"`
		} `json:"output"`
	} `json:"audio"`
}

type wireEvent struct {
	Type    string          `json:"type"`
	Item    wireItem        `json:"item"`
	Turn    wireTurn        `json:"turn"`
	Session wireSession     `json:"session"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// ParseEvent normalises one frameless protocol frame. Unrecognised frames are
// reported as EventUnknown rather than an error so new server events never
// break a running session.
func ParseEvent(data []byte) (Event, error) {
	var wire wireEvent
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
	case "input_transcript.added":
		event.Kind = EventUserTranscript
		event.Role = RoleUser
		event.Text = wire.Item.Text
	case "output_transcript.added":
		event.Kind = EventAssistantTranscript
		event.Role = RoleAssistant
		event.Text = wire.Item.Text
	case "turn.done":
		event.Kind = EventTurnDone
		event.Role = wire.Turn.Role
		event.Text = wire.Turn.Transcript
	case "delegation.created":
		if wire.Item.Type != "delegation" || wire.Item.Target != "client" {
			return event, nil
		}
		event.Kind = EventDelegationCreated
		event.DelegationID = wire.Item.ID
		event.Text = delegationInputText(wire.Item)
	case "session.closed", "session.close":
		event.Kind = EventEnded
	case "error":
		event.Kind = EventError
		event.Text = errorText(wire)
	}
	return event, nil
}

func delegationInputText(item wireItem) string {
	var b strings.Builder
	for _, part := range item.Content {
		if part.Type != "input_text" {
			continue
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func errorText(wire wireEvent) string {
	if len(wire.Error) > 0 {
		var text string
		if err := json.Unmarshal(wire.Error, &text); err == nil && strings.TrimSpace(text) != "" {
			return text
		}
		var object struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(wire.Error, &object); err == nil {
			if strings.TrimSpace(object.Message) != "" {
				return object.Message
			}
			if strings.TrimSpace(object.Code) != "" {
				return object.Code
			}
			if strings.TrimSpace(object.Type) != "" {
				return object.Type
			}
		}
	}
	if strings.TrimSpace(wire.Message) != "" {
		return wire.Message
	}
	return "live session error"
}

// Outbound is a message sent up the control channel.
type Outbound struct {
	Type             string          `json:"type"`
	DelegationItemID string          `json:"delegation_item_id,omitempty"`
	Channel          string          `json:"channel,omitempty"`
	Content          []wireContent   `json:"content,omitempty"`
	Session          json.RawMessage `json:"session,omitempty"`
}

func inputText(text string) []wireContent {
	return []wireContent{{Type: "input_text", Text: text}}
}

// DelegationContextAppend continues a delegation with more agent output.
func DelegationContextAppend(delegationID, channel, text string) Outbound {
	return Outbound{
		Type:             "delegation.context.append",
		DelegationItemID: delegationID,
		Channel:          normalizeChannel(channel),
		Content:          inputText(text),
	}
}

// SessionContextAppend injects text into the conversation outside a delegation.
func SessionContextAppend(channel, text string) Outbound {
	return Outbound{
		Type:    "session.context.append",
		Channel: normalizeChannel(channel),
		Content: inputText(text),
	}
}

// SessionUpdate replaces session settings mid-call.
func SessionUpdate(session json.RawMessage) Outbound {
	return Outbound{Type: "session.update", Session: session}
}

// SessionClose ends the session from the client side.
func SessionClose() Outbound {
	return Outbound{Type: "session.close"}
}

// normalizeChannel maps a channel to ChatGPT's wire value; anything that is not
// quiet is spoken.
func normalizeChannel(channel string) string {
	if channel == ChannelQuiet {
		return chatGPTQuietChannel
	}
	return ChannelSpeakable
}

// ChunkText splits text into pieces of at most limit UTF-8 bytes without
// splitting a rune. Empty input yields no chunks.
func ChunkText(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		return []string{text}
	}
	var chunks []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			// A single rune wider than the limit: emit it whole rather than
			// producing invalid UTF-8.
			_, size := utf8.DecodeRuneInString(text)
			cut = size
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
