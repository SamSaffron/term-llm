package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCopilotChatCompletionsCloseDoesNotHangWhenConsumerStopsReading(t *testing.T) {
	started := make(chan struct{})
	wroteMany := make(chan struct{})
	cancelled := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer does not support flushing")
			close(started)
			close(wroteMany)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		close(started)

		for i := 0; i < 64; i++ {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			flusher.Flush()
		}
		close(wroteMany)

		<-r.Context().Done()
		close(cancelled)
	}))
	defer ts.Close()

	provider := &CopilotProvider{
		model:              "gpt-4.1",
		apiBaseURL:         ts.URL,
		sessionToken:       "test-session",
		sessionTokenExpiry: time.Now().Add(time.Hour),
	}

	stream, err := provider.streamChatCompletions(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	}, "gpt-4.1")
	if err != nil {
		t.Fatalf("streamChatCompletions() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive request")
	}

	select {
	case <-wroteMany:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not write streaming response")
	}

	// Give the producer a moment to fill the buffered event channel and block on
	// a send before we close the stream.
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stream.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung while producer was blocked on event send")
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not cancelled")
	}
}
