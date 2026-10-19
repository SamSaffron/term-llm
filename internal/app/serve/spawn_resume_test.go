package serve

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestResumeAgentRepairsDanglingCallsBeforeMockProvider(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "child-session"
	if err := store.Create(ctx, &session.Session{ID: "parent", Provider: "mock", Model: "mock-model"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &session.Session{ID: id, ParentID: "parent", Provider: "mock", Model: "mock-model", IsSubagent: true}); err != nil {
		t.Fatal(err)
	}
	call := core.Message{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "finished", Name: "read_file"}},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "dangling", Name: "shell"}},
	}}
	for _, msg := range []core.Message{core.UserText("task"), call, core.ToolResultMessage("finished", "read_file", "existing result", nil)} {
		if err := store.AddMessage(ctx, id, session.NewMessage(id, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	child := &SpawnAgentRunner{store: store}
	history, err := child.loadChildResumeHistory(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("repaired history = %+v", history)
	}
	persisted, err := store.GetMessages(ctx, id, 0, 0)
	if err != nil || len(persisted) != 4 {
		t.Fatalf("persisted repair = %+v, %v", persisted, err)
	}
	if !strings.Contains(persisted[3].Parts[0].ToolResult.Content, "side effects unknown") || !persisted[3].Parts[0].ToolResult.IsError {
		t.Fatalf("synthetic repair = %+v", persisted[3])
	}
	mock := mockprovider.NewMockProvider("mock").AddTextResponse("continued")
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
	includeTools := false
	_, err = runner.Run(ctx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: id, ParentSessionID: "parent", IsSubagent: true, Persist: true, Stateful: true, Resume: true, Cwd: t.TempDir(), Messages: []core.Message{core.UserText("please continue")}, IncludeConfiguredTools: &includeTools, ProviderInstance: mock}, eventSinkFunc(func(core.Event) {}))
	if err != nil {
		t.Fatal(err)
	}
	requests := mock.RecordedRequests()
	if len(requests) == 0 {
		t.Fatal("no provider request")
	}
	hasExisting, hasSynthetic, hasInstruction := false, false, false
	for _, msg := range requests[0].Messages {
		for _, part := range msg.Parts {
			if part.Type == core.PartToolResult && part.ToolResult != nil {
				if part.ToolResult.ID == "finished" {
					hasExisting = true
				}
				if part.ToolResult.ID == "dangling" && strings.Contains(part.ToolResult.Content, "side effects unknown") {
					hasSynthetic = true
				}
			}
		}
		if msg.Role == core.RoleUser && strings.Contains(core.MessageText(msg), "please continue") {
			hasInstruction = true
		}
	}
	if !hasExisting || !hasSynthetic || !hasInstruction {
		t.Fatalf("provider lost repaired context: existing=%v synthetic=%v instruction=%v", hasExisting, hasSynthetic, hasInstruction)
	}
}
