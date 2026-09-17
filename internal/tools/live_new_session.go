package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// LiveNewSessionToolName starts a new conversation and binds the live call to it.
const LiveNewSessionToolName = "live_new_session"

// LiveNewSessionRequest is the transport-free request for one new conversation.
// Both fields are optional: the host resolves an omitted one to the current
// conversation's own project and agent, so "start a new conversation" stays
// where the user is.
type LiveNewSessionRequest struct {
	Project string
	Agent   string
}

// LiveNewSessionResult reports the conversation the call drives after a
// successful creation: the number, project, and agent the voice model has to
// name out loud. The host resolves and validates everything; the tool never
// guesses, and it carries no title because a new conversation has none.
type LiveNewSessionResult struct {
	SessionID     string `json:"session_id"`
	SessionNumber int64  `json:"session_number,omitempty"`
	Project       string `json:"project,omitempty"`
	Agent         string `json:"agent,omitempty"`
}

type liveNewSessionKey struct{}
type liveNewSessionBinding struct {
	sessionID string
	create    func(context.Context, LiveNewSessionRequest) (LiveNewSessionResult, error)
}

// ContextWithLiveNewSession pins new-conversation authority to the originating
// chat turn. A registered schema alone grants no access, including to subagent
// sessions.
func ContextWithLiveNewSession(ctx context.Context, sessionID string, create func(context.Context, LiveNewSessionRequest) (LiveNewSessionResult, error)) context.Context {
	return context.WithValue(ctx, liveNewSessionKey{}, liveNewSessionBinding{sessionID, create})
}

// LiveNewSessionTool starts a brand-new conversation and moves the live call to it.
type LiveNewSessionTool struct{}

func (*LiveNewSessionTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: LiveNewSessionToolName,
		Description: "Start a brand-new, empty conversation and bind this live voice call to it. Both fields are optional: project and agent default " +
			"to the ones the current conversation uses, so omitting them keeps the user where they are. A named project or agent must be one the " +
			"user actually has — an unrecognised name is rejected with the available ones rather than guessed. The call is bound to the new " +
			"conversation immediately; use live_switch_session to move to an existing conversation instead.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "Optional project name or id to start the conversation in. Omit to stay in the current conversation's project."},
				"agent":   map[string]any{"type": "string", "description": "Optional agent name the new conversation should run. Omit to keep the current conversation's agent."},
			},
			"additionalProperties": false,
		},
	}
}

func (*LiveNewSessionTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	binding, ok := ctx.Value(liveNewSessionKey{}).(liveNewSessionBinding)
	if !ok || binding.create == nil || binding.sessionID == "" || binding.sessionID != llm.SessionIDFromContext(ctx) {
		return llm.ToolOutput{}, NewToolError(ErrPermissionDenied, "starting a new conversation is available only to the originating live chat turn")
	}
	params := struct {
		Project string `json:"project"`
		Agent   string `json:"agent"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return llm.ToolOutput{}, NewToolErrorf(ErrInvalidParams, "invalid live new session: %v", err)
	}
	result, err := binding.create(ctx, LiveNewSessionRequest{
		Project: strings.TrimSpace(params.Project), Agent: strings.TrimSpace(params.Agent),
	})
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("live new session: %w", err)
	}
	encoded, err := json.Marshal(liveNewSessionOutput{
		LiveNewSessionResult: result, Note: liveNewSessionNote(result),
	})
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("encode live new session: %w", err)
	}
	return llm.TextOutput(string(encoded)), nil
}

func (*LiveNewSessionTool) Preview(json.RawMessage) string {
	return "Start a new conversation"
}

type liveNewSessionOutput struct {
	LiveNewSessionResult
	Note string `json:"note,omitempty"`
}

// liveNewSessionNote states the consequence the agent has to say out loud. It is
// the one thing the result fields cannot carry: the conversation the call now
// drives has no history at all, so the exchange that follows starts from nothing.
func liveNewSessionNote(result LiveNewSessionResult) string {
	note := "The live call is bound to this new conversation now; it is empty, so it has no history to speak from."
	if result.SessionNumber > 0 {
		note += fmt.Sprintf(" It is session #%d.", result.SessionNumber)
	}
	return note
}
