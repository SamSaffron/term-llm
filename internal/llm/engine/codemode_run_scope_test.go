package engine

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestChildEngineDoesNotInheritScriptExecutionOverride(t *testing.T) {
	var parentCalls, childCalls, parentSettlements atomic.Int32
	parentLease := &nestedLease{parent: "parent-script", finish: func() { parentSettlements.Add(1) }}
	parentTool := codeTestTool{name: "parent-agent-call", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		parentCalls.Add(1)
		return core.TextOutput("wrong parent execution"), nil
	}}
	ctx := context.WithValue(context.Background(), nestedContextKey{}, parentLease)
	ctx = context.WithValue(ctx, nestedToolOverrideKey{}, core.Tool(parentTool))
	ctx = core.ContextWithCodeModeToolPause(ctx, func() (func() error, error) {
		t.Error("child used parent's permit parking hook")
		return func() error { return nil }, nil
	})
	provider := mockprovider.NewMockProvider("child")
	provider.AddToolCall("child-call", "child-leaf", map[string]any{}).AddTextResponse("done")
	e := NewEngine(provider, nil)
	if err := e.RegisterTool(codeTestTool{name: "child-leaf", run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		if nestedExecution(ctx) || nestedParent(ctx) != "" {
			t.Error("child tool retained parent script attribution")
		}
		resume, err := core.PauseCodeModeTool(ctx)
		if err != nil {
			return core.ToolOutput{}, err
		}
		if err := resume(); err != nil {
			return core.ToolOutput{}, err
		}
		childCalls.Add(1)
		return core.TextOutput("child result"), nil
	}}); err != nil {
		t.Fatal(err)
	}
	stream, err := e.Stream(ctx, core.Request{Messages: []core.Message{core.UserText("child task")}, Tools: e.tools.AllSpecs(), MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if parentCalls.Load() != 0 || parentSettlements.Load() != 0 || parentLease.started || childCalls.Load() != 1 {
		t.Fatalf("child borrowed parent execution: parent=%d settlements=%d started=%t child=%d", parentCalls.Load(), parentSettlements.Load(), parentLease.started, childCalls.Load())
	}
}
