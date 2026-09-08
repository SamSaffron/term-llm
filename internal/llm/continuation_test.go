package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

type continuationProvider struct{ calls atomic.Int32 }

func (*continuationProvider) Name() string               { return "checkpoint-fixture" }
func (*continuationProvider) Credential() string         { return "" }
func (*continuationProvider) Capabilities() Capabilities { return Capabilities{ToolCalls: true} }
func (p *continuationProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.calls.Add(1)
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		if lastMessageHasToolResults(req.Messages) {
			return send.Send(Event{Type: EventTextDelta, Text: "finished"})
		}
		if err := send.Send(Event{Type: EventTextDelta, Text: "Hello"}); err != nil {
			return err
		}
		return send.Send(Event{Type: EventToolCall, Tool: &ToolCall{ID: "once", Name: "effect", Arguments: json.RawMessage(`{}`)}})
	}), nil
}

type continuationTool struct {
	calls              atomic.Int32
	started            chan struct{}
	release            chan struct{}
	ignoreCancellation bool
}

func (*continuationTool) Spec() ToolSpec {
	return ToolSpec{Name: "effect", Description: "fixture", Schema: map[string]any{"type": "object"}}
}
func (*continuationTool) Preview(json.RawMessage) string { return "effect" }
func (t *continuationTool) Execute(ctx context.Context, _ json.RawMessage) (ToolOutput, error) {
	t.calls.Add(1)
	if t.started != nil {
		close(t.started)
	}
	if t.release != nil {
		if t.ignoreCancellation {
			<-t.release
		} else {
			select {
			case <-t.release:
			case <-ctx.Done():
				return ToolOutput{}, ctx.Err()
			}
		}
	}
	return TextOutput("effect complete"), nil
}
func readSuspension(t *testing.T, stream Stream) *Continuation {
	t.Helper()
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("invocation completed rather than suspended")
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Err != nil {
			var suspended *SuspendedError
			if !errors.As(event.Err, &suspended) {
				t.Fatal(event.Err)
			}
			return suspended.Continuation
		}
	}
}

func TestContinuationPausesAfterHelloBeforeToolDispatch(t *testing.T) {
	c := &restart.Coordinator{}
	ctx, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, closeTask := c.NewTask(ctx)
	defer closeTask()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	provider := &continuationProvider{}
	tool := &continuationTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	engine.SetResponseCompletedCallback(func(context.Context, int, Message, TurnMetrics) error { c.Request(); return nil })
	stream, err := engine.Stream(ctx, Request{Messages: []Message{UserText("run")}, Tools: []ToolSpec{tool.Spec()}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	saved := readSuspension(t, stream)
	if tool.calls.Load() != 0 || len(saved.Pending) != 1 || MessageText(saved.Request.Messages[len(saved.Request.Messages)-1]) != "Hello" {
		t.Fatalf("bad boundary: %+v calls=%d", saved, tool.calls.Load())
	}
	// A different engine instance executes only the undispatched suffix. The
	// completed assistant text is context, not a second visible Hello message.
	restored := NewEngine(provider, registry)
	next, err := restored.Stream(context.Background(), Request{Resume: saved})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for {
		event, err := next.Recv()
		if err == io.EOF {
			break
		}
		if err != nil || event.Err != nil {
			t.Fatal(err, event.Err)
		}
		if event.Type == EventTextDelta && event.Text == "Hello" {
			t.Fatal("replayed completed assistant output")
		}
	}
	if tool.calls.Load() != 1 || provider.calls.Load() != 2 {
		t.Fatalf("replay: tools=%d model=%d", tool.calls.Load(), provider.calls.Load())
	}
}

func TestContinuationCancellationJoinsActualTool(t *testing.T) {
	c := &restart.Coordinator{InterruptAfter: 10 * time.Millisecond}
	ctx, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, closeTask := c.NewTask(ctx)
	defer closeTask()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	tool := &continuationTool{started: make(chan struct{}), release: make(chan struct{}), ignoreCancellation: true}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(&continuationProvider{}, registry)
	stream, err := engine.Stream(ctx, Request{Messages: []Message{UserText("run")}, Tools: []ToolSpec{tool.Spec()}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *Continuation, 1)
	go func() { done <- readSuspension(t, stream) }()
	<-tool.started
	c.Request()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("checkpoint overtook an actual tool")
	default:
	}
	close(tool.release)
	select {
	case saved := <-done:
		if !saved.Interrupted || len(saved.Pending) != 0 || !lastMessageHasToolResults(saved.Request.Messages) {
			t.Fatalf("bad cancelled boundary: %+v", saved)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled tool did not reach continuation boundary")
	}
}
