package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSteeringFreezeOwnsWholeQueueAndRollsBack(t *testing.T) {
	e := NewEngine(&OpenAIProvider{}, nil)
	e.beginSteeringRun(true)
	for _, id := range []string{"b", "a"} {
		e.QueueSteering(QueuedSteering{ID: id, Message: UserText(id), Origin: SteeringOriginUser})
	}
	owner := SteeringTransition{OperationID: "rush", Fence: 1}
	entries, err := e.FreezeSteering(owner)
	if err != nil || len(entries) != 2 {
		t.Fatalf("freeze: %+v %v", entries, err)
	}
	if got := e.DrainSteering(); len(got) != 0 {
		t.Fatal("source drained frozen input")
	}
	if _, status := e.QueueSteeringWithStatus(QueuedSteering{ID: "late", Message: UserText("late")}); status != SteeringQueueTransitioning {
		t.Fatalf("late input: %s", status)
	}
	if e.CancelSteering("b") || e.DiscardPendingSteering() != 0 {
		t.Fatal("ordinary cancellation stole frozen input")
	}
	if e.ReleaseSteeringFreeze(SteeringTransition{OperationID: "wrong", Fence: 1}, true) {
		t.Fatal("wrong owner released freeze")
	}
	if !e.ReleaseSteeringFreeze(owner, false) {
		t.Fatal("rollback failed")
	}
	got := e.DrainSteering()
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("rollback changed FIFO: %+v", got)
	}
}

func TestOpenAIHTTPInterruptThenResumeWithSeparateSteeringRows(t *testing.T) {
	var calls atomic.Int32
	cancelled := make(chan struct{})
	secondInput := make(chan []ResponsesInputItem, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []ResponsesInputItem `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(cancelled)
			return
		}
		secondInput <- request.Input
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"replacement\"}}\n\n")
	}))
	defer server.Close()
	p := &OpenAIProvider{model: "gpt-4.1", responsesClient: &ResponsesClient{BaseURL: server.URL, HTTPClient: server.Client()}}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Stream(ctx, Request{Messages: []Message{UserText("source")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not settle")
	}
	a := UserText("first steer")
	a.ClientMessageID = "a"
	b := UserText("second steer")
	b.ClientMessageID = "b"
	next, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("source"), AssistantText("partial"), a, b}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = next.Recv()
		if err != nil {
			break
		}
	}
	_ = next.Close()
	input := <-secondInput
	if len(input) != 4 {
		t.Fatalf("replacement lost distinct input rows: %+v", input)
	}
	if a.ClientMessageID != "a" || b.ClientMessageID != "b" {
		t.Fatal("provider rewrote message identity")
	}
}

type steeringBridgeProvider struct {
	Provider
	execute func(context.Context, string, json.RawMessage) (ToolOutput, error)
}

func (p *steeringBridgeProvider) SetToolExecutor(execute func(context.Context, string, json.RawMessage) (ToolOutput, error)) {
	p.execute = execute
}

// Neither synthetic cancellation nor a cancelled native bridge certifies actual
// tool completion. Both still allow the user to request Rush while work is active.
func TestRushWaitsUntilCancelledToolActuallyExits(t *testing.T) {
	for _, bridged := range []bool{false, true} {
		name := "engine"
		if bridged {
			name = "native-bridge"
		}
		t.Run(name, func(t *testing.T) {
			tool := newContextIgnoringTool(1)
			released := false
			defer func() {
				if !released {
					close(tool.release)
				}
			}()
			registry := NewToolRegistry()
			registry.Register(tool)
			provider := &steeringBridgeProvider{Provider: NewMockProvider("custom")}
			e := NewEngine(provider, registry)
			e.beginSteeringRun(true)
			e.QueueSteering(QueuedSteering{ID: "guidance", Message: UserText("keep this"), Origin: SteeringOriginUser})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if bridged {
					_, err := provider.execute(ctx, tool.Spec().Name, json.RawMessage(`{}`))
					result <- err
				} else {
					_, err, _ := e.executeToolWithCancellation(ctx, tool, json.RawMessage(`{}`))
					result <- err
				}
			}()
			select {
			case <-tool.started:
			case <-time.After(3 * time.Second):
				t.Fatal("tool did not start")
			}
			e.callbackMu.RLock()
			settled := e.steeringToolsSettled
			e.callbackMu.RUnlock()
			if !e.SteeringAvailability().CanRush {
				t.Fatal("active tools must permit cancellation")
			}
			owner := SteeringTransition{OperationID: "rush", Fence: 1}
			entries, err := e.FreezeSteering(owner)
			if err != nil || len(entries) != 1 {
				t.Fatalf("admission: %v, %+v", err, entries)
			}
			cancel()
			if !bridged {
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("synthetic result: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("synthetic cancellation did not return")
				}
			}
			if err := e.WaitSteeringSettlement(ctx, owner); !errors.Is(err, context.Canceled) {
				t.Fatalf("abandoned execution counted as settled: %v", err)
			}
			if !e.SteeringTransitioning() {
				t.Fatal("cancelled wait released freeze")
			}
			select {
			case <-settled:
				t.Fatal("cancellation closed actual settlement barrier")
			default:
			}
			close(tool.release)
			released = true
			select {
			case <-settled:
			case <-time.After(3 * time.Second):
				t.Fatal("actual completion did not settle")
			}
			if err := e.WaitSteeringSettlement(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			e.ReleaseSteeringFreeze(owner, false)
		})
	}
}

func TestRushUsesNormalContinuationAcrossProviders(t *testing.T) {
	providers := map[string]Provider{
		"openai": &OpenAIProvider{}, "openai-websocket": &OpenAIProvider{useWebSocket: true},
		"compat": &OpenAICompatProvider{}, "anthropic": &AnthropicProvider{}, "gemini": &GeminiProvider{},
		"claude-bin": &ClaudeBinProvider{}, "cursor-bin": &CursorBinProvider{},
		"agy-bin": &AgyBinProvider{}, "grok-acp": &GrokBinProvider{},
		"custom": NewMockProvider("custom"), "retry": &RetryProvider{inner: NewMockProvider("custom")},
	}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			e := NewEngine(provider, nil)
			if e.SteeringAvailability().CanRush {
				t.Fatal("idle run offered Rush")
			}
			e.beginSteeringRun(true)
			e.QueueSteering(QueuedSteering{ID: "guidance", Message: UserText("continue here"), Origin: SteeringOriginUser})
			if !e.SteeringAvailability().CanRush {
				t.Fatal("provider type blocked normal stop-and-continue")
			}
			owner := SteeringTransition{OperationID: "rush", Fence: 1}
			entries, err := e.FreezeSteering(owner)
			if err != nil || len(entries) != 1 {
				t.Fatalf("admission: %v, %+v", err, entries)
			}
			e.ReleaseSteeringFreeze(owner, false)
		})
	}
}
