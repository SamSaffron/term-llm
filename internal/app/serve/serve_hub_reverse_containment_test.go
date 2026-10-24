package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

// Observe a real socket write, without replacing network backpressure with a
// mocked writer. The peer completes its handshake, then stops reading entirely.
type containmentConn struct {
	net.Conn
	armed         atomic.Bool
	writing       chan struct{}
	deadlineLimit atomic.Int64
	afterWrite    atomic.Pointer[func()]
}

func (c *containmentConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		select {
		case c.writing <- struct{}{}:
		default:
		}
	}
	n, err := c.Conn.Write(p)
	if hook := c.afterWrite.Swap(nil); hook != nil {
		(*hook)()
	}
	return n, err
}

func (c *containmentConn) SetWriteDeadline(d time.Time) error {
	if limit := c.deadlineLimit.Load(); limit > 0 && !d.IsZero() {
		d = minDeadline(d, time.Now().Add(time.Duration(limit)))
	}
	return c.Conn.SetWriteDeadline(d)
}

func minDeadline(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

type containmentListener struct{ net.Listener }

func (l containmentListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4096)
	}
	return &containmentConn{Conn: c, writing: make(chan struct{}, 1)}, nil
}
func containmentHub(t *testing.T) (*hubServer, *httptest.Server, hub.Node, *websocket.Conn, *hubReverseConnection) {
	t.Helper()
	node := hub.Node{ID: "stalled", Connection: "reverse", BasePath: "/chat", Token: "token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node, {ID: "healthy", Connection: "reverse", BasePath: "/chat", Token: "good"}}}), nil)
	ts := httptest.NewUnstartedServer(s.handler())
	ts.Listener = containmentListener{ts.Listener}
	ts.Start()
	t.Cleanup(ts.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/connect?node_id="+node.ID, http.Header{"Authorization": {"Bearer token"}, hubNodeIDHeader: {node.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	waitForReverseNode(t, s, node.ID)
	s.reverse.mu.RLock()
	c := s.reverse.conns[node.ID]
	s.reverse.mu.RUnlock()
	return s, ts, node, peer, c
}
func containmentDo(s *hubServer, ctx context.Context, node hub.Node, path string, body []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "http://reverse.local"+path, bytes.NewReader(body)).WithContext(ctx)
		resp, err := s.reverse.do(ctx, node, req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	return done
}
func containmentAwait(t *testing.T, done <-chan error) error {
	t.Helper()
	return containmentAwaitWithin(t, done, 2*time.Second)
}
func containmentAwaitWithin(t *testing.T, done <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("operation did not finish within watchdog")
		return nil
	}
}
func containmentStall(t *testing.T, s *hubServer, node hub.Node, c *hubReverseConnection, ctx context.Context) <-chan error {
	t.Helper()
	socket := c.conn.UnderlyingConn().(*containmentConn)
	socket.armed.Store(true)
	done := containmentDo(s, ctx, node, "/upload", bytes.Repeat([]byte("x"), 16<<20))
	select {
	case <-socket.writing:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never entered socket")
	}
	select {
	case err := <-done:
		t.Fatalf("stop-reading peer did not hold writer: %v", err)
	default:
	}
	return done
}

func TestHubReverseContainmentQueuedCancellation(t *testing.T) {
	s, ts, node, peer, c := containmentHub(t)
	first := containmentStall(t, s, node, c, context.Background())
	healthyPeer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/connect?node_id=healthy", http.Header{"Authorization": {"Bearer good"}, hubNodeIDHeader: {"healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	defer healthyPeer.Close()
	waitForReverseNode(t, s, "healthy")
	go func() {
		var frame hubReverseRequest
		if healthyPeer.ReadJSON(&frame) == nil {
			_ = healthyPeer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: 200, Body: []byte(`{}`)})
		}
	}()
	if err := containmentAwait(t, containmentDo(s, t.Context(), hub.Node{ID: "healthy"}, "/healthz", nil)); err != nil {
		t.Fatalf("healthy reverse peer: %v", err)
	}
	for _, path := range []string{"/healthz", "/api/nodes?view=sidebar"} {
		controlCtx, controlCancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer controlCancel()
		req, _ := http.NewRequestWithContext(controlCtx, http.MethodGet, ts.URL+path, nil)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := containmentDo(s, ctx, node, "/expired", nil)
	// Wait for registration, not an arbitrary sleep, to prove this request was
	// admitted behind the occupied writer before we cancel its context.
	watchdog := time.After(2 * time.Second)
	for {
		c.pendingMu.Lock()
		count := len(c.pending)
		c.pendingMu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-watchdog:
			t.Fatal("queued request was never registered")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	if err := containmentAwait(t, queued); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", err)
	}
	// The canceled caller must finish while the original socket writer remains
	// blocked, not merely when that writer's finite size-scaled deadline fires.
	select {
	case err := <-first:
		t.Fatalf("writer released before cancellation returned: %v", err)
	default:
	}
	// Resume reading and use a subsequent successful round trip as a wire
	// ordering barrier: no expired request may have been sent in between.
	paths := make(chan string, 4)
	if tcp, ok := c.conn.UnderlyingConn().(*containmentConn).Conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(1 << 20)
	}
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
			if frame.Type != hubReverseFrameRequest {
				continue
			}
			paths <- frame.Path
			_ = peer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: 200, Body: []byte(`{}`)})
		}
	}()
	// Draining the 16 MiB stall payload is cleanup, not the property under test;
	// it takes ~0.9s under -race on a fast host, so allow slow CI runners.
	if err := containmentAwaitWithin(t, first, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := containmentAwait(t, containmentDo(s, context.Background(), node, "/barrier", nil)); err != nil {
		t.Fatal(err)
	}
	if path := <-paths; path != "/upload" {
		t.Fatalf("first path=%q", path)
	}
	if path := <-paths; path != "/barrier" {
		t.Fatalf("expired work reached wire: %q", path)
	}
}

func TestHubReverseContainmentWriteDeadlineDetaches(t *testing.T) {
	s, _, node, _, c := containmentHub(t)
	// A real socket deadline failure still poisons Gorilla and must detach the
	// entire tunnel. Caller cancellation alone no longer interrupts a message.
	c.conn.UnderlyingConn().(*containmentConn).deadlineLimit.Store(int64(50 * time.Millisecond))
	first := containmentStall(t, s, node, c, context.Background())
	if err := containmentAwait(t, first); err == nil {
		t.Fatal("expired socket write succeeded")
	}
	if s.reverse.isConnected(node.ID) {
		t.Fatal("failed reverse connection remains attached")
	}
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("failed connection retained %d pending requests", pending)
	}
}

func TestHubReverseContainmentUndrainedBodyCancellation(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var frame hubReverseRequest
		if peer.ReadJSON(&frame) == nil {
			_ = peer.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: frame.ID, Status: 200})
		}
	}()
	req := httptest.NewRequest(http.MethodGet, "http://reverse.local/stream", nil).WithContext(ctx)
	resp, err := s.reverse.do(ctx, node, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	c.pendingMu.Lock()
	var pending *hubReversePending
	for _, p := range c.pending {
		pending = p
	}
	c.pendingMu.Unlock()
	if pending == nil {
		t.Fatal("stream has no pending operation")
	}
	// Enqueue synchronously so observing queuedBytes==0 means the copier has
	// consumed this frame and entered (or is about to enter) its pipe write.
	// No reader will drain that write until cancellation has already finished.
	if queued, _ := pending.enqueue(hubReverseResponse{Type: hubReverseFrameResponseBody, Body: []byte("unread body")}); !queued {
		t.Fatal("body frame not queued")
	}
	watchdog := time.After(2 * time.Second)
	for pending.queuedBytes.Load() != 0 {
		select {
		case <-watchdog:
			t.Fatal("copier did not consume body frame")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation failed to interrupt an undrained pipe writer")
	}
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	if err := containmentAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("body cancellation=%v", err)
	}
	c.pendingMu.Lock()
	count := len(c.pending)
	c.pendingMu.Unlock()
	if count != 0 {
		t.Fatalf("canceled stream retained %d pending requests", count)
	}
}

func TestHubReverseContainmentBodyCloseWithoutFrames(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	go func() {
		var frame hubReverseRequest
		if peer.ReadJSON(&frame) == nil {
			_ = peer.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: frame.ID, Status: 200})
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://reverse.local/stream", nil).WithContext(ctx)
	resp, err := s.reverse.do(ctx, node, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	c.pendingMu.Lock()
	count := len(c.pending)
	c.pendingMu.Unlock()
	if count != 0 {
		t.Fatalf("Body.Close retained %d pending operations waiting for more frames", count)
	}
}

func TestHubReverseContainmentAdmissionBound(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	requests := make(chan struct{}, 32)
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
			if frame.Type == hubReverseFrameRequest {
				requests <- struct{}{}
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []<-chan error
	for i := 0; i < 16; i++ {
		calls = append(calls, containmentDo(s, ctx, node, "/silent", nil))
		select {
		case <-requests:
		case <-time.After(2 * time.Second):
			t.Fatal("admitted request not sent")
		}
	}
	if err := containmentAwait(t, containmentDo(s, ctx, node, "/overflow", nil)); err == nil {
		t.Fatal("connection admitted more than its bounded outstanding work")
	}
	cancel()
	for _, call := range calls {
		if err := containmentAwait(t, call); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request=%v", err)
		}
	}
	c.pendingMu.Lock()
	count := len(c.pending)
	c.pendingMu.Unlock()
	if count != 0 {
		t.Fatalf("cancellation retained %d pending requests", count)
	}
}
