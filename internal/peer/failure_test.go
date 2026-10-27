package peer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOversizedRequestDoesNotPoisonHealthyChannel(t *testing.T) {
	limits := Limits{MaxFrameBytes: 8 << 10, MaxRequestBodyBytes: 1 << 20}
	normalized := limits.withDefaults()
	var dispatches atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte(strings.Repeat("x", len(body))))
	})
	p := newPair(t, pairConfig{handler: handler, limits: limits})
	client := p.httpClient()

	oversized, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://peer/too-large", bytes.NewReader(make([]byte, normalized.MaxRequestBodyBytes+1)))
	if _, err := client.Do(oversized); err == nil || !strings.Contains(err.Error(), "request body exceeds") {
		t.Fatalf("oversized body error = %v", err)
	}

	encodedOversize, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://peer/header-overhead", bytes.NewReader(make([]byte, normalized.MaxRequestBodyBytes)))
	encodedOversize.Header.Set("X-Fill", strings.Repeat("h", 3<<10))
	if _, err := client.Do(encodedOversize); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("encoded oversized request error = %v, want errFrameTooLarge", err)
	}
	if got := dispatches.Load(); got != 0 {
		t.Fatalf("rejected requests dispatched %d times", got)
	}

	small, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://peer/small", strings.NewReader("ok"))
	response, err := client.Do(small)
	if err != nil {
		t.Fatalf("small request after rejection: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || string(body) != "xx" || dispatches.Load() != 1 {
		t.Fatalf("small response body=%q readErr=%v dispatches=%d", body, readErr, dispatches.Load())
	}
}

func TestChannelLossTerminatesRequestsWithoutCompletingTruncatedResponses(t *testing.T) {
	t.Run("before headers", func(t *testing.T) {
		started := make(chan struct{})
		handlerDone := make(chan struct{})
		channelDone := make(chan struct{})
		handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			defer close(handlerDone)
			<-r.Context().Done()
		})
		p := newPair(t, pairConfig{handler: handler, onClose: func() { close(channelDone) }})
		result := make(chan error, 1)
		go func() {
			request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://peer/wait", nil)
			_, err := p.httpClient().Do(request)
			result <- err
		}()
		waitFailureSignal(t, started, "handler start")
		if err := p.cliEP.Close(); err != nil {
			t.Fatal(err)
		}
		if err := waitFailureResult(t, result, "request before headers"); !errors.Is(err, ErrClosed) {
			t.Fatalf("request error = %v, want ErrClosed", err)
		}
		waitFailureSignal(t, handlerDone, "handler cancellation")
		waitFailureSignal(t, channelDone, "server channel drain")
		assertClientDrained(t, p.client)
	})

	for _, test := range []struct {
		name        string
		contentType string
		first       string
	}{
		{name: "mid finite body", contentType: "text/plain", first: "first-body"},
		{name: "mid SSE", contentType: "text/event-stream", first: "data: first\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstSent := make(chan struct{})
			handlerDone := make(chan struct{})
			lateWrite := make(chan error, 1)
			channelDone := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.first)
				w.(http.Flusher).Flush()
				close(firstSent)
				<-r.Context().Done()
				_, err := w.Write(bytes.Repeat([]byte("l"), DefaultBodyChunkBytes))
				lateWrite <- err
			})
			p := newPair(t, pairConfig{handler: handler, onClose: func() { close(channelDone) }})
			request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://peer/stream", nil)
			response, err := p.httpClient().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			first := make([]byte, len(test.first))
			if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != test.first {
				t.Fatalf("first payload = %q, %v", first, err)
			}
			waitFailureSignal(t, firstSent, "first response chunk")
			if err := p.cliEP.Close(); err != nil {
				t.Fatal(err)
			}
			if remainder, err := io.ReadAll(response.Body); !errors.Is(err, ErrClosed) {
				t.Fatalf("truncated response completed: remainder=%q err=%v", remainder, err)
			}
			if err := waitFailureResult(t, lateWrite, "late handler write"); err == nil {
				t.Fatal("late handler write succeeded after channel loss")
			}
			waitFailureSignal(t, handlerDone, "handler drain")
			waitFailureSignal(t, channelDone, "server channel drain")
			assertClientDrained(t, p.client)
		})
	}
}

func TestCancellationUnparksBackpressuredSenderAndKeepsChannelUsable(t *testing.T) {
	backpressured := make(chan struct{}, 1)
	writeResult := make(chan error, 1)
	lateWriteResult := make(chan error, 1)
	handlerDone := make(chan struct{})
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/small" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		defer close(handlerDone)
		w.(http.Flusher).Flush()
		_, err := w.Write(bytes.Repeat([]byte("x"), 32<<20))
		writeResult <- err
		_, err = io.WriteString(w, "late")
		lateWriteResult <- err
	})
	p := newPair(t, pairConfig{
		handler: handler,
		limits:  Limits{MaxConcurrentStreams: 1, MaxBufferedBytes: 32 << 10},
		testOnBackpressure: func() {
			select {
			case backpressured <- struct{}{}:
			default:
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer/backpressure", nil)
	response, err := p.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	waitFailureSignal(t, backpressured, "real DataChannel backpressure")
	cancel()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("body after cancellation = %v, want context.Canceled", err)
	}
	if err := waitFailureResult(t, writeResult, "blocked write cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked write error = %v, want context.Canceled", err)
	}
	if err := waitFailureResult(t, lateWriteResult, "late write cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("late write error = %v, want context.Canceled", err)
	}
	waitFailureSignal(t, handlerDone, "backpressured handler drain")
	assertClientDrained(t, p.client)

	small, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://peer/small", nil)
	smallResponse, err := p.httpClient().Do(small)
	if err != nil {
		t.Fatalf("small request after backpressure cancellation: %v", err)
	}
	body, readErr := io.ReadAll(smallResponse.Body)
	smallResponse.Body.Close()
	if readErr != nil || string(body) != "ok" || calls.Load() != 2 {
		t.Fatalf("small response body=%q readErr=%v calls=%d", body, readErr, calls.Load())
	}
}

func assertClientDrained(t *testing.T, client *Client) {
	t.Helper()
	client.mu.Lock()
	streams := len(client.streams)
	client.mu.Unlock()
	if streams != 0 || len(client.sem) != 0 {
		t.Fatalf("client retained streams=%d slots=%d", streams, len(client.sem))
	}
}

func waitFailureSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitFailureResult(t *testing.T, result <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}
