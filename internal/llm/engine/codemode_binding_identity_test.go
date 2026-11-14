package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestCodeModeSameSchemaBindingReplacementRequiresNewCell(t *testing.T) {
	e := newCodeEngine()
	makeTool := func(result string) codeTestTool {
		return codeTestTool{name: "bound", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
			return core.TextOutput(result), nil
		}}
	}
	if err := e.RegisterTool(makeTool("original scope")); err != nil {
		t.Fatal(err)
	}
	cell := admitTestCell(t, e)
	if err := e.RegisterTool(makeTool("different scope")); err != nil {
		t.Fatal(err)
	}
	if _, err := cell.Call(context.Background(), "bound", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("old cell followed a same-schema replacement: %v", err)
	}
	cell.Close()
	next := admitTestCell(t, e)
	out, err := next.Call(context.Background(), "bound", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(out), "different scope") {
		t.Fatalf("new cell did not receive new binding: %s, %v", out, err)
	}
}

func TestCodeModeUnregisterAndRegisterSameInstanceRequiresNewCell(t *testing.T) {
	e := newCodeEngine()
	tool := codeTestTool{name: "scoped", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("ok"), nil
	}}
	if err := e.RegisterTool(tool); err != nil {
		t.Fatal(err)
	}
	cell := admitTestCell(t, e)
	e.UnregisterTool(tool.name)
	if err := e.RegisterTool(tool); err != nil {
		t.Fatal(err)
	}
	if _, err := cell.Describe(context.Background(), []string{tool.name}); err == nil {
		t.Fatal("old cell retained a removed and re-registered scoped binding")
	}
}

// A live target can change before a catalogue refresh re-registers the wrapper.
type liveBindingFixture struct {
	codeTestTool
	binding *atomic.Value
}

func (t liveBindingFixture) ToolBindingID() string { return t.binding.Load().(string) }

func TestCodeModeLiveBindingChangeBeforeRegistrationRefresh(t *testing.T) {
	e := newCodeEngine()
	binding := &atomic.Value{}
	binding.Store("original-target")
	tool := liveBindingFixture{codeTestTool: codeTestTool{name: "remote", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) { return core.TextOutput("effect"), nil }}, binding: binding}
	if err := e.RegisterTool(tool); err != nil {
		t.Fatal(err)
	}
	cell := admitTestCell(t, e)
	generation, _ := e.tools.BindingGeneration("remote")
	binding.Store("different-target-same-schema")
	if current, _ := e.tools.BindingGeneration("remote"); current != generation {
		t.Fatal("fixture unexpectedly changed registration")
	}
	if _, err := cell.Call(context.Background(), "remote", json.RawMessage(`{}`)); err == nil {
		t.Fatal("live target change bypassed frozen identity before catalogue refresh")
	}
}
