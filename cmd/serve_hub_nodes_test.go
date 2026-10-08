package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

func hubNodeTestResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

const hubNodeTestSessions = `{"sessions":[{"id":"healthy-session","short_title":"Healthy","active_run":true}]}`

func TestHubCollectNodesIsolatesDirectWorkflows(t *testing.T) {
	for _, blockedPath := range []string{"/chat/healthz", "/chat/v1/sessions/status"} {
		t.Run(blockedPath, func(t *testing.T) {
			nodes := []hub.Node{
				{ID: "slow", URL: "http://slow", BasePath: "/chat"},
				{ID: "healthy", URL: "http://healthy", BasePath: "/chat", Token: "healthy-token"},
			}
			s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
			s.basePath = "/hub"
			s.nodeOrder = hub.NewNodeOrderStore(filepath.Join(t.TempDir(), "order.json"))
			if _, err := s.nodeOrder.Reorder(nodes, []string{"slow", "healthy"}); err != nil {
				t.Fatal(err)
			}
			blocked := make(chan struct{})
			release := make(chan struct{})
			healthySessions := make(chan struct{})
			var healthyHealthCtx context.Context
			s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 3*time.Second {
					t.Errorf("request lacks bounded stage budget: %s", r.URL)
				}
				if r.URL.Host == "slow" && r.URL.Path == blockedPath {
					close(blocked)
					select {
					case <-release:
						return nil, context.DeadlineExceeded
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				if r.URL.Path == "/chat/healthz" {
					if r.URL.Host == "healthy" {
						healthyHealthCtx = r.Context()
					}
					return hubHealthTestResponse(), nil
				}
				if r.URL.Host == "healthy" && r.URL.Path == "/chat/v1/sessions/status" {
					// The canceled health context must not be reused for sessions.
					if healthyHealthCtx.Err() != context.Canceled || r.Context().Err() != nil {
						t.Errorf("health and session stages share cancellation: health=%v session=%v", healthyHealthCtx.Err(), r.Context().Err())
					}
					if r.Header.Get("Authorization") != "Bearer healthy-token" {
						t.Error("missing healthy node session authorization")
					}
					close(healthySessions)
				}
				return hubNodeTestResponse(hubNodeTestSessions), nil
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var views []hubNodeView
			var err error
			go func() {
				defer close(finished)
				views, err = s.collectNodes(ctx)
			}()
			defer func() { cancel(); waitHubHealthSignal(t, finished) }()
			waitHubHealthSignal(t, blocked)
			// This barrier is reached while the slow node is still blocked.
			// Batch health collection would prevent the healthy session request.
			waitHubHealthSignal(t, healthySessions)
			close(release)
			waitHubHealthSignal(t, finished)
			if err != nil || len(views) != 2 || views[0].ID != "slow" || views[1].ID != "healthy" {
				t.Fatalf("saved node order changed: views=%+v err=%v", views, err)
			}
			assertHubHealthyNode(t, views[1])
			if views[0].Sessions != nil {
				t.Fatalf("failed slow node unexpectedly has sessions: %+v", views[0])
			}
			if blockedPath == "/chat/healthz" && views[0].Status.Reachable {
				t.Fatalf("failed health probe reported reachable: %+v", views[0].Status)
			}
			if blockedPath != "/chat/healthz" && !views[0].Status.Reachable {
				t.Fatalf("session failure discarded health: %+v", views[0].Status)
			}
			assertDiagnosticCode(t, views[0], "missing_token")
		})
	}
}

func assertHubHealthyNode(t *testing.T, view hubNodeView) {
	t.Helper()
	if !view.Status.Reachable || view.Status.Agent != "test" {
		t.Fatalf("healthy node lost health: %+v", view.Status)
	}
	if view.Sessions == nil || view.Sessions.CountLabel != "1 session" || view.Sessions.ActiveCount != 1 ||
		view.Sessions.ResumePath != "/hub/node/healthy/chat/healthy-session" {
		t.Fatalf("healthy node lost session summary: %+v", view.Sessions)
	}
}

// attachHubNodeTestReverse uses real websocket framing but an explicit attach
// barrier, so tests don't poll for connection readiness or depend on sleeps.
func attachHubNodeTestReverse(t *testing.T, s *hubServer, n hub.Node, respond func(hubReverseRequest) hubReverseResponse) {
	t.Helper()
	attached := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade test reverse connection: %v", err)
			return
		}
		s.reverse.attach(n, conn)
		close(attached)
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitHubHealthSignal(t, attached)
	go func() {
		for {
			var req hubReverseRequest
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			if req.Type != hubReverseFrameRequest {
				continue
			}
			resp := respond(req)
			resp.ID = req.ID
			if err := conn.WriteJSON(resp); err != nil {
				return
			}
		}
	}()
}

func TestHubCollectNodesIsolatesReverseWorkflows(t *testing.T) {
	for _, healthyConnection := range []string{"direct", "reverse"} {
		t.Run(healthyConnection, func(t *testing.T) {
			nodes := []hub.Node{
				{ID: "slow", Connection: "reverse", URL: "http://slow-direct", BasePath: "/chat", Token: "slow-token"},
				{ID: "healthy", Connection: healthyConnection, URL: "http://healthy", BasePath: "/chat"},
			}
			s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
			s.basePath = "/hub"
			blocked := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseSlow := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseSlow()
			healthySessions := make(chan struct{})
			s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "healthy" || healthyConnection != "direct" {
					t.Errorf("reverse node was directly probed: %s", r.URL)
					return nil, context.Canceled
				}
				if r.URL.Path == "/chat/healthz" {
					return hubHealthTestResponse(), nil
				}
				close(healthySessions)
				return hubNodeTestResponse(hubNodeTestSessions), nil
			}))
			attachHubNodeTestReverse(t, s, nodes[0], func(req hubReverseRequest) hubReverseResponse {
				if req.Path == "/chat/healthz" {
					close(blocked)
					<-release
					return hubReverseResponse{Error: "bad node health"}
				}
				return hubReverseResponse{Status: http.StatusServiceUnavailable}
			})
			if healthyConnection == "reverse" {
				attachHubNodeTestReverse(t, s, nodes[1], func(req hubReverseRequest) hubReverseResponse {
					if req.Path == "/chat/healthz" {
						return hubReverseResponse{Status: http.StatusOK, Body: []byte(`{"agent":"test"}`)}
					}
					close(healthySessions)
					return hubReverseResponse{Status: http.StatusOK, Body: []byte(hubNodeTestSessions)}
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var views []hubNodeView
			var err error
			go func() {
				defer close(finished)
				views, err = s.collectNodes(ctx)
			}()
			defer func() { cancel(); waitHubHealthSignal(t, finished) }()
			waitHubHealthSignal(t, blocked)
			waitHubHealthSignal(t, healthySessions)
			// Let the bad node finish only after healthy session collection
			// has started, without racing parent cancellation against decoding.
			releaseSlow()
			waitHubHealthSignal(t, finished)
			if err != nil || len(views) != 2 {
				t.Fatalf("nodes=%+v err=%v", views, err)
			}
			byID := map[string]hubNodeView{}
			for _, view := range views {
				byID[view.ID] = view
			}
			assertHubHealthyNode(t, byID["healthy"])
			if !byID["slow"].Status.Reachable || byID["slow"].Status.State != "connected" || byID["slow"].Status.Error == "" {
				t.Fatalf("reverse failure changed connected best-effort semantics: %+v", byID["slow"].Status)
			}
		})
	}
}

func TestHubReverseHealthBodyBoundAndBestEffort(t *testing.T) {
	for _, body := range []string{
		`{"agent":"partial","capabilities":`,
		`{"agent":"` + strings.Repeat("a", hub.HealthResponseMaxBytes) + `"}`,
		`{"agent":"partial","unknown":"` + strings.Repeat("a", hub.HealthResponseMaxBytes) + `"}`,
	} {
		t.Run(body[:20], func(t *testing.T) {
			n := hub.Node{ID: "reverse", Connection: "reverse", BasePath: "/chat"}
			s := newHubServer(nil, nil)
			attachHubNodeTestReverse(t, s, n, func(hubReverseRequest) hubReverseResponse {
				return hubReverseResponse{Status: http.StatusOK, Body: []byte(body)}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			st := s.probeReverseNode(ctx, n, time.Unix(1, 0), time.Unix(2, 0))
			if !st.Reachable || st.State != "connected" || st.Error != "" || st.Details["connection"] != "reverse" {
				t.Fatalf("invalid health JSON changed reverse connection status: %+v", st)
			}
			if st.Agent != "" || st.Version != "" || len(st.Capabilities) != 0 {
				t.Fatalf("invalid health JSON published partial identity: %+v", st)
			}
		})
	}
}

func TestHubCollectNodesReturnsWithoutNoncooperativeTransport(t *testing.T) {
	for _, blockedPath := range []string{"/chat/healthz", "/chat/v1/sessions/status"} {
		t.Run(blockedPath, func(t *testing.T) {
			n := hub.Node{ID: "stuck", URL: "http://stuck", BasePath: "/chat"}
			s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{n}}), nil)
			started := make(chan struct{})
			release := make(chan struct{})
			transportFinished := make(chan struct{})
			var releaseOnce sync.Once
			releaseTransport := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseTransport()
			s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == blockedPath {
					close(started)
					// Deliberately ignore the request context. Production
					// transports honor it, but aggregation must not require it.
					<-release
					defer close(transportFinished)
					return nil, context.Canceled
				}
				return hubHealthTestResponse(), nil
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var views []hubNodeView
			var err error
			go func() {
				defer close(finished)
				views, err = s.collectNodes(ctx)
			}()
			waitHubHealthSignal(t, started)
			cancel()
			waitHubHealthSignal(t, finished)
			if err != nil || len(views) != 1 || views[0].ID != n.ID || views[0].Status.State != "unreachable" || views[0].Status.Error != context.Canceled.Error() {
				t.Fatalf("missing timeout placeholder: views=%+v err=%v", views, err)
			}
			assertDiagnosticCode(t, views[0], "missing_token")
			releaseTransport()
			waitHubHealthSignal(t, transportFinished)
			// A late result must never mutate a view that the caller owns.
			if views[0].Status.Error != context.Canceled.Error() || views[0].Sessions != nil {
				t.Fatalf("late worker changed returned placeholder: %+v", views[0])
			}
		})
	}
}

func TestHubCollectNodesPreservesAttentionWithBadHealth(t *testing.T) {
	nodes := []hub.Node{
		{ID: "bad", URL: "http://bad", BasePath: "/chat"},
		{ID: "healthy", URL: "http://healthy", BasePath: "/chat"},
	}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
	s.basePath = "/hub"
	projection, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	s.attentionStore = projection
	if err := projection.ReplaceNode(context.Background(), "healthy", "store", "", []hub.SessionActivity{
		{SessionID: "needs-input", Kind: "input_required"},
		{SessionID: "unseen", Kind: "terminal_unseen"},
	}); err != nil {
		t.Fatal(err)
	}
	s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/chat/healthz" {
			if r.URL.Host == "bad" {
				return hubNodeTestResponse(`{"agent":"` + strings.Repeat("a", hub.HealthResponseMaxBytes) + `"}`), nil
			}
			return hubHealthTestResponse(), nil
		}
		if r.URL.Host == "bad" {
			return hubNodeTestResponse(`{"sessions":`), nil
		}
		return hubNodeTestResponse(hubNodeTestSessions), nil
	}))
	views, err := s.collectNodes(context.Background())
	if err != nil || len(views) != 2 {
		t.Fatalf("nodes=%+v err=%v", views, err)
	}
	byID := map[string]hubNodeView{}
	for _, view := range views {
		byID[view.ID] = view
	}
	assertHubHealthyNode(t, byID["healthy"])
	sessions := byID["healthy"].Sessions
	if sessions.InputRequiredCount != 1 || sessions.UnseenCount != 1 || sessions.AttentionCapability != string(hub.AttentionSupported) || sessions.AttentionLastSuccessAt == 0 {
		t.Fatalf("cached attention lost after bad node response: %+v", sessions)
	}
	if !byID["bad"].Status.Reachable || byID["bad"].Status.Agent != "" {
		t.Fatalf("oversized JSON changed best-effort health: %+v", byID["bad"].Status)
	}
}
