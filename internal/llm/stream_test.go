package llm

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestNewEventStreamReturnsRunErrorAfterBufferedEventsWhenBufferIsFull(t *testing.T) {
	filled := make(chan struct{})
	wantErr := errors.New("boom")
	bufSize := 0

	stream := newEventStream(context.Background(), func(ctx context.Context, send eventSender) error {
		bufSize = cap(send.ch)
		for range bufSize {
			send.Send(Event{Type: EventTextDelta, Text: "x"})
		}
		close(filled)
		return wantErr
	})

	<-filled

	for i := range bufSize {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() %d error = %v", i, err)
		}
		if event.Type == EventError {
			t.Fatalf("Recv() %d unexpectedly returned error event: %v", i, event.Err)
		}
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() terminal error = %v, want nil", err)
	}
	if event.Type != EventError {
		t.Fatalf("Recv() terminal event type = %v, want %v", event.Type, EventError)
	}
	if !errors.Is(event.Err, wantErr) {
		t.Fatalf("Recv() terminal event error = %v, want %v", event.Err, wantErr)
	}

	event, err = stream.Recv()
	if err != io.EOF {
		t.Fatalf("final Recv() error = %v, want %v (event=%+v)", err, io.EOF, event)
	}
}

func TestTrySend_ClosedChannel(t *testing.T) {
	ch := make(chan Event)
	close(ch)

	s := eventSender{ctx: context.Background(), ch: ch}

	// Must not panic; should return false
	if s.TrySend(Event{Type: EventHeartbeat}) {
		t.Fatal("TrySend should return false for closed channel")
	}
}

func TestTrySend_CancelledContext(t *testing.T) {
	ch := make(chan Event) // unbuffered, will block

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := eventSender{ctx: ctx, ch: ch}

	if s.TrySend(Event{Type: EventHeartbeat}) {
		t.Fatal("TrySend should return false for cancelled context")
	}
}

func TestTrySend_Success(t *testing.T) {
	ch := make(chan Event, 1)
	s := eventSender{ctx: context.Background(), ch: ch}

	if !s.TrySend(Event{Type: EventHeartbeat, Text: "ping"}) {
		t.Fatal("TrySend should return true when buffer has space")
	}
	got := <-ch
	if got.Text != "ping" {
		t.Fatalf("expected text 'ping', got %q", got.Text)
	}
}

func TestTrySend_BufferFull(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventHeartbeat} // fill the buffer

	s := eventSender{ctx: context.Background(), ch: ch}

	if s.TrySend(Event{Type: EventHeartbeat}) {
		t.Fatal("TrySend should return false when buffer is full")
	}
}

func TestTrySend_NilChannel(t *testing.T) {
	s := eventSender{}

	if s.TrySend(Event{Type: EventHeartbeat}) {
		t.Fatal("TrySend should return false for nil channel")
	}
}
