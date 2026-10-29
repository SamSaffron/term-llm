package serve

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func (p *generationSecurityPeer) reconnect(t *testing.T, token string) *generationSecurityPeer {
	t.Helper()
	conn, _, err := p.dial(token)
	if err != nil {
		t.Fatalf("connect replacement peer: %v", err)
	}
	fresh := &generationSecurityPeer{hub: p.hub, server: p.server, conn: conn, done: make(chan struct{}), pong: make(chan struct{}, 1)}
	fresh.startReader(t)
	fresh.observed(t) // pong processing proves the replacement was attached
	return fresh
}

func (p *generationSecurityPeer) assertResponse(t *testing.T, suffix, token string) {
	t.Helper()
	status, body := p.request(t, http.MethodGet, "/node/"+generationSecurityNodeID+suffix, generationSecurityHubToken, "")
	frames := p.observed(t)
	if status != http.StatusOK || body != generationSecurityPeerBody || len(frames) == 0 {
		t.Fatalf("current peer response: status=%d body=%q frames=%d", status, body, len(frames))
	}
	frame := frames[len(frames)-1]
	if frame.Path != "/chat"+suffix || frame.Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("current peer received wrong path or token: %+v", frame)
	}
}

func TestHubReverseGenerationRegistrationControls(t *testing.T) {
	p := newGenerationSecurityPeer(t)
	p.assertControl(t)
	p.register(t, generationSecurityOldToken, http.StatusOK)
	p.assertResponse(t, "/same-token-registration", generationSecurityOldToken)

	// A different node, even with the same credential, has independent authority.
	other := hub.Node{ID: "other-node", Name: "Other", Connection: "reverse", BasePath: "/chat", Token: generationSecurityOldToken}
	if _, _, err := p.hub.upsertHubNode(other); err != nil {
		t.Fatal(err)
	}
	other.Token = generationSecurityNewToken
	if _, _, err := p.hub.upsertHubNode(other); err != nil {
		t.Fatal(err)
	}
	if err := p.hub.removeHubNode(other.ID); err != nil {
		t.Fatal(err)
	}
	// Failed mutations also leave the original socket alone.
	if err := p.hub.removeHubNode("missing-node"); err == nil {
		t.Fatal("removing missing node succeeded")
	}
	p.assertResponse(t, "/unrelated-mutations", generationSecurityOldToken)
}

func TestHubReverseGenerationBothDeleteHandlersRevokeImmediately(t *testing.T) {
	for _, route := range []string{"/api/register-node/", "/api/nodes/"} {
		t.Run(route, func(t *testing.T) {
			p := newGenerationSecurityPeer(t)
			p.assertControl(t)
			token := generationSecurityRegToken
			if route == "/api/nodes/" {
				token = generationSecurityHubToken
			}
			status, body := p.request(t, http.MethodDelete, route+generationSecurityNodeID, token, "")
			if status != http.StatusOK {
				t.Fatalf("delete status=%d body=%q", status, body)
			}
			waitForClosed(t, p.done, "proactive deletion revocation, without another lookup")
			p.register(t, generationSecurityOldToken, http.StatusCreated)
			p.assertNotForwarded(t, http.MethodGet, "/after-deletion", generationSecurityHubToken, generationSecurityOldToken)
			fresh := p.reconnect(t, generationSecurityOldToken)
			fresh.assertResponse(t, "/fresh-lifetime", generationSecurityOldToken)
		})
	}
}

func TestHubReverseGenerationUIAddCannotReuseDeletedSocket(t *testing.T) {
	p := newGenerationSecurityPeer(t)
	old, _ := p.hub.registry.Lookup(generationSecurityNodeID)
	status, _ := p.request(t, http.MethodDelete, "/api/nodes/"+generationSecurityNodeID, generationSecurityHubToken, "")
	if status != http.StatusOK {
		t.Fatal(status)
	}
	waitForClosed(t, p.done, "UI deletion revocation")
	// UI Add creates a direct node. It must not inherit the deleted reverse
	// transport even when the ID and token match the former registration.
	payload := `{"id":"` + generationSecurityNodeID + `","url":"http://127.0.0.1:1/chat","token":"` + generationSecurityOldToken + `"}`
	status, body := p.request(t, http.MethodPost, "/api/nodes", generationSecurityHubToken, payload)
	if status != http.StatusCreated {
		t.Fatalf("UI add status=%d body=%q", status, body)
	}
	current, _ := p.hub.registry.Lookup(generationSecurityNodeID)
	if current.UsesReverseConnection() || old.SameIdentity(current) {
		t.Fatal("UI Add retained the deleted reverse identity")
	}
	if _, err := p.hub.reverse.connection(old); err == nil {
		t.Fatal("UI Add made old reverse identity usable")
	}
}

type generationSecurityHijackBarrier struct {
	http.ResponseWriter
	entered chan struct{}
	release chan struct{}
}

func (w *generationSecurityHijackBarrier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	close(w.entered) // authenticateNode has succeeded; Upgrade has not attached
	<-w.release
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func TestHubReverseGenerationAuthenticatedUpgradeCannotAttachAfterMutation(t *testing.T) {
	for _, sameToken := range []bool{false, true} {
		name := "rotation"
		if sameToken {
			name = "delete-reregister-same-token"
		}
		t.Run(name, func(t *testing.T) {
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			p := newGenerationSecurityPeer(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/connect" && armed.CompareAndSwap(true, false) {
						defer close(returned)
						w = &generationSecurityHijackBarrier{ResponseWriter: w, entered: entered, release: release}
					}
					next.ServeHTTP(w, r)
				})
			})
			// Release a blocked handler before peer/server cleanup on failures.
			t.Cleanup(unblock)
			armed.Store(true)
			dialDone := make(chan error, 1)
			go func() {
				conn, resp, err := p.dial(generationSecurityOldToken)
				if conn != nil {
					defer conn.Close()
					_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
					_, _, err = conn.ReadMessage() // rejected upgrade must close
				}
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				dialDone <- err
			}()
			waitForClosed(t, entered, "handshake after authentication, before upgrade")
			token, wantStatus := generationSecurityNewToken, http.StatusOK
			if sameToken {
				status, _ := p.request(t, http.MethodDelete, "/api/register-node/"+generationSecurityNodeID, generationSecurityRegToken, "")
				if status != http.StatusOK {
					t.Fatal(status)
				}
				token, wantStatus = generationSecurityOldToken, http.StatusCreated
			}
			p.register(t, token, wantStatus)
			waitForClosed(t, p.done, "proactive original-socket revocation")
			fresh := p.reconnect(t, token)
			unblock()
			waitForClosed(t, returned, "stale upgrade attach rejection")
			select {
			case err := <-dialDone:
				var timeout net.Error
				if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
					t.Fatalf("stale authenticated upgrade was not closed: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stale upgrade did not finish")
			}
			fresh.assertResponse(t, "/after-stale-attach", token)
		})
	}
}

type generationSecurityBodyBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *generationSecurityBodyBarrier) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}
func (*generationSecurityBodyBarrier) Close() error { return nil }

func TestHubReverseGenerationAdmittedRequestCannotMigrateToReplacement(t *testing.T) {
	p := newGenerationSecurityPeer(t)
	old, _ := p.hub.registry.Lookup(generationSecurityNodeID)
	body := &generationSecurityBodyBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(body.release) }) }
	t.Cleanup(unblock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "http://reverse.local/chat/admitted-before-rotation", body).WithContext(ctx)
		resp, err := p.hub.reverse.do(ctx, old, req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	waitForClosed(t, body.entered, "old request admitted before body read")
	p.register(t, generationSecurityNewToken, http.StatusOK)
	waitForClosed(t, p.done, "rotation closes admitted request transport")
	fresh := p.reconnect(t, generationSecurityNewToken)
	unblock()
	if err := containmentAwait(t, done); err == nil {
		t.Fatal("request admitted to revoked socket succeeded")
	}
	// A stale request presented AFTER replacement must also not be reselected
	// by ID onto the replacement's current socket.
	req := httptest.NewRequest(http.MethodGet, "http://reverse.local/chat/stale-snapshot", nil)
	if resp, err := p.hub.reverse.do(ctx, old, req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("stale snapshot routed to replacement socket")
	}
	if frames := fresh.observed(t); len(frames) != 0 {
		t.Fatalf("replacement received stale requests: %+v", frames)
	}
	fresh.assertResponse(t, "/current-request", generationSecurityNewToken)
}

type generationSecurityResolver struct {
	mu   sync.Mutex
	node hub.Node
}

func (*generationSecurityResolver) Source() string { return hub.SourceConfig }
func (r *generationSecurityResolver) Nodes() ([]hub.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []hub.Node{r.node}, nil
}

func TestHubReverseGenerationQueuedWriterRevalidatesDynamicResolver(t *testing.T) {
	resolver := &generationSecurityResolver{node: hub.Node{
		ID: generationSecurityNodeID, Connection: "reverse", BasePath: "/chat", Token: generationSecurityOldToken,
	}}
	s := newHubServer(hub.NewRegistry(resolver), nil)
	p := &generationSecurityPeer{hub: s, server: httptest.NewServer(s.handler())}
	t.Cleanup(p.server.Close)
	fresh := p.reconnect(t, generationSecurityOldToken)
	c, err := p.hub.reverse.connection(mustGenerationSecurityNode(t, p.hub))
	if err != nil {
		t.Fatal(err)
	}
	c.writeGate <- struct{}{}
	done := make(chan error, 1)
	go func() {
		done <- c.writeFrame(t.Context(), hubReverseRequest{
			Type: hubReverseFrameRequest, ID: "queued-stale", Method: http.MethodGet, Path: "/chat/queued",
			Header: http.Header{"Authorization": {"Bearer " + generationSecurityNewToken}},
		})
	}()
	resolver.mu.Lock()
	resolver.node.Token = generationSecurityNewToken
	resolver.mu.Unlock()
	// No registry read/mutation notification occurred: the final write-gate
	// validation must discover the external configuration credential change.
	<-c.writeGate
	if err := containmentAwait(t, done); err == nil {
		t.Fatal("queued writer sent request after dynamic resolver token rotation")
	}
	if frames := fresh.observed(t); len(frames) != 0 {
		t.Fatalf("revoked configuration socket received request/current token: %+v", frames)
	}
	waitForClosed(t, fresh.done, "dynamic resolver revocation")
}

func mustGenerationSecurityNode(t *testing.T, s *hubServer) hub.Node {
	t.Helper()
	node, ok := s.registry.Lookup(generationSecurityNodeID)
	if !ok {
		t.Fatal("test node missing")
	}
	return node
}
