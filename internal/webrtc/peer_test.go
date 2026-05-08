package webrtc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// newTestPeer creates a peer wired to handler with the given basePath,
// suitable for calling dispatchRequest in unit tests.
func newTestPeer(basePath string, handler http.Handler) *peer {
	return &peer{
		cfg:     Config{BasePath: basePath, SignalingURL: "https://example.com"},
		handler: handler,
	}
}

// collectFrames runs dispatchRequest and returns all JSON frames sent.
func collectFrames(p *peer, rawFrame []byte) []responseFrame {
	var frames []responseFrame
	send := func(text string) error {
		var f responseFrame
		if err := json.Unmarshal([]byte(text), &f); err == nil {
			frames = append(frames, f)
		}
		return nil
	}
	p.dispatchRequest(context.Background(), send, rawFrame)
	return frames
}

func collectChunkData(frames []responseFrame) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Type == "chunk" {
			b.WriteString(f.Data)
		}
	}
	return b.String()
}

func encodeRequest(id, method, path string, headers map[string]string, body string) []byte {
	f := requestFrame{ID: id, Method: method, Path: path, Headers: headers}
	if body != "" {
		f.Body = base64.StdEncoding.EncodeToString([]byte(body))
	}
	data, _ := json.Marshal(f)
	return data
}

// --- validPath tests ---

func TestValidPath_Allowed(t *testing.T) {
	p := newTestPeer("/ui", nil)
	for _, path := range []string{
		"/ui/v1/responses",
		"/ui/v1/models",
		"/ui/v1/sessions/abc/messages",
	} {
		if !p.validPath(path) {
			t.Errorf("expected valid path %q", path)
		}
	}
}

func TestValidPath_Rejected(t *testing.T) {
	p := newTestPeer("/ui", nil)
	for _, path := range []string{
		"/ui/v1/../etc/passwd",
		"/ui/images/secret.png",
		"/ui/",
		"/v1/responses",      // missing basePath
		"../ui/v1/responses", // traversal
		"/ui/v2/jobs",        // not under /v1/
		"",
	} {
		if p.validPath(path) {
			t.Errorf("expected invalid path %q to be rejected", path)
		}
	}
}

// --- dispatchRequest tests ---

func TestDispatchRequest_ValidPath(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello\n"))
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req1", "GET", "/ui/v1/models", nil, "")
	frames := collectFrames(p, raw)

	if len(frames) < 2 {
		t.Fatalf("expected at least 2 frames (headers + done), got %d: %v", len(frames), frames)
	}
	if frames[0].Type != "headers" {
		t.Errorf("first frame should be headers, got %q", frames[0].Type)
	}
	last := frames[len(frames)-1]
	if last.Type != "done" {
		t.Errorf("last frame should be done, got %q", last.Type)
	}
	if last.Status != http.StatusOK {
		t.Errorf("done status = %d, want 200", last.Status)
	}
}

func TestDispatchRequest_RejectsInvalidPath(t *testing.T) {
	p := newTestPeer("/ui", http.DefaultServeMux)
	for _, path := range []string{"/ui/v1/../etc/passwd", "/ui/images/", "/"} {
		raw := encodeRequest("req1", "GET", path, nil, "")
		frames := collectFrames(p, raw)
		if len(frames) != 1 || frames[0].Type != "done" || frames[0].Status != http.StatusBadRequest {
			t.Errorf("path %q: expected single done/400 frame, got %v", path, frames)
		}
	}
}

func TestDispatchRequest_AuthTokenForwarded(t *testing.T) {
	var gotAuth string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	p := newTestPeer("/ui", handler)
	hdrs := map[string]string{"Authorization": "Bearer secrettoken"}
	raw := encodeRequest("req2", "GET", "/ui/v1/models", hdrs, "")
	collectFrames(p, raw)

	if gotAuth != "Bearer secrettoken" {
		t.Errorf("Authorization header not forwarded: got %q", gotAuth)
	}
}

func TestDispatchRequest_StripsAcceptEncoding(t *testing.T) {
	var gotAcceptEncoding string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	})
	p := newTestPeer("/ui", handler)
	hdrs := map[string]string{"Accept-Encoding": "gzip", "Authorization": "Bearer secrettoken"}
	raw := encodeRequest("req-encoding", "GET", "/ui/v1/models", hdrs, "")
	collectFrames(p, raw)

	if gotAcceptEncoding != "" {
		t.Errorf("Accept-Encoding should not be forwarded over WebRTC: got %q", gotAcceptEncoding)
	}
}

func TestDispatchRequest_AuthTokenMissing(t *testing.T) {
	// Simulate an auth middleware that rejects requests without a token.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req3", "GET", "/ui/v1/models", nil, "")
	frames := collectFrames(p, raw)

	last := frames[len(frames)-1]
	if last.Status != http.StatusUnauthorized {
		t.Errorf("expected 401 done frame without auth token, got %d", last.Status)
	}
}

func TestDispatchRequest_BodySizeLimit(t *testing.T) {
	p := newTestPeer("/ui", http.DefaultServeMux)
	// Create a base64 string that represents more than maxFrameBytes of data.
	bigBody := strings.Repeat("A", maxFrameBytes+1)
	f := requestFrame{ID: "req4", Method: "POST", Path: "/ui/v1/responses", Body: bigBody}
	raw, _ := json.Marshal(f)
	frames := collectFrames(p, raw)

	if len(frames) != 1 || frames[0].Status != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 done frame for oversized body, got %v", frames)
	}
}

func TestDispatchRequest_BodyDecodedAndForwarded(t *testing.T) {
	const bodyContent = `{"stream":true}`
	var gotBody string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req5", "POST", "/ui/v1/responses", nil, bodyContent)
	collectFrames(p, raw)

	if gotBody != bodyContent {
		t.Errorf("body not forwarded correctly: got %q, want %q", gotBody, bodyContent)
	}
}

func TestNewPeer_InsecureSignalingURL(t *testing.T) {
	_, err := New(context.Background(), Config{
		SignalingURL: "http://example.com/signal",
	}, http.DefaultServeMux)
	if err == nil {
		t.Fatal("expected error for http:// signaling URL")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("error should mention HTTPS: %v", err)
	}
}

func TestChunkStreaming(t *testing.T) {
	// Handler that writes multiple SSE lines.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-response-id", "resp-abc")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, line := range []string{
			"event: ping",
			"data: {}",
			"",
			"event: ping",
			"data: {}",
			"",
		} {
			_, _ = w.Write([]byte(line + "\n"))
			flusher.Flush()
		}
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req6", "GET", "/ui/v1/responses", nil, "")
	frames := collectFrames(p, raw)

	// Expect: 1 headers frame, chunk frames, 1 done frame.
	if len(frames) < 3 {
		t.Fatalf("too few frames: %v", frames)
	}
	if frames[0].Type != "headers" {
		t.Errorf("frame[0] should be headers, got %q", frames[0].Type)
	}
	if frames[0].Headers["x-response-id"] != "resp-abc" {
		t.Errorf("headers frame missing x-response-id: %v", frames[0].Headers)
	}

	got := collectChunkData(frames)
	want := "event: ping\ndata: {}\n\nevent: ping\ndata: {}\n\n"
	if got != want {
		t.Fatalf("chunk data mismatch\ngot:  %q\nwant: %q", got, want)
	}

	last := frames[len(frames)-1]
	if last.Type != "done" || last.Status != http.StatusOK {
		t.Errorf("last frame should be done/200, got %v", last)
	}
}

func TestChunkWriter_SplitsLargePayloadInto16KFrames(t *testing.T) {
	large := strings.Repeat("🙂", maxChunkDataBytes) + "tail"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(large))
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req-large", "GET", "/ui/v1/models", nil, "")
	frames := collectFrames(p, raw)

	var chunkCount int
	for _, f := range frames {
		if f.Type != "chunk" {
			continue
		}
		chunkCount++
		if len([]byte(f.Data)) > maxChunkDataBytes {
			t.Fatalf("chunk size = %d, want <= %d", len([]byte(f.Data)), maxChunkDataBytes)
		}
	}
	if chunkCount < 2 {
		t.Fatalf("expected payload to split across multiple chunk frames, got %d", chunkCount)
	}
	if got := collectChunkData(frames); got != large {
		t.Fatalf("reconstructed payload mismatch: got %d bytes want %d", len(got), len(large))
	}
}

func TestFlushSendsHeadersWithoutBody(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-response-id", "resp-idle")
		w.(http.Flusher).Flush()
	})
	p := newTestPeer("/ui", handler)
	raw := encodeRequest("req7", "GET", "/ui/v1/responses/resp-idle/events", nil, "")
	frames := collectFrames(p, raw)

	if len(frames) < 2 {
		t.Fatalf("expected at least 2 frames (headers + done), got %d: %v", len(frames), frames)
	}
	if frames[0].Type != "headers" {
		t.Fatalf("frame[0] should be headers, got %q", frames[0].Type)
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("headers status = %d, want 200", frames[0].Status)
	}
	if frames[0].Headers["content-type"] != "text/event-stream" {
		t.Fatalf("headers content-type = %q, want text/event-stream", frames[0].Headers["content-type"])
	}
	if frames[0].Headers["x-response-id"] != "resp-idle" {
		t.Fatalf("headers x-response-id = %q, want resp-idle", frames[0].Headers["x-response-id"])
	}

	last := frames[len(frames)-1]
	if last.Type != "done" || last.Status != http.StatusOK {
		t.Fatalf("last frame should be done/200, got %v", last)
	}
}

func TestFrameEncoding_RoundTrip(t *testing.T) {
	original := requestFrame{
		ID:      "test-id",
		Method:  "POST",
		Path:    "/ui/v1/responses",
		Headers: map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/json"},
		Body:    base64.StdEncoding.EncodeToString([]byte(`{"stream":true}`)),
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded requestFrame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != original.ID || decoded.Method != original.Method ||
		decoded.Path != original.Path || decoded.Body != original.Body {
		t.Errorf("round-trip mismatch: got %+v", decoded)
	}
	bodyBytes, _ := base64.StdEncoding.DecodeString(decoded.Body)
	if string(bodyBytes) != `{"stream":true}` {
		t.Errorf("body decode mismatch: %q", string(bodyBytes))
	}
}

func TestDispatchRequest_CancelPostStopsSSEStream(t *testing.T) {
	// Simulate a response run: the SSE endpoint streams until cancelled,
	// and the cancel endpoint triggers the cancellation.
	cancelCh := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses/resp-123/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.in_progress\ndata: {}\n\n"))
		// Block until cancel signal.
		select {
		case <-cancelCh:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("event: response.cancelled\ndata: {}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	mux.HandleFunc("/v1/responses/resp-123/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		close(cancelCh)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}\n"))
	})

	// The outer handler must strip the base path, matching the real serve.go setup.
	handler := http.StripPrefix("/ui", mux)
	p := newTestPeer("/ui", handler)

	// Start the SSE stream in a goroutine since it blocks until cancel.
	var sseFrames []responseFrame
	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		raw := encodeRequest("sse-1", "GET", "/ui/v1/responses/resp-123/events", nil, "")
		sseFrames = collectFrames(p, raw)
	}()

	// Give the SSE handler time to start streaming.
	// Then dispatch the cancel POST.
	// Use a brief sleep since we need the SSE goroutine to be blocked in the handler.
	for i := 0; i < 50; i++ {
		select {
		case <-sseDone:
			// SSE finished early (shouldn't happen before cancel).
			t.Fatal("SSE stream ended before cancel was sent")
		default:
		}
		// Check if the SSE handler has started (cancelCh not closed yet means it's waiting).
		select {
		case <-cancelCh:
			t.Fatal("cancelCh closed before cancel POST")
		default:
		}
		if i > 5 {
			break
		}
		// Small yield to let the SSE goroutine start.
		func() {
			ch := make(chan struct{})
			go func() { close(ch) }()
			<-ch
		}()
	}

	cancelRaw := encodeRequest("cancel-1", "POST", "/ui/v1/responses/resp-123/cancel", nil, "")
	cancelFrames := collectFrames(p, cancelRaw)

	// Verify cancel POST returned 200.
	cancelLast := cancelFrames[len(cancelFrames)-1]
	if cancelLast.Type != "done" || cancelLast.Status != http.StatusOK {
		t.Fatalf("cancel POST: expected done/200, got type=%q status=%d", cancelLast.Type, cancelLast.Status)
	}

	// Wait for SSE stream to finish (it should now that cancel was sent).
	<-sseDone

	// Verify the SSE stream received terminal events.
	sseData := collectChunkData(sseFrames)
	if !strings.Contains(sseData, "response.cancelled") {
		t.Errorf("SSE stream missing response.cancelled event, got: %q", sseData)
	}
	if !strings.Contains(sseData, "[DONE]") {
		t.Errorf("SSE stream missing [DONE], got: %q", sseData)
	}
	sseLast := sseFrames[len(sseFrames)-1]
	if sseLast.Type != "done" || sseLast.Status != http.StatusOK {
		t.Errorf("SSE stream: expected done/200, got type=%q status=%d", sseLast.Type, sseLast.Status)
	}
}

func TestResponseFrameEncoding_PreservesBlankChunkData(t *testing.T) {
	frame := responseFrame{
		ID:   "chunk-id",
		Type: "chunk",
		Data: "",
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"data":""`) {
		t.Fatalf("blank chunk data omitted from JSON: %s", string(data))
	}
}
