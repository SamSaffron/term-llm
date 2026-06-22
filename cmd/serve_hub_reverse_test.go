package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubReverseConnectionNextRequestIDIsUnique(t *testing.T) {
	var c hubReverseConnection
	const goroutines = 32
	const idsPerGoroutine = 512

	ids := make(chan string, goroutines*idsPerGoroutine)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < idsPerGoroutine; j++ {
				ids <- c.nextRequestID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*idsPerGoroutine)
	for id := range ids {
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate request ID %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != goroutines*idsPerGoroutine {
		t.Fatalf("unique IDs = %d, want %d", len(seen), goroutines*idsPerGoroutine)
	}
}

func TestHubReverseNodeProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/healthz" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer node-token" {
			t.Fatalf("backend auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","agent":"artist"}`))
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())
	waitForReverseNode(t, s, "artist")

	req := httptest.NewRequest(http.MethodGet, "/node/artist/healthz", nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"agent":"artist"`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestHubReverseNodeProxyStreamsChunkedResponse(t *testing.T) {
	large := strings.Repeat("0123456789abcdef", (hubReverseChunkSize*3)/16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/stream" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(large))
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())
	waitForReverseNode(t, s, "artist")

	req := httptest.NewRequest(http.MethodGet, "/node/artist/stream", nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body len=%d", rec.Code, rec.Body.Len())
	}
	if rec.Body.String() != large {
		t.Fatalf("streamed body mismatch len=%d want=%d", rec.Body.Len(), len(large))
	}
}

func TestHubReverseNodeProxyRejectsOversizedRequestBody(t *testing.T) {
	backendCalled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case backendCalled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())
	waitForReverseNode(t, s, "artist")

	payload := strings.Repeat("a", hubReverseMaxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/node/artist/upload", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reverse request body exceeds") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	select {
	case <-backendCalled:
		t.Fatal("backend received oversized reverse request body")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHubReverseResponseCancellation(t *testing.T) {
	backendCanceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/slow" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("hello"))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(backendCanceled)
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	connectorCtx, stopConnector := context.WithCancel(context.Background())
	defer stopConnector()
	go runHubReverseConnector(connectorCtx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())
	waitForReverseNode(t, s, "artist")

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/slow", nil).WithContext(reqCtx)
	resp, err := s.reverse.do(reqCtx, node, req)
	if err != nil {
		t.Fatalf("reverse do: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	cancelReq()
	_, err = io.ReadAll(resp.Body)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel err = %v", err)
	}
	select {
	case <-backendCanceled:
	case <-time.After(2 * time.Second):
		t.Fatalf("backend request was not canceled")
	}
}

func TestHubReverseReadLoopBackpressuresSlowConsumerInsteadOfCanceling(t *testing.T) {
	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	wsURL := "ws" + strings.TrimPrefix(hubTS.URL, "http") + "/api/connect?node_id=artist"
	header := http.Header{}
	header.Set("Authorization", "Bearer node-token")
	header.Set(hubNodeIDHeader, "artist")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial reverse websocket: %v", err)
	}
	defer conn.Close()
	waitForReverseNode(t, s, "artist")

	requests := make(chan hubReverseRequest, 16)
	go func() {
		for {
			var frame hubReverseRequest
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == hubReverseFrameRequest {
				requests <- frame
			}
		}
	}()

	firstResult := make(chan reverseDoResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/slow-stream", nil)
		resp, err := s.reverse.do(context.Background(), node, req)
		firstResult <- reverseDoResult{resp: resp, err: err}
	}()
	firstReq := waitForReverseRequest(t, requests, "/chat/slow-stream")
	if err := conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: firstReq.ID, Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}}}); err != nil {
		t.Fatalf("write first response_start: %v", err)
	}
	first := waitForReverseDoResult(t, firstResult)
	if first.err != nil {
		t.Fatalf("first reverse do: %v", first.err)
	}
	defer first.resp.Body.Close()

	const chunk = "chunk"
	const chunkCount = hubReversePendingBuffer + 4
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < chunkCount; i++ {
			if err := conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseBody, ID: firstReq.ID, Body: []byte(chunk)}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseEnd, ID: firstReq.ID})
	}()

	// Leave the response body unread long enough for the per-request queue to fill.
	// The stream must apply backpressure instead of being canceled as "slow".
	time.Sleep(100 * time.Millisecond)

	body, err := io.ReadAll(first.resp.Body)
	if err != nil {
		t.Fatalf("read slow body: %v", err)
	}
	if string(body) != strings.Repeat(chunk, chunkCount) {
		t.Fatalf("slow body len=%d, want len=%d", len(body), len(strings.Repeat(chunk, chunkCount)))
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("write slow response: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reverse writer")
	}
}

func TestHubReverseReadLoopSurvivesCanceledUndrainedStream(t *testing.T) {
	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	wsURL := "ws" + strings.TrimPrefix(hubTS.URL, "http") + "/api/connect?node_id=artist"
	header := http.Header{}
	header.Set("Authorization", "Bearer node-token")
	header.Set(hubNodeIDHeader, "artist")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial reverse websocket: %v", err)
	}
	defer conn.Close()
	waitForReverseNode(t, s, "artist")

	requests := make(chan hubReverseRequest, 8)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var frame hubReverseRequest
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == hubReverseFrameRequest {
				requests <- frame
			}
		}
	}()

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan reverseDoResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/stream", nil).WithContext(firstCtx)
		resp, err := s.reverse.do(firstCtx, node, req)
		firstResult <- reverseDoResult{resp: resp, err: err}
	}()
	firstReq := waitForReverseRequest(t, requests, "/chat/stream")
	if err := conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: firstReq.ID, Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}}}); err != nil {
		t.Fatalf("write first response_start: %v", err)
	}
	first := waitForReverseDoResult(t, firstResult)
	if first.err != nil {
		t.Fatalf("first reverse do: %v", first.err)
	}
	defer first.resp.Body.Close()

	// Fill the first request's response queue until the websocket reader is
	// backpressured behind an undrained body. Canceling the request must unblock
	// that path so later multiplexed requests on the same websocket still work.
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < hubReversePendingBuffer+4; i++ {
			if err := conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseBody, ID: firstReq.ID, Body: []byte("chunk")}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	time.Sleep(100 * time.Millisecond)
	cancelFirst()
	_ = first.resp.Body.Close()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("write blocked body frames: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked reverse writer to unblock after cancel")
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSecond()
	secondResult := make(chan reverseDoResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/healthz", nil).WithContext(secondCtx)
		resp, err := s.reverse.do(secondCtx, node, req)
		secondResult <- reverseDoResult{resp: resp, err: err}
	}()
	secondReq := waitForReverseRequest(t, requests, "/chat/healthz")
	if err := conn.WriteJSON(hubReverseResponse{ID: secondReq.ID, Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: []byte("ok")}); err != nil {
		t.Fatalf("write second response: %v", err)
	}
	second := waitForReverseDoResult(t, secondResult)
	if second.err != nil {
		t.Fatalf("second reverse do after canceled stream: %v", second.err)
	}
	defer second.resp.Body.Close()
	body, err := io.ReadAll(second.resp.Body)
	if err != nil {
		t.Fatalf("read second body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("second body = %q", body)
	}
}

type reverseDoResult struct {
	resp *http.Response
	err  error
}

func waitForReverseDoResult(t *testing.T, ch <-chan reverseDoResult) reverseDoResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for reverse response")
		return reverseDoResult{}
	}
}

func waitForReverseRequest(t *testing.T, ch <-chan hubReverseRequest, wantPath string) hubReverseRequest {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case req := <-ch:
			if req.Path == wantPath {
				return req
			}
			t.Fatalf("reverse request path = %q, want %q", req.Path, wantPath)
		case <-deadline:
			t.Fatalf("timed out waiting for reverse request %s", wantPath)
		}
	}
}

func TestHubReverseRequestAllowsEncodedSlashInQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/v1/sessions/s1/file-changes/diff" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("path"); got != "var/www/discourse/Gemfile.lock" {
			t.Fatalf("query path = %q", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	var frames []hubReverseResponse
	handleHubReverseRequest(
		context.Background(),
		hubReverseRequest{
			ID:     "req_1",
			Method: http.MethodGet,
			Path:   "/chat/v1/sessions/s1/file-changes/diff?path=var%2Fwww%2Fdiscourse%2FGemfile.lock",
		},
		"node-token",
		backend.URL,
		"/chat",
		backend.Client(),
		func(resp hubReverseResponse) error {
			frames = append(frames, resp)
			return nil
		},
	)

	if len(frames) == 0 {
		t.Fatal("no reverse response frames written")
	}
	if frames[0].Error != "" {
		t.Fatalf("unexpected reverse error: %s", frames[0].Error)
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", frames[0].Status)
	}
}

func TestHubReverseConnectorReconnectsAfterDroppedWebsocket(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/healthz" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","agent":"artist"}`))
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())

	first := waitForReverseNodeConnection(t, s, "artist")
	assertReverseProxyContains(t, s, "/node/artist/healthz", `"agent":"artist"`)

	// Simulate a long-lived tunnel dying. The node-side connector should notice
	// the websocket read error, retry, and attach a fresh hub-side connection.
	_ = first.conn.Close()
	second := waitForReverseNodeReconnect(t, s, "artist", first)
	if second == first {
		t.Fatal("reverse connection pointer did not change after reconnect")
	}
	assertReverseProxyContains(t, s, "/node/artist/healthz", `"agent":"artist"`)
}

func TestHubReverseConnectorFailsInFlightRequestAndRecoversAfterDrop(t *testing.T) {
	slowStarted := make(chan struct{})
	slowCanceled := make(chan struct{})
	var slowStartedOnce, slowCanceledOnce sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/slow":
			slowStartedOnce.Do(func() { close(slowStarted) })
			<-r.Context().Done()
			slowCanceledOnce.Do(func() { close(slowCanceled) })
		case "/chat/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","agent":"artist"}`))
		default:
			t.Fatalf("backend path = %q", r.URL.Path)
		}
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())

	first := waitForReverseNodeConnection(t, s, "artist")
	inFlight := make(chan reverseDoResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/slow", nil)
		resp, err := s.reverse.do(ctx, node, req)
		inFlight <- reverseDoResult{resp: resp, err: err}
	}()
	waitForClosed(t, slowStarted, "slow backend request to start")

	_ = first.conn.Close()
	res := waitForReverseDoResult(t, inFlight)
	if res.err == nil {
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		t.Fatal("in-flight reverse request succeeded after tunnel drop; want error")
	}
	waitForClosed(t, slowCanceled, "slow backend request to be canceled")

	waitForReverseNodeReconnect(t, s, "artist", first)
	assertReverseProxyContains(t, s, "/node/artist/healthz", `"agent":"artist"`)
}

func TestHubReverseDelegationNodeJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/v2/jobs" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer node-token" {
			t.Fatalf("backend auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job_reverse"}`))
	}))
	defer backend.Close()

	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(s.handler())
	defer hubTS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, "artist", "node-token", backend.URL, "/chat", backend.Client())
	waitForReverseNode(t, s, "artist")

	var out struct {
		ID string `json:"id"`
	}
	if err := s.doNodeJSON(ctx, node, http.MethodPost, "/v2/jobs", map[string]string{"name": "demo"}, &out); err != nil {
		t.Fatalf("doNodeJSON: %v", err)
	}
	if out.ID != "job_reverse" {
		t.Fatalf("id = %q", out.ID)
	}
}

func assertReverseProxyContains(t *testing.T, s *hubServer, path, want string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d body=%q", path, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("%s body = %q, want substring %q", path, rec.Body.String(), want)
	}
}

func waitForClosed(t *testing.T, ch <-chan struct{}, desc string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", desc)
	}
}

func waitForReverseNodeConnection(t *testing.T, s *hubServer, nodeID string) *hubReverseConnection {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.reverse.mu.RLock()
		c := s.reverse.conns[nodeID]
		s.reverse.mu.RUnlock()
		if c != nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %q did not connect", nodeID)
	return nil
}

func waitForReverseNodeReconnect(t *testing.T, s *hubServer, nodeID string, old *hubReverseConnection) *hubReverseConnection {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s.reverse.mu.RLock()
		c := s.reverse.conns[nodeID]
		s.reverse.mu.RUnlock()
		if c != nil && c != old {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %q did not reconnect", nodeID)
	return nil
}

func waitForReverseNode(t *testing.T, s *hubServer, nodeID string) {
	t.Helper()
	_ = waitForReverseNodeConnection(t, s, nodeID)
}
