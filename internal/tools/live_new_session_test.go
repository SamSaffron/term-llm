package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestLiveNewSessionRequiresOriginatingTurnAuthority(t *testing.T) {
	tool := &LiveNewSessionTool{}
	calls := 0
	create := func(context.Context, LiveNewSessionRequest) (LiveNewSessionResult, error) {
		calls++
		return LiveNewSessionResult{SessionID: "fresh", SessionNumber: 43, Project: "Reflow", Agent: "reviewer"}, nil
	}
	bound := ContextWithLiveNewSession(context.Background(), "parent", create)
	for name, ctx := range map[string]context.Context{
		"ordinary turn": llm.ContextWithSessionID(context.Background(), "parent"),
		"no session":    bound,
		"child session": llm.ContextWithSessionID(bound, "child"),
		"foreign turn":  llm.ContextWithSessionID(ContextWithLiveNewSession(context.Background(), "other", create), "parent"),
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
}

func TestLiveNewSessionDeniesMissingAndEmptyBindings(t *testing.T) {
	tool := &LiveNewSessionTool{}
	for name, candidate := range map[string]context.Context{
		"nil callback": llm.ContextWithSessionID(
			ContextWithLiveNewSession(context.Background(), "parent", nil), "parent"),
		"empty session": llm.ContextWithSessionID(
			ContextWithLiveNewSession(context.Background(), "", func(context.Context, LiveNewSessionRequest) (LiveNewSessionResult, error) {
				return LiveNewSessionResult{}, nil
			}), "parent"),
		"unbound context": llm.ContextWithSessionID(context.Background(), "parent"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(candidate, json.RawMessage(`{}`))
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != ErrPermissionDenied {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLiveNewSessionArgumentsAreOptionalTrimmedAndTyped(t *testing.T) {
	var seen []LiveNewSessionRequest
	ctx := llm.ContextWithSessionID(ContextWithLiveNewSession(context.Background(), "chat",
		func(_ context.Context, request LiveNewSessionRequest) (LiveNewSessionResult, error) {
			seen = append(seen, request)
			if request.Project == "missing" {
				return LiveNewSessionResult{}, errors.New(`there is no "missing" project`)
			}
			return LiveNewSessionResult{SessionID: "fresh", SessionNumber: 43}, nil
		}), "chat")
	tool := &LiveNewSessionTool{}
	// Both fields are optional, and the empty object is the common call: it is how
	// "start a new conversation" arrives when the user names neither a project nor
	// an agent.
	for name, args := range map[string]string{
		"empty":         `{}`,
		"null fields":   `{"project":null,"agent":null}`,
		"unknown field": `{"project":"Reflow","title":"scratch"}`,
		"numeric":       `{"project":42}`,
		"boolean":       `{"agent":true}`,
		"array":         `{"agent":["reviewer"]}`,
		"object":        `{"project":{"name":"Reflow"}}`,
		"malformed":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, json.RawMessage(args))
			switch name {
			case "empty", "null fields":
				if err != nil {
					t.Fatalf("optional fields were refused: %v", err)
				}
			default:
				var toolErr *ToolError
				if !errors.As(err, &toolErr) || toolErr.Type != ErrInvalidParams {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
	if len(seen) != 2 || seen[0] != (LiveNewSessionRequest{}) || seen[1] != (LiveNewSessionRequest{}) {
		t.Fatalf("requests that reached the host = %+v, want two empty requests", seen)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"project":"  Reflow  ","agent":"  Reviewer "}`)); err != nil {
		t.Fatal(err)
	}
	if want := (LiveNewSessionRequest{Project: "Reflow", Agent: "Reviewer"}); seen[2] != want {
		t.Fatalf("trimmed request = %+v, want %+v", seen[2], want)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"project":"missing"}`)); err == nil || !strings.Contains(err.Error(), `"missing" project`) {
		t.Fatalf("error = %v", err)
	}
	if preview := tool.Preview(json.RawMessage(`{"project":"Reflow"}`)); preview != "Start a new conversation" {
		t.Fatalf("preview = %q", preview)
	}
	if ValidToolName(LiveNewSessionToolName) {
		t.Fatal("live_new_session must not be a configurable registry tool")
	}
	for _, name := range StandardToolNames() {
		if name == LiveNewSessionToolName {
			t.Fatal("the new-session tool must not be in the implicit CLI tool set")
		}
	}
}

func TestLiveNewSessionOutputNamesTheConversationItStarted(t *testing.T) {
	ctx := llm.ContextWithSessionID(ContextWithLiveNewSession(context.Background(), "chat",
		func(context.Context, LiveNewSessionRequest) (LiveNewSessionResult, error) {
			return LiveNewSessionResult{SessionID: "20260917-130000-3f5c1a9b", SessionNumber: 43, Project: "Reflow", Agent: "reviewer"}, nil
		}), "chat")
	out, err := (&LiveNewSessionTool{}).Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		SessionID     string `json:"session_id"`
		SessionNumber int64  `json:"session_number"`
		Project       string `json:"project"`
		Agent         string `json:"agent"`
		Note          string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out.Content), &decoded); err != nil {
		t.Fatalf("output is not JSON: %s", out.Content)
	}
	if decoded.SessionID != "20260917-130000-3f5c1a9b" || decoded.SessionNumber != 43 ||
		decoded.Project != "Reflow" || decoded.Agent != "reviewer" {
		t.Fatalf("output = %+v", decoded)
	}
	// The note is the part the fields cannot carry: the conversation the call now
	// drives is empty, and the agent has to say so rather than summarise its
	// predecessor's work.
	for _, want := range []string{"bound to this new conversation", "empty", "#43"} {
		if !strings.Contains(decoded.Note, want) {
			t.Fatalf("note %q missing %q", decoded.Note, want)
		}
	}
	// A host that resolved no number must not have "session #0" spoken at it.
	sparse := liveNewSessionNote(LiveNewSessionResult{SessionID: "fresh"})
	if strings.Contains(sparse, "#0") || !strings.Contains(sparse, "empty") {
		t.Fatalf("sparse note = %q", sparse)
	}
}
