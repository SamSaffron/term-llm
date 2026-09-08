package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

func TestReloadWaitsForActualToolAfterCallerCancellation(t *testing.T) {
	t.Parallel()
	c := &restart.Coordinator{Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, release, err := c.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tool := newContextIgnoringTool(1)
	engine := NewEngine(&fakeProvider{}, nil)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		defer release()
		engine.executeToolWithCancellation(ctx, tool, json.RawMessage(`{}`))
	}()
	<-tool.started
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("caller did not cancel")
	}
	select {
	case <-replaced:
		t.Fatal("replaced while actual Tool.Execute was alive")
	default:
	}
	close(tool.release)
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("tool exit did not release ownership")
	}
}

func TestReloadWaitsForStreamProducerNotStreamClose(t *testing.T) {
	t.Parallel()
	c := &restart.Coordinator{Timeout: time.Second}
	ctx, release, _ := c.Enter(context.Background())
	started, finish := make(chan struct{}), make(chan struct{})
	stream := newEventStream(ctx, func(context.Context, eventSender) error { close(started); <-finish; return nil })
	<-started
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	closed := make(chan struct{})
	go func() { _ = stream.Close(); close(closed) }()
	release()
	select {
	case <-replaced:
		t.Fatal("stream close was mistaken for producer exit")
	default:
	}
	close(finish)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream close did not join")
	}
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("producer exit did not release ownership")
	}
}
