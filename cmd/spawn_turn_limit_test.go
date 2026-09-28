package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

type turnLimitTestTool struct{}

func (turnLimitTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "test_tool", Description: "Test tool", Schema: map[string]any{"type": "object"}}
}
func (turnLimitTestTool) Preview(_ json.RawMessage) string { return "" }
func (turnLimitTestTool) Execute(_ context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("tool complete"), nil
}

func TestChildTurnLimitPersistsToolResultsWithMockProvider(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := &session.Session{ID: "parent", Provider: "mock", Model: "mock-model"}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	mock := llm.NewMockProvider("mock").AddToolCall("call-one", "test_tool", map[string]any{})
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
	includeTools := false
	_, err = runner.Run(ctx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "child", ParentSessionID: parent.ID, IsSubagent: true, Persist: true, Cwd: t.TempDir(), Prompt: "do work", MaxTurns: 1, MaxTurnsSet: true, IncludeConfiguredTools: &includeTools, ExtraTools: []llm.ToolSpec{(turnLimitTestTool{}).Spec()}, OnEngineReady: func(e *llm.Engine) { e.RegisterTool(turnLimitTestTool{}) }, ProviderInstance: mock}, eventSinkFunc(func(llm.Event) {}))
	var limit *llm.MaxTurnsExceededError
	if !errors.As(err, &limit) {
		t.Fatalf("run error = %v, want typed turn limit", err)
	}
	messages, err := store.GetMessages(ctx, "child", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil {
				calls[part.ToolCall.ID] = true
			}
			if part.Type == llm.PartToolResult && part.ToolResult != nil {
				results[part.ToolResult.ID] = true
			}
		}
	}
	if !calls["call-one"] || !results["call-one"] {
		t.Fatalf("turn-limit transcript lost call/result pair: %+v", messages)
	}
}
