package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestLiveSettingsRequiresOriginatingTurnAuthority(t *testing.T) {
	tool := &LiveSettingsTool{}
	calls := 0
	apply := func(context.Context, string) (live.Capabilities, error) {
		calls++
		return live.Capabilities{Voice: "cove"}, nil
	}
	bound := ContextWithLiveSettings(context.Background(), "parent", apply)
	for name, ctx := range map[string]context.Context{
		"ordinary turn": llm.ContextWithSessionID(context.Background(), "parent"),
		"no session":    bound,
		"child session": llm.ContextWithSessionID(bound, "child"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, json.RawMessage(`{}`))
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != ErrPermissionDenied {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("unauthorized callbacks = %d", calls)
	}
	ctx := llm.ContextWithSessionID(bound, "parent")
	out, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out.Content, `"voice":"cove"`) || calls != 1 {
		t.Fatalf("authorized output = %+v, %v; calls=%d", out, err, calls)
	}
}

func TestLiveSettingsArgumentsAndProviderFailures(t *testing.T) {
	var requested string
	ctx := ContextWithLiveSettings(context.Background(), "chat", func(_ context.Context, voice string) (live.Capabilities, error) {
		requested = voice
		return live.Capabilities{}, errors.New("provider refused the change")
	})
	ctx = llm.ContextWithSessionID(ctx, "chat")
	tool := &LiveSettingsTool{}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"voice":42}`)); err == nil {
		t.Fatal("invalid arguments accepted")
	}
	if requested != "" {
		t.Fatal("invalid request reached provider")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"voice":"maple"}`)); err == nil || !strings.Contains(err.Error(), "provider refused") {
		t.Fatalf("error = %v", err)
	}
	if requested != "maple" {
		t.Fatalf("voice = %q", requested)
	}
	if !ValidToolName(LiveSettingsToolName) || GetToolKind(LiveSettingsToolName) != KindSessionState {
		t.Fatal("missing session-state tool classification")
	}
	for _, name := range StandardToolNames() {
		if name == LiveSettingsToolName {
			t.Fatal("live settings must not be in the implicit CLI tool set")
		}
	}
}
