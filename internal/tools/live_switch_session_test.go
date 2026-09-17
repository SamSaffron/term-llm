package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestLiveSwitchSessionRequiresOriginatingTurnAuthority(t *testing.T) {
	tool := &LiveSwitchSessionTool{}
	calls := 0
	activate := func(context.Context, string) (LiveSessionSwitchResult, error) {
		calls++
		return LiveSessionSwitchResult{SessionID: "target", Number: 42, Title: "Fix reflow crash"}, nil
	}
	bound := ContextWithLiveSwitchSession(context.Background(), "parent", activate)
	for name, ctx := range map[string]context.Context{
		"ordinary turn": llm.ContextWithSessionID(context.Background(), "parent"),
		"no session":    bound,
		"child session": llm.ContextWithSessionID(bound, "child"),
		"foreign turn":  llm.ContextWithSessionID(ContextWithLiveSwitchSession(context.Background(), "other", activate), "parent"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, json.RawMessage(`{"session":"42"}`))
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
	out, err := tool.Execute(ctx, json.RawMessage(`{"session":"42"}`))
	if err != nil || calls != 1 || !strings.Contains(out.Content, `"session_id":"target"`) || !strings.Contains(out.Content, `"session_number":42`) {
		t.Fatalf("authorized output = %+v, %v; calls=%d", out, err, calls)
	}
	if !strings.Contains(out.Content, "bound to this session now") {
		t.Fatalf("missing consequence note: %s", out.Content)
	}
	if strings.Contains(out.Content, "next request") {
		t.Fatalf("switch still claims a delayed effect: %s", out.Content)
	}
}

func TestLiveSwitchSessionDeniesMissingAndEmptyBindings(t *testing.T) {
	tool := &LiveSwitchSessionTool{}
	for name, candidate := range map[string]context.Context{
		"nil callback": llm.ContextWithSessionID(
			ContextWithLiveSwitchSession(context.Background(), "parent", nil), "parent"),
		"empty session": llm.ContextWithSessionID(
			ContextWithLiveSwitchSession(context.Background(), "", func(context.Context, string) (LiveSessionSwitchResult, error) {
				return LiveSessionSwitchResult{}, nil
			}), "parent"),
		"unbound context": llm.ContextWithSessionID(context.Background(), "parent"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(candidate, json.RawMessage(`{"session":"42"}`))
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != ErrPermissionDenied {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLiveSwitchSessionArgumentsAndFailures(t *testing.T) {
	var requested string
	ctx := ContextWithLiveSwitchSession(context.Background(), "chat", func(_ context.Context, session string) (LiveSessionSwitchResult, error) {
		requested = session
		if session == "999" {
			return LiveSessionSwitchResult{}, errors.New("no chat session matches \"999\"")
		}
		return LiveSessionSwitchResult{SessionID: "target", Number: 42}, nil
	})
	ctx = llm.ContextWithSessionID(ctx, "chat")
	tool := &LiveSwitchSessionTool{}
	for name, args := range map[string]string{
		"unknown field":  `{"session":"42","force":true}`,
		"object":         `{"session":{"id":"42"}}`,
		"array":          `{"session":["42"]}`,
		"boolean":        `{"session":true}`,
		"null":           `{"session":null}`,
		"fraction":       `{"session":42.5}`,
		"exponent":       `{"session":4.2e1}`,
		"missing":        `{}`,
		"blank":          `{"session":"   "}`,
		"malformed body": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, json.RawMessage(args))
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != ErrInvalidParams {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if requested != "" {
		t.Fatalf("invalid arguments reached the host: %q", requested)
	}
	// session_directory reports session numbers as JSON integers, so a model that
	// copies one straight into the call must not be rejected, and docs/live-voice.md
	// documents {"session":42} for the same reason.
	if _, err := tool.Execute(ctx, json.RawMessage(`{"session":42}`)); err != nil || requested != "42" {
		t.Fatalf("integer selector = %q, %v", requested, err)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"session":" 42 "}`)); err != nil || requested != "42" {
		t.Fatalf("trimmed selector = %q, %v", requested, err)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"session":"999"}`)); err == nil || !strings.Contains(err.Error(), "no chat session matches") {
		t.Fatalf("error = %v", err)
	}
	if preview := tool.Preview(json.RawMessage(`{"session":42}`)); preview != "Switch the live call to session 42" {
		t.Fatalf("integer preview = %q", preview)
	}
	if preview := tool.Preview(json.RawMessage(`{"session":{}}`)); preview != "Switch the live call to another session" {
		t.Fatalf("invalid preview = %q", preview)
	}
	if ValidToolName(LiveSwitchSessionToolName) {
		t.Fatal("live_switch_session must not be a configurable registry tool")
	}
	for _, name := range StandardToolNames() {
		if name == LiveSwitchSessionToolName {
			t.Fatal("the switch tool must not be in the implicit CLI tool set")
		}
	}
}

func TestLiveSwitchSessionNotesCoverBusyArchivedAndNoOpTargets(t *testing.T) {
	tool := &LiveSwitchSessionTool{}
	for name, tc := range map[string]struct {
		result LiveSessionSwitchResult
		want   []string
	}{
		"running": {
			result: LiveSessionSwitchResult{SessionID: "t", Running: true, RunningTask: "fix the tests"},
			want:   []string{"queued as guidance"},
		},
		"archived": {
			result: LiveSessionSwitchResult{SessionID: "t", Archived: true},
			want:   []string{"archived"},
		},
		"no op": {
			result: LiveSessionSwitchResult{SessionID: "t", NoOp: true},
			want:   []string{"already bound", "nothing changed"},
		},
		"activity": {
			result: LiveSessionSwitchResult{SessionID: "t", LastActivity: time.Now().Add(-2 * time.Hour)},
			want:   []string{`"activity":"2h ago"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := llm.ContextWithSessionID(ContextWithLiveSwitchSession(context.Background(), "chat",
				func(context.Context, string) (LiveSessionSwitchResult, error) { return tc.result, nil }), "chat")
			out, err := tool.Execute(ctx, json.RawMessage(`{"session":"t"}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.Content, want) {
					t.Fatalf("output %s missing %q", out.Content, want)
				}
			}
			var decoded struct {
				Note        string `json:"note"`
				RunningTask string `json:"running_task"`
			}
			if err := json.Unmarshal([]byte(out.Content), &decoded); err != nil {
				t.Fatalf("output is not JSON: %s", out.Content)
			}
			if tc.result.RunningTask != "" && decoded.RunningTask != tc.result.RunningTask {
				t.Fatalf("running task = %q", decoded.RunningTask)
			}
		})
	}
}

func TestLiveSwitchSessionBoundsTheRunningTaskDescription(t *testing.T) {
	long := strings.Repeat("speakable ", 60)
	ctx := llm.ContextWithSessionID(ContextWithLiveSwitchSession(context.Background(), "chat",
		func(context.Context, string) (LiveSessionSwitchResult, error) {
			return LiveSessionSwitchResult{SessionID: "t", Running: true, RunningTask: long}, nil
		}), "chat")
	out, err := (&LiveSwitchSessionTool{}).Execute(ctx, json.RawMessage(`{"session":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		RunningTask string `json:"running_task"`
	}
	if err := json.Unmarshal([]byte(out.Content), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RunningTask == "" || len([]rune(decoded.RunningTask)) > sessionDirectoryMaxSnippetRunes+1 {
		t.Fatalf("running task = %q", decoded.RunningTask)
	}
}
