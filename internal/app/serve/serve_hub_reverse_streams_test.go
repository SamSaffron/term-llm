package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
)

const (
	hubReverseMaxStreams      = appserver.ReverseMaxStreams
	hubReverseStreamBodyBytes = appserver.ReverseStreamBodyBytes
)

func waitForHubStreams(t *testing.T, c *hubReverseConnection, streams int) {
	t.Helper()
	watchdog := time.After(5 * time.Second)
	for len(c.streams) != streams {
		select {
		case <-watchdog:
			t.Fatalf("event streams=%d, want %d", len(c.streams), streams)
		default:
			runtime.Gosched()
		}
	}
}

// Open tabs hold event streams indefinitely. Those streams move to their own
// pool, so they cannot pin the request slots that page loads need; only when
// that pool is full does a stream fall back to holding a request slot.
func TestHubReverseEventStreamsUseOwnPool(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
			if frame.Type != hubReverseFrameRequest {
				continue
			}
			resp := hubReverseResponse{ID: frame.ID, Status: http.StatusOK}
			if frame.Path == "/events" {
				resp.Type = hubReverseFrameResponseStart
				resp.Header = http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}
			}
			if peer.WriteJSON(resp) != nil {
				return
			}
		}
	}()
	do := func(ctx context.Context, path string) (*http.Response, error) {
		req := httptest.NewRequest(http.MethodGet, "http://reverse.local"+path, nil).WithContext(ctx)
		return s.reverse.do(ctx, node, req)
	}
	var streams []*http.Response
	defer func() {
		for _, resp := range streams {
			_ = resp.Body.Close()
		}
	}()
	open := func(count int) {
		t.Helper()
		for range count {
			resp, err := do(t.Context(), "/events")
			if err != nil {
				t.Fatal(err)
			}
			streams = append(streams, resp)
		}
	}
	get := func() {
		t.Helper()
		resp, err := do(t.Context(), "/api")
		if err != nil {
			t.Fatalf("request beside open event streams: %v", err)
		}
		_ = resp.Body.Close()
	}

	open(hubReverseMaxStreams)
	if len(c.streams) != hubReverseMaxStreams || len(c.active) != 0 {
		t.Fatalf("streams=%d active=%d, want %d/0", len(c.streams), len(c.active), hubReverseMaxStreams)
	}
	get()

	// A full stream pool falls back to request slots, as before.
	open(hubReverseMaxActive)
	if len(c.streams) != hubReverseMaxStreams || len(c.active) != hubReverseMaxActive {
		t.Fatalf("streams=%d active=%d, want %d/%d", len(c.streams), len(c.active), hubReverseMaxStreams, hubReverseMaxActive)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := do(t.Context(), "/api")
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	waitForHubAdmission(t, c, hubReverseMaxActive, 1)
	last := len(streams) - 1
	_ = streams[last].Body.Close() // a fallback stream frees its request slot
	streams = streams[:last]
	if err := containmentAwait(t, done); err != nil {
		t.Fatalf("waiter after a fallback stream closed: %v", err)
	}

	// Closing a pooled stream frees room for the next one.
	_ = streams[0].Body.Close()
	streams = streams[1:]
	waitForHubStreams(t, c, hubReverseMaxStreams-1)
	open(1)
	waitForHubStreams(t, c, hubReverseMaxStreams)
	waitForHubAdmission(t, c, hubReverseMaxActive-1, 0)

	for _, resp := range streams {
		_ = resp.Body.Close()
	}
	streams = nil
	waitForHubAdmission(t, c, 0, 0)
	waitForHubStreams(t, c, 0)
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("retained %d pending operations", pending)
	}
}

// Pooled event streams buffer at most their smaller window for a stalled
// consumer; ordinary streams keep the request window.
func TestHubReverseEventStreamWindow(t *testing.T) {
	const chunk = 32 << 10
	const sent = 2 * hubReverseStreamBodyBytes // above the stream window, below the request window
	for _, tc := range []struct {
		contentType string
		cutOff      bool
	}{
		{contentType: "text/event-stream", cutOff: true},
		{contentType: "application/octet-stream", cutOff: false},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			s, _, node, peer, _ := containmentHub(t)
			go func() {
				var streamID string
				for {
					var frame hubReverseRequest
					if peer.ReadJSON(&frame) != nil {
						return
					}
					if frame.Type != hubReverseFrameRequest {
						continue
					}
					switch frame.Path {
					case "/stream":
						streamID = frame.ID
						_ = peer.WriteJSON(hubReverseResponse{ID: frame.ID, Type: hubReverseFrameResponseStart, Status: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}})
					case "/barrier":
						// The Hub dispatches frames in order: once the barrier
						// completes, every stream frame has been queued or refused.
						for range sent / chunk {
							_ = peer.WriteJSON(hubReverseResponse{ID: streamID, Type: hubReverseFrameResponseBody, Body: bytes.Repeat([]byte("x"), chunk)})
						}
						_ = peer.WriteJSON(hubReverseResponse{ID: streamID, Type: hubReverseFrameResponseEnd})
						_ = peer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: http.StatusOK})
					}
				}
			}()
			req := httptest.NewRequest(http.MethodGet, "http://reverse.local/stream", nil).WithContext(t.Context())
			stream, err := s.reverse.do(t.Context(), node, req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			if err := containmentAwait(t, containmentDo(s, t.Context(), node, "/barrier", nil)); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(stream.Body)
			if tc.cutOff {
				if !errors.Is(err, errHubReverseSlowConsumer) {
					t.Fatalf("stalled event stream read %d bytes, err=%v; want slow-consumer cutoff", len(body), err)
				}
				return
			}
			if err != nil || len(body) != sent {
				t.Fatalf("stream read %d/%d bytes, err=%v", len(body), sent, err)
			}
		})
	}
}
