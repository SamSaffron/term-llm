package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
)

// LiveSettingsToolName controls only the current live call, never saved config.
const LiveSettingsToolName = "live_settings"

type liveSettingsKey struct{}
type liveSettingsBinding struct {
	sessionID string
	apply     func(context.Context, string) (live.Capabilities, error)
}

// ContextWithLiveSettings pins authority to the originating chat and call. A
// registered schema alone grants no access, including to subagent sessions.
func ContextWithLiveSettings(ctx context.Context, sessionID string, apply func(context.Context, string) (live.Capabilities, error)) context.Context {
	return context.WithValue(ctx, liveSettingsKey{}, liveSettingsBinding{sessionID, apply})
}

// LiveSettingsTool exposes a host-bound, session-local voice control.
type LiveSettingsTool struct{}

func (*LiveSettingsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        LiveSettingsToolName,
		Description: "Inspect this term-llm live call's model, current voice and supported voices. Supply voice to request a change for this call only. Provider restrictions may prevent changes after speech starts; report errors truthfully. Never edits saved configuration.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"voice": map[string]any{"type": "string", "description": "Optional supported voice to select for the current call. Omit to inspect settings."}}, "additionalProperties": false},
	}
}

func (*LiveSettingsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	binding, ok := ctx.Value(liveSettingsKey{}).(liveSettingsBinding)
	if !ok || binding.apply == nil || binding.sessionID == "" || binding.sessionID != llm.SessionIDFromContext(ctx) {
		return llm.ToolOutput{}, NewToolError(ErrPermissionDenied, "live settings are available only to the originating live chat turn")
	}
	var params struct {
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return llm.ToolOutput{}, NewToolErrorf(ErrInvalidParams, "invalid live settings: %v", err)
	}
	capabilities, err := binding.apply(ctx, params.Voice)
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("live settings: %w", err)
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("encode live settings: %w", err)
	}
	return llm.TextOutput(string(encoded)), nil
}

func (*LiveSettingsTool) Preview(json.RawMessage) string {
	return "Inspect or change the current live call's voice"
}
