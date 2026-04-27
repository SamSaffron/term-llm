package llm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type cancelIgnoringTool struct {
	name    string
	started chan<- struct{}
	release <-chan struct{}
}

func (t *cancelIgnoringTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name}
}

func (t *cancelIgnoringTool) Execute(context.Context, json.RawMessage) (ToolOutput, error) {
	if t.started != nil {
		t.started <- struct{}{}
	}
	<-t.release
	return TextOutput("done"), nil
}

func (t *cancelIgnoringTool) Preview(json.RawMessage) string {
	return ""
}

func TestExecuteToolCallsParallelReturnsOnContextCancel(t *testing.T) {
	registry := NewToolRegistry()
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var closeRelease sync.Once
	defer closeRelease.Do(func() { close(release) })

	registry.Register(&cancelIgnoringTool{name: "blocking_tool", started: started, release: release})
	engine := NewEngine(NewMockProvider("test"), registry)
	calls := []ToolCall{
		{ID: "call-1", Name: "blocking_tool", Arguments: json.RawMessage(`{}`)},
		{ID: "call-2", Name: "blocking_tool", Arguments: json.RawMessage(`{}`)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := engine.executeToolCalls(ctx, calls, true, eventSender{}, false, false)
		done <- err
	}()

	for i := 0; i < len(calls); i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			closeRelease.Do(func() { close(release) })
			t.Fatal("timed out waiting for parallel tool execution to start")
		}
	}

	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("executeToolCalls() error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Fatalf("executeToolCalls() returned after %s, want prompt cancellation", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		closeRelease.Do(func() { close(release) })
		<-done
		t.Fatal("executeToolCalls() waited for context-ignoring parallel tools after cancellation")
	}
}

func BenchmarkExecuteToolCallsParallelPreCanceledContext(b *testing.B) {
	registry := NewToolRegistry()
	engine := NewEngine(NewMockProvider("test"), registry)
	calls := []ToolCall{
		{ID: "call-1", Name: "blocking_tool", Arguments: json.RawMessage(`{}`)},
		{ID: "call-2", Name: "blocking_tool", Arguments: json.RawMessage(`{}`)},
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		release := make(chan struct{})
		registry.Register(&cancelIgnoringTool{name: "blocking_tool", release: release})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var closeRelease sync.Once
		timer := time.AfterFunc(2*time.Millisecond, func() {
			closeRelease.Do(func() { close(release) })
		})
		_, _ = engine.executeToolCalls(ctx, calls, true, eventSender{}, false, false)
		if timer.Stop() {
			closeRelease.Do(func() { close(release) })
		}
	}
}
