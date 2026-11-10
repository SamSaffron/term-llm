package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

type codeTestTool struct {
	name   string
	schema map[string]interface{}
	run    func(context.Context, json.RawMessage) (core.ToolOutput, error)
}

func (t codeTestTool) Preview(json.RawMessage) string { return "" }
func (t codeTestTool) Spec() core.ToolSpec            { return core.ToolSpec{Name: t.name, Schema: t.schema} }
func (t codeTestTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return t.run(ctx, args)
}
func newCodeEngine() *Engine { return NewEngine(mockprovider.NewMockProvider("code-test"), nil) }
func admitTestCell(t *testing.T, e *Engine) *nestedCell {
	t.Helper()
	c, err := e.admitCodeCell(context.Background(), "outer", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func TestCodeModeRawBeforePresentationAndPolicy(t *testing.T) {
	e := newCodeEngine()
	e.maxToolOutputChars = 10
	raw := `{"content":[{"type":"text","text":"not JSON"}],"structuredContent":{"rows":[{"id":1,"keep":true}],"padding":"` + strings.Repeat("z", 10000) + `"}}`
	e.RegisterTool(codeTestTool{name: "awkward.tool-name", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.ToolOutput{Content: "presentation", Programmatic: json.RawMessage(raw)}, nil
	}})
	c := admitTestCell(t, e)
	r := codemode.Run(context.Background(), codemode.Input{Code: `const r=await tools['awkward.tool-name']({}); text(r.structuredContent.rows.filter(x=>x.keep).map(x=>x.id));`}, c.Names(), c, c)
	if r.Status != "completed" || len(r.Output) != 1 || string(r.Output[0].Value) != `[1]` {
		t.Fatalf("%+v", r)
	}
	if len(c.Outcomes()) != 1 {
		t.Fatal(c.Outcomes())
	}
	e.SetAllowedToolsFilter([]string{})
	if _, err := c.Call(context.Background(), "awkward.tool-name", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatal(err)
	}
	for _, name := range []string{"code_mode", "activate_skill", "ask_user", "suggest_commands", "unregistered"} {
		if _, err := c.Call(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("allowed %s", name)
		}
	}
	if _, err := e.admitCodeCell(context.Background(), "second", corestream.Sender{}); err == nil {
		t.Fatal("second cell admitted")
	}
	c.Close()
	if _, err := e.admitCodeCell(context.Background(), "empty", corestream.Sender{}); err == nil {
		t.Fatal("explicit empty allowed cell")
	}
}
func TestCodeModeFrozenSchemaAndNoWidening(t *testing.T) {
	e := newCodeEngine()
	run := func(context.Context, json.RawMessage) (core.ToolOutput, error) { return core.TextOutput("ok"), nil }
	e.RegisterTool(codeTestTool{name: "leaf", schema: map[string]interface{}{"type": "object"}, run: run})
	c := admitTestCell(t, e)
	e.RegisterTool(codeTestTool{name: "new", run: run})
	if _, err := c.Call(context.Background(), "new", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ceiling widened")
	}
	e.RegisterTool(codeTestTool{name: "leaf", schema: map[string]interface{}{"type": "array"}, run: run})
	if _, err := c.Call(context.Background(), "leaf", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "schema_changed") {
		t.Fatal(err)
	}
	e.UnregisterTool("leaf")
	if _, err := c.Call(context.Background(), "leaf", json.RawMessage(`{}`)); err == nil {
		t.Fatal("removed tool allowed")
	}
}
func TestCodeModeActualLifetimeQuarantine(t *testing.T) {
	e := newCodeEngine()
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	e.RegisterTool(codeTestTool{name: "stubborn", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		close(started)
		<-release
		defer close(finished)
		return core.TextOutput("late"), nil
	}})
	c := admitTestCell(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "stubborn", json.RawMessage(`{}`)); done <- err }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancel succeeded")
	}
	c.Close()
	if _, err := e.admitCodeCell(context.Background(), "second", corestream.Sender{}); err == nil {
		t.Fatal("ignored-context lease lost")
	}
	close(release)
	<-finished
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		e.codeModeMu.Lock()
		idle := e.codeModeCell == nil
		e.codeModeMu.Unlock()
		if idle {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("lease never released")
		default:
			runtime.Gosched()
		}
	}
	next, err := e.admitCodeCell(context.Background(), "third", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
func TestCodeModeChildConcurrencyAndEvents(t *testing.T) {
	e := newCodeEngine()
	var active, maxActive atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	e.RegisterTool(codeTestTool{name: "leaf", run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return core.ToolOutput{}, ctx.Err()
		}
		return core.TextOutput("ok"), nil
	}})
	events := make(chan core.Event, 64)
	c, err := e.admitCodeCell(context.Background(), "outer", corestream.NewSender(context.Background(), events))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 8)
	for range 8 {
		go func() { _, err := c.Call(context.Background(), "leaf", json.RawMessage(`{}`)); done <- err }()
	}
	for range 4 {
		<-started
	}
	if maxActive.Load() != 4 {
		t.Fatal(maxActive.Load())
	}
	close(release)
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() > 4 {
		t.Fatal("child limit", maxActive.Load())
	}
	close(events)
	starts, ends := 0, 0
	for ev := range events {
		if ev.ParentToolCallID != "outer" {
			t.Fatalf("missing parent %+v", ev)
		}
		switch ev.Type {
		case core.EventToolExecStart:
			starts++
		case core.EventToolExecEnd:
			ends++
			if ev.ToolOutput != "" {
				t.Fatal("raw intermediate leaked")
			}
		}
	}
	if starts != 8 || ends != 8 {
		t.Fatalf("events %d %d", starts, ends)
	}
}
func TestCodeModeProjectionIsAlwaysJSON(t *testing.T) {
	for _, budget := range []int{1, 100, 200, 1000} {
		out := projectCellOutput(core.ToolOutput{PreserveJSON: true, Content: `{"status":"completed","state_committed":true,"state":{"x":1},"output":[{"kind":"text","value":"` + strings.Repeat("x", 2000) + `"}]}`}, budget)
		if !json.Valid([]byte(out.Content)) {
			t.Fatalf("%d %s", budget, out.Content)
		}
		if budget == 1 && !out.IsError {
			t.Fatal("tiny budget claimed success")
		}
	}
}
func TestCodeModeResultLimitHonestOutcome(t *testing.T) {
	e := newCodeEngine()
	e.RegisterTool(codeTestTool{name: "large", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.ToolOutput{Programmatic: json.RawMessage(fmt.Sprintf(`{"content":[],"structuredContent":%q}`, strings.Repeat("x", codemode.Limits.RawResult)))}, nil
	}})
	c := admitTestCell(t, e)
	_, err := c.Call(context.Background(), "large", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "outcome=completed") {
		t.Fatal(err)
	}
}
