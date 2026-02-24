package chat

import (
	"github.com/samsaffron/term-llm/internal/ui"
)

// WireEvent is the JSON envelope sent server->client.
// Every event has a monotonic Seq for catchup replay.
type WireEvent struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`

	// session_ready
	SessionID string        `json:"session_id,omitempty"`
	Agent     string        `json:"agent,omitempty"`
	History   []HistoryItem `json:"history,omitempty"`

	// catchup
	Events []WireEvent `json:"events,omitempty"`

	// phase_change
	Phase string `json:"phase,omitempty"`

	// text_delta / reasoning_delta / error
	Text    string `json:"text,omitempty"`
	Message string `json:"message,omitempty"`

	// tool_start / tool_end
	Tool        string `json:"tool,omitempty"`
	Description string `json:"description,omitempty"`
	Success     bool   `json:"success,omitempty"`
	Summary     string `json:"summary,omitempty"`

	// message_done
	Usage *UsageInfo `json:"usage,omitempty"`

	// approval_request
	RequestID string `json:"request_id,omitempty"`
	IsWrite   bool   `json:"is_write,omitempty"`
	IsShell   bool   `json:"is_shell,omitempty"`

	// ask_user_request
	Questions []AskUserQuestion `json:"questions,omitempty"`
}

type HistoryItem struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type UsageInfo struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Cached int `json:"cached"`
}

type AskUserQuestion struct {
	Header      string          `json:"header"`
	Question    string          `json:"question"`
	Options     []AskUserOption `json:"options"`
	MultiSelect bool            `json:"multi_select"`
}

type AskUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ClientEvent is the JSON envelope sent client->server.
type ClientEvent struct {
	Type string `json:"type"`

	// message
	Text string `json:"text,omitempty"`

	// approval_response
	RequestID string `json:"request_id,omitempty"`
	Approved  bool   `json:"approved,omitempty"`

	// ask_user_response
	Responses []AskUserAnswer `json:"responses,omitempty"`
}

type AskUserAnswer struct {
	QuestionIndex int      `json:"question_index"`
	Selected      []string `json:"selected"`
}

// ToWireEvent converts a StreamEvent into a WireEvent with the supplied sequence.
func ToWireEvent(seq int64, e ui.StreamEvent) WireEvent {
	switch e.Type {
	case ui.StreamEventText:
		return WireEvent{Seq: seq, Type: "text_delta", Text: e.Text}
	case ui.StreamEventPhase:
		return WireEvent{Seq: seq, Type: "phase_change", Phase: e.Phase}
	case ui.StreamEventToolStart:
		return WireEvent{Seq: seq, Type: "tool_start", Tool: e.ToolName, Description: e.ToolInfo}
	case ui.StreamEventToolEnd:
		return WireEvent{Seq: seq, Type: "tool_end", Tool: e.ToolName, Success: e.ToolSuccess, Summary: e.ToolInfo}
	case ui.StreamEventUsage:
		return WireEvent{Seq: seq, Type: "message_done", Usage: &UsageInfo{Input: e.InputTokens, Output: e.OutputTokens, Cached: e.CachedTokens}}
	case ui.StreamEventDone:
		return WireEvent{Seq: seq, Type: "message_done", Usage: &UsageInfo{Input: e.InputTokens, Output: e.OutputTokens, Cached: e.CachedTokens}}
	case ui.StreamEventError:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		return WireEvent{Seq: seq, Type: "error", Message: msg}
	default:
		return WireEvent{Seq: seq}
	}
}
