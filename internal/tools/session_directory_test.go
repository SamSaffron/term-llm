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

func TestSessionDirectoryRequiresOriginatingTurnAuthority(t *testing.T) {
	tool := &SessionDirectoryTool{}
	calls := 0
	list := func(context.Context, SessionDirectoryQuery) ([]SessionDirectoryEntry, error) {
		calls++
		return []SessionDirectoryEntry{{ID: "sess", Number: 7, Title: "Fix reflow crash"}}, nil
	}
	bound := ContextWithSessionDirectory(context.Background(), "parent", list)
	for name, ctx := range map[string]context.Context{
		"ordinary turn": llm.ContextWithSessionID(context.Background(), "parent"),
		"no session":    bound,
		"child session": llm.ContextWithSessionID(bound, "child"),
		"foreign turn":  llm.ContextWithSessionID(ContextWithSessionDirectory(context.Background(), "other", list), "parent"),
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
	out, err := tool.Execute(ctx, json.RawMessage(`{"limit":5}`))
	if err != nil || calls != 1 || !strings.Contains(out.Content, `"session_id":"sess"`) || !strings.Contains(out.Content, `"number":7`) {
		t.Fatalf("authorized output = %+v, %v; calls=%d", out, err, calls)
	}
}

func TestSessionDirectoryDeniesMissingAndEmptyBindings(t *testing.T) {
	tool := &SessionDirectoryTool{}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	for name, candidate := range map[string]context.Context{
		"nil callback":    llm.ContextWithSessionID(ContextWithSessionDirectory(context.Background(), "parent", nil), "parent"),
		"empty session":   llm.ContextWithSessionID(ContextWithSessionDirectory(context.Background(), "", func(context.Context, SessionDirectoryQuery) ([]SessionDirectoryEntry, error) { return nil, nil }), "parent"),
		"unbound context": ctx,
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

func TestSessionDirectoryArgumentsAndFailures(t *testing.T) {
	var requested SessionDirectoryQuery
	ctx := ContextWithSessionDirectory(context.Background(), "chat", func(_ context.Context, query SessionDirectoryQuery) ([]SessionDirectoryEntry, error) {
		requested = query
		if query.ProjectID == "prj_fail" {
			return nil, errors.New("directory unavailable")
		}
		return []SessionDirectoryEntry{{ID: "ok"}}, nil
	})
	ctx = llm.ContextWithSessionID(ctx, "chat")
	tool := &SessionDirectoryTool{}
	for name, args := range map[string]string{
		"unknown field":  `{"sessions":"all"}`,
		"wrong type":     `{"query":42}`,
		"wrong bool":     `{"running_only":"yes"}`,
		"wrong archived": `{"include_archived":"yes"}`,
		"wrong limit":    `{"limit":"many"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, json.RawMessage(args))
			var toolErr *ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != ErrInvalidParams {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if requested != (SessionDirectoryQuery{}) {
		t.Fatalf("invalid arguments reached the directory: %+v", requested)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"query":" reflow ","running_only":true,"project":"prj_fail","include_archived":true,"limit":1000}`)); err == nil || !strings.Contains(err.Error(), "directory unavailable") {
		t.Fatalf("error = %v", err)
	}
	if requested.Query != "reflow" || !requested.RunningOnly || requested.ProjectID != "prj_fail" ||
		!requested.IncludeArchived || requested.Limit != sessionDirectoryQueryLimit {
		t.Fatalf("query = %+v", requested)
	}
	out, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out.Content, `"session_id":"ok"`) {
		t.Fatalf("empty arguments = %+v, %v", out, err)
	}
	if requested.Limit != sessionDirectoryDefaultLimit || requested.RunningOnly || requested.IncludeArchived {
		t.Fatalf("default query = %+v", requested)
	}
	// The agent can only reach an archived session if the schema names the switch
	// that includes it; the adapter cannot add a field it is never asked for.
	properties := tool.Spec().Schema["properties"].(map[string]any)
	if _, ok := properties["include_archived"]; !ok {
		t.Fatalf("include_archived missing from the schema: %+v", properties)
	}
	// The directory is injected only for live turns in v1, so it is deliberately
	// not a configurable registry name: configuration alone grants no access.
	if ValidToolName(SessionDirectoryToolName) {
		t.Fatal("session_directory must not be a configurable registry tool")
	}
	for _, name := range StandardToolNames() {
		if name == SessionDirectoryToolName {
			t.Fatal("the session directory must not be in the implicit CLI tool set")
		}
	}
}

func TestSessionDirectoryOutputIsBoundedAndRelative(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	entries := make([]SessionDirectoryEntry, 0, 12)
	for i := range 12 {
		entries = append(entries, SessionDirectoryEntry{
			ID: "sess-" + string(rune('a'+i)), Number: int64(i + 1), Title: strings.Repeat("标题", 100),
			Project: "Reflow", Status: "active", Snippet: strings.Repeat("snippet ", 60) + "🙂",
			Running: i%2 == 0, NeedsInput: i == 2, LastActivity: now.Add(-time.Duration(i) * time.Minute),
			MessageCount: i,
		})
	}
	body := sessionDirectoryOutput(entries, now)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !body.Truncated || len(body.Sessions) != sessionDirectoryMaxEntries {
		t.Fatalf("bounds = %+v", body)
	}
	// Count is the matched total the lookup returned, so it can report "12
	// matched, 10 shown"; overwriting it with the cap would make it the length of
	// the list next to it and convey nothing.
	if body.Count != len(entries) {
		t.Fatalf("count = %d, want the matched total %d", body.Count, len(entries))
	}
	if !strings.Contains(string(encoded), "More sessions matched than shown") {
		t.Fatalf("truncation note missing: %s", encoded)
	}
	first := body.Sessions[0]
	if first.Activity != "just now" || !first.Running {
		t.Fatalf("first entry = %+v", first)
	}
	if body.Sessions[1].Activity != "1m ago" {
		t.Fatalf("activity = %q", body.Sessions[1].Activity)
	}
	for _, entry := range body.Sessions {
		if len([]rune(entry.Snippet)) > sessionDirectoryMaxSnippetRunes+1 || !strings.HasSuffix(entry.Snippet, "…") {
			t.Fatalf("snippet not bounded: %q", entry.Snippet)
		}
		if len([]rune(entry.Title)) > sessionDirectoryMaxTitleRunes+1 {
			t.Fatalf("title not bounded: %q", entry.Title)
		}
	}
	if !body.Sessions[2].NeedsInput {
		t.Fatal("needs_input dropped")
	}

	empty := sessionDirectoryOutput(nil, now)
	if empty.Count != 0 || len(empty.Sessions) != 0 || !strings.Contains(empty.Note, "No sessions matched") {
		t.Fatalf("empty output = %+v", empty)
	}
	exact := sessionDirectoryOutput([]SessionDirectoryEntry{{ID: "one", LastActivity: time.Time{}}}, now)
	if exact.Sessions[0].Activity != sessionDirectoryUnknownActivity || exact.Note != "" {
		t.Fatalf("unknown activity = %+v", exact)
	}
	if got := sessionDirectoryActivity(now.Add(-90*time.Minute), now); got != "1h ago" {
		t.Fatalf("hours = %q", got)
	}
	if got := sessionDirectoryActivity(now.Add(-72*time.Hour), now); got != "3d ago" {
		t.Fatalf("days = %q", got)
	}
}
