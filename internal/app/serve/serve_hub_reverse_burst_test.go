package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/term-llm/term-llm/internal/hub"
)

// Use observable admission state as a barrier, not a wall-clock sleep. The
// watchdog only bounds a broken test; expiry itself is tested with synctest.
func waitForHubAdmission(t *testing.T, c *hubReverseConnection, active, waiting int) {
	t.Helper()
	watchdog := time.After(5 * time.Second)
	for len(c.active) != active || len(c.waiting) != waiting {
		select {
		case <-watchdog:
			t.Fatalf("admission active=%d waiting=%d, want %d/%d", len(c.active), len(c.waiting), active, waiting)
		default:
			runtime.Gosched()
		}
	}
}

func TestHubReverseAdmissionHTTPAssetBurst(t *testing.T) {
	for _, count := range []int{8, 16, 17, 32, 64, 80} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			gate := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(gate) })
			started := make(chan struct{}, count)
			var current, peak atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := current.Add(1)
				defer current.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = io.WriteString(w, "asset:"+r.URL.Path)
			}))
			t.Cleanup(backend.Close)
			node := hub.Node{ID: "assets", Connection: "reverse", BasePath: "/chat", Token: "test-token"}
			s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
			ts := httptest.NewServer(s.handler())
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithCancel(t.Context())
			connectorDone := make(chan error, 1)
			go func() {
				connectorDone <- hubReverseConnectOnce(ctx, ts.URL, node.ID, node.Token, backend.URL, node.BasePath, backend.Client())
			}()
			c := waitForReverseNodeConnection(t, s, node.ID)
			t.Cleanup(func() {
				cancel()
				c.closeWithError(context.Canceled)
				_ = containmentAwait(t, connectorDone)
			})
			type result struct {
				status int
				body   string
				err    error
			}
			results := make(chan result, count)
			client := ts.Client()
			client.Timeout = 10 * time.Second
			for i := range count {
				go func() {
					path := fmt.Sprintf("/assets/avatar-%d.png", i)
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/node/assets"+path, nil)
					if err != nil {
						results <- result{err: err}
						return
					}
					resp, err := client.Do(req)
					if err != nil {
						results <- result{err: err}
						return
					}
					body, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err == nil && string(body) != "asset:/chat"+path {
						err = fmt.Errorf("unexpected asset body %q", body)
					}
					results <- result{resp.StatusCode, string(body), err}
				}()
			}
			for range min(count, hubReverseMaxActive) {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("first asset wave did not reach backend")
				}
			}
			waitForHubAdmission(t, c, min(count, hubReverseMaxActive), max(0, count-hubReverseMaxActive))
			release.Do(func() { close(gate) })
			for range count {
				got := <-results
				if got.err != nil || got.status != http.StatusOK {
					t.Errorf("asset status=%d body=%q err=%v", got.status, got.body, got.err)
				}
			}
			waitForHubAdmission(t, c, 0, 0)
			c.pendingMu.Lock()
			pending := len(c.pending)
			c.pendingMu.Unlock()
			if pending != 0 || peak.Load() > hubReverseMaxActive {
				t.Fatalf("pending=%d peak active backend=%d", pending, peak.Load())
			}
			t.Logf("%d/%d HTTP 200 assets, peak active backend=%d, final active=0 waiting=0 pending=0", count, count, peak.Load())
		})
	}
}

// Request bodies must not be consumed or buffered while waiting for admission.
type admissionRequestBody struct{ read atomic.Bool }

func (b *admissionRequestBody) Read([]byte) (int, error) { b.read.Store(true); return 0, io.EOF }
func (b *admissionRequestBody) Close() error             { return nil }

// Long downloads keep their request slots (event streams move to their own
// pool; see TestHubReverseEventStreamsUseOwnPool).
func TestHubReverseAdmissionLongStreams(t *testing.T) {
	for _, ending := range []string{"release", "cancel", "close"} {
		t.Run(ending, func(t *testing.T) {
			s, ts, node, peer, c := containmentHub(t)
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
					if frame.Path == "/stream" {
						resp.Type = hubReverseFrameResponseStart
						resp.Header = http.Header{"Content-Type": {"application/octet-stream"}}
					}
					if peer.WriteJSON(resp) != nil {
						return
					}
				}
			}()
			var streams []*http.Response
			defer func() {
				for _, resp := range streams {
					_ = resp.Body.Close()
				}
			}()
			for range hubReverseMaxActive {
				req := httptest.NewRequest(http.MethodGet, "http://reverse.local/stream", nil).WithContext(t.Context())
				resp, err := s.reverse.do(t.Context(), node, req)
				if err != nil {
					t.Fatal(err)
				}
				streams = append(streams, resp)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := &admissionRequestBody{}
			done := make(chan error, 1)
			go func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://reverse.local/barrier", body)
				resp, err := s.reverse.do(ctx, node, req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()
			waitForHubAdmission(t, c, hubReverseMaxActive, 1)
			if body.read.Load() || c.requestSeq.Load() != hubReverseMaxActive {
				t.Fatal("waiting request read its body or received a wire ID")
			}
			// Admission pressure is per node: another reverse tunnel and Hub
			// control traffic must still work while long streams hold all slots.
			healthyPeer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/connect?node_id=healthy", http.Header{"Authorization": {"Bearer good"}, hubNodeIDHeader: {"healthy"}})
			if err != nil {
				t.Fatal(err)
			}
			defer healthyPeer.Close()
			waitForReverseNode(t, s, "healthy")
			go func() {
				var frame hubReverseRequest
				if healthyPeer.ReadJSON(&frame) == nil {
					_ = healthyPeer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: 200})
				}
			}()
			healthy, _ := s.registry.Lookup("healthy")
			if err := containmentAwait(t, containmentDo(s, t.Context(), healthy, "/healthz", nil)); err != nil {
				t.Fatalf("healthy node under sibling admission pressure: %v", err)
			}
			control := httptest.NewRecorder()
			s.handler().ServeHTTP(control, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if control.Code != http.StatusOK {
				t.Fatalf("Hub health status=%d", control.Code)
			}
			switch ending {
			case "release":
				_ = streams[0].Body.Close()
			case "cancel":
				cancel()
			case "close":
				// Close the peer, not just the in-memory connection: exercise
				// read-loop teardown waking the admission waiter.
				_ = peer.Close()
			}
			err = containmentAwait(t, done)
			switch ending {
			case "release":
				if err != nil || !body.read.Load() {
					t.Fatalf("released stream did not admit waiter: err=%v body read=%v", err, body.read.Load())
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("waiting cancellation=%v", err)
				}
			case "close":
				if err == nil || s.reverse.isConnected(node.ID) || !s.reverse.isConnected(healthy.ID) {
					t.Fatalf("tunnel close did not isolate/wake waiter: %v", err)
				}
			}
			if ending != "release" && (body.read.Load() || c.requestSeq.Load() != hubReverseMaxActive) {
				t.Fatal("canceled/closed waiting request consumed a body or wire ID")
			}
			for _, resp := range streams {
				_ = resp.Body.Close()
			}
			waitForHubAdmission(t, c, 0, 0)
			c.pendingMu.Lock()
			pending := len(c.pending)
			c.pendingMu.Unlock()
			if pending != 0 {
				t.Fatalf("retained %d pending operations", pending)
			}
		})
	}
}

// A full tunnel is busy, not down: the proxy answers 503 with Retry-After so
// clients back off and retry, while other failures stay 502.
func TestHubReverseProxyReportsBusy(t *testing.T) {
	s, ts, node, peer, c := containmentHub(t)
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	var calls []<-chan error
	defer func() {
		cancel()
		for _, call := range calls {
			_ = containmentAwait(t, call)
		}
	}()
	for range hubReverseMaxActive + hubReverseMaxWaiting {
		calls = append(calls, containmentDo(s, ctx, node, "/silent", nil))
	}
	waitForHubAdmission(t, c, hubReverseMaxActive, hubReverseMaxWaiting)
	resp, err := ts.Client().Get(ts.URL + "/node/" + node.ID + "/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("full tunnel: status=%d Retry-After=%q body=%q", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	for _, err := range []error{context.Canceled, errors.New("reverse connection is closed")} {
		rec := httptest.NewRecorder()
		hubProxyErrorHandler(rec, httptest.NewRequest(http.MethodGet, "/node/x/", nil), err)
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "" {
			t.Fatalf("%v: status=%d Retry-After=%q", err, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
}
