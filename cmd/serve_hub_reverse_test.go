package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

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

func waitForReverseNode(t *testing.T, s *hubServer, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.reverse.isConnected(nodeID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %q did not connect", nodeID)
}
