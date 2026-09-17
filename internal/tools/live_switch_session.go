package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// LiveSwitchSessionToolName rebinds the current live call to another chat
// session.
const LiveSwitchSessionToolName = "live_switch_session"

// LiveSessionSwitchResult reports the binding after a successful switch. The
// host resolves and validates the target; the tool never guesses.
type LiveSessionSwitchResult struct {
	SessionID   string `json:"session_id"`
	Number      int64  `json:"session_number,omitempty"`
	Title       string `json:"title,omitempty"`
	Project     string `json:"project,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	Running     bool   `json:"running,omitempty"`
	RunningTask string `json:"running_task,omitempty"`
	NoOp        bool   `json:"no_op,omitempty"`
	// LastActivity is rendered relative in the tool output, not as a timestamp.
	LastActivity time.Time `json:"-"`
}

type liveSwitchSessionKey struct{}
type liveSwitchSessionBinding struct {
	sessionID string
	activate  func(context.Context, string) (LiveSessionSwitchResult, error)
}

// ContextWithLiveSwitchSession pins switch authority to the originating chat
// turn. A registered schema alone grants no access, including to subagent
// sessions.
func ContextWithLiveSwitchSession(ctx context.Context, sessionID string, activate func(context.Context, string) (LiveSessionSwitchResult, error)) context.Context {
	return context.WithValue(ctx, liveSwitchSessionKey{}, liveSwitchSessionBinding{sessionID, activate})
}

// LiveSwitchSessionTool rebinds the live call to another existing chat session.
type LiveSwitchSessionTool struct{}

func (*LiveSwitchSessionTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: LiveSwitchSessionToolName,
		Description: "Bind the current live voice call to a different existing chat session. Supply a session number or session id from " +
			"session_directory; titles, descriptions, and partial ids are not accepted. The switch takes effect immediately: it runs in the host, " +
			"with no chat turn in flight, so the session it leaves is no longer driven by voice from that moment. If the new session already has a " +
			"task running, the next spoken request is queued as guidance to that task instead of being answered directly.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{"type": []any{"integer", "string"}, "description": "Durable session number (for example 42) or the full session id from session_directory. An unknown number or id is rejected rather than guessed."},
			},
			"required":             []any{"session"},
			"additionalProperties": false,
		},
	}
}

func (*LiveSwitchSessionTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	binding, ok := ctx.Value(liveSwitchSessionKey{}).(liveSwitchSessionBinding)
	if !ok || binding.activate == nil || binding.sessionID == "" || binding.sessionID != llm.SessionIDFromContext(ctx) {
		return llm.ToolOutput{}, NewToolError(ErrPermissionDenied, "switching sessions is available only to the originating live chat turn")
	}
	params := struct {
		Session json.RawMessage `json:"session"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return llm.ToolOutput{}, NewToolErrorf(ErrInvalidParams, "invalid live switch: %v", err)
	}
	selector, ok := liveSwitchSelector(params.Session)
	if !ok {
		return llm.ToolOutput{}, NewToolError(ErrInvalidParams, "a session number or session id is required")
	}
	result, err := binding.activate(ctx, selector)
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("live switch: %w", err)
	}
	// The running task's request is speech material, not a transcript dump.
	result.RunningTask = truncateRunes(strings.TrimSpace(result.RunningTask), sessionDirectoryMaxSnippetRunes)
	activity := ""
	if !result.LastActivity.IsZero() {
		activity = sessionDirectoryActivity(result.LastActivity, time.Now())
	}
	encoded, err := json.Marshal(liveSwitchSessionOutput{
		LiveSessionSwitchResult: result, Activity: activity, Note: liveSwitchNote(result),
	})
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("encode live switch: %w", err)
	}
	return llm.TextOutput(string(encoded)), nil
}

func (*LiveSwitchSessionTool) Preview(args json.RawMessage) string {
	params := struct {
		Session json.RawMessage `json:"session"`
	}{}
	if err := json.Unmarshal(args, &params); err != nil {
		return "Switch the live call to another session"
	}
	selector, ok := liveSwitchSelector(params.Session)
	if !ok {
		return "Switch the live call to another session"
	}
	return "Switch the live call to session " + selector
}

// liveSwitchSelector normalizes the session argument to a trimmed selector. It
// accepts a JSON string or a JSON number, because the directory reports session
// numbers as integers and a model that copies one into the call would otherwise
// fail schema validation. Everything else — booleans, null, fractional numbers,
// objects, and arrays — is rejected rather than coerced, and partial ids are not
// completed here: the host decides what matches a session.
func liveSwitchSelector(raw json.RawMessage) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		selector := strings.TrimSpace(typed)
		return selector, selector != ""
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return "", false
		}
		return strconv.FormatInt(number, 10), true
	default:
		return "", false
	}
}

type liveSwitchSessionOutput struct {
	LiveSessionSwitchResult
	Activity string `json:"activity,omitempty"`
	Note     string `json:"note,omitempty"`
}

// liveSwitchNote states the consequences the agent has to say out loud.
func liveSwitchNote(result LiveSessionSwitchResult) string {
	if result.NoOp {
		return "The live call was already bound to this session; nothing changed."
	}
	note := "The live call is bound to this session now; the session it just left is no longer driven by voice."
	if result.Running {
		note += " A task is already running there, so the next spoken request is queued as guidance to that task rather than answered directly."
	}
	if result.Archived {
		note += " That session is archived."
	}
	return note
}
