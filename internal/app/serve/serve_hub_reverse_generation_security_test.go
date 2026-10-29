package serve

import (
	"encoding/json"
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

const (
	generationSecurityNodeID   = "generation-node"
	generationSecurityOldToken = "test-old-node-token"
	generationSecurityNewToken = "test-replacement-node-token"
	generationSecurityHubToken = "test-hub-operator-token"
	generationSecurityRegToken = "test-registration-token"
	generationSecurityPeerBody = "original-reverse-peer-response"
)

// These tests use only loopback HTTP/WebSocket traffic and temporary node stores.
// The peer records requests before replying, and a WebSocket ping/pong fences
// observations after each HTTP round trip, without sleeps or negative timeouts.
// Deadlines below are failure guards, not evidence that no request was sent.
type generationSecurityPeer struct {
	hub    *hubServer
	server *httptest.Server
	conn   *websocket.Conn
	done   chan struct{}
	pong   chan struct{}
	mu     sync.Mutex
	frames []hubReverseRequest
}

func newGenerationSecurityPeer(t *testing.T, wrappers ...func(http.Handler) http.Handler) *generationSecurityPeer {
	t.Helper()
	store := hub.NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	s := newHubServer(hub.NewRegistry(store), store)
	s.requireAuth = true
	s.token = generationSecurityHubToken
	s.registrationToken = generationSecurityRegToken

	attached := make(chan struct{})
	var attachedOnce sync.Once
	handler := s.handler()
	for _, wrap := range wrappers {
		handler = wrap(handler)
	}
	p := &generationSecurityPeer{hub: s, done: make(chan struct{}), pong: make(chan struct{}, 1)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.URL.Path == "/api/connect" {
			// The upgrade response can reach Dial before attach finishes. Signal
			// handler completion instead of polling the reverse manager's map.
			attachedOnce.Do(func() { close(attached) })
		}
	}))
	p.server.Client().Timeout = 5 * time.Second
	t.Cleanup(p.server.Close)
	p.register(t, generationSecurityOldToken, http.StatusCreated)

	var err error
	p.conn, _, err = p.dial(generationSecurityOldToken)
	if err != nil {
		t.Fatalf("connect original reverse peer: %v", err)
	}
	p.startReader(t)
	waitForClosed(t, attached, "authenticated reverse attach")
	return p
}

func (p *generationSecurityPeer) startReader(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = p.conn.Close()
		waitForClosed(t, p.done, "security peer reader shutdown")
	})
	p.conn.SetPongHandler(func(string) error {
		p.pong <- struct{}{}
		return nil
	})
	go func() {
		defer close(p.done)
		for {
			var frame hubReverseRequest
			if err := p.conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type != hubReverseFrameRequest {
				continue
			}
			p.mu.Lock()
			p.frames = append(p.frames, frame)
			p.mu.Unlock()
			// A malicious old node need not validate the newly injected token.
			if err := p.conn.WriteJSON(hubReverseResponse{
				ID: frame.ID, Status: http.StatusOK,
				Header: http.Header{"Content-Type": {"text/plain"}},
				Body:   []byte(generationSecurityPeerBody),
			}); err != nil {
				return
			}
		}
	}()
}

func (p *generationSecurityPeer) dial(token string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	header.Set(hubNodeIDHeader, generationSecurityNodeID)
	header.Set("Authorization", "Bearer "+token)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	return dialer.Dial("ws"+strings.TrimPrefix(p.server.URL, "http")+"/api/connect", header)
}

func (p *generationSecurityPeer) request(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, p.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(data)
}

func (p *generationSecurityPeer) register(t *testing.T, token string, wantStatus int) {
	t.Helper()
	body, err := json.Marshal(hubRegisterNodeRequest{
		ID: generationSecurityNodeID, Name: "Generation security test",
		Connection: "reverse", BasePath: "/chat", Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, response := p.request(t, http.MethodPost, "/api/register-node", generationSecurityRegToken, string(body))
	if status != wantStatus {
		t.Fatalf("registration status = %d, want %d; body=%q", status, wantStatus, response)
	}
}

func (p *generationSecurityPeer) observed(t *testing.T) []hubReverseRequest {
	t.Helper()
	// The Hub's pong is ordered after previously written request frames. A
	// securely revoked/closed socket is also a completed observation barrier.
	_ = p.conn.WriteControl(websocket.PingMessage, []byte("security-observation-barrier"), time.Now().Add(5*time.Second))
	select {
	case <-p.pong:
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reverse peer did not reach observation barrier")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]hubReverseRequest(nil), p.frames...)
}

func (p *generationSecurityPeer) assertControl(t *testing.T) {
	t.Helper()
	status, body := p.request(t, http.MethodGet, "/node/"+generationSecurityNodeID+"/control", generationSecurityHubToken, "")
	frames := p.observed(t)
	if status != http.StatusOK || body != generationSecurityPeerBody || len(frames) != 1 {
		t.Fatalf("authenticated connected-node control: status=%d body=%q frames=%d", status, body, len(frames))
	}
	frame := frames[0]
	if frame.Method != http.MethodGet || frame.Path != "/chat/control" || frame.Header.Get("Authorization") != "Bearer "+generationSecurityOldToken {
		t.Fatalf("connected-node control received unexpected method, path, or credential: %+v", frame)
	}
}

func (p *generationSecurityPeer) assertNotForwarded(t *testing.T, method, suffix, hubToken, forbiddenToken string) {
	t.Helper()
	before := len(p.observed(t))
	status, body := p.request(t, method, "/node/"+generationSecurityNodeID+suffix, hubToken, "")
	for _, frame := range p.observed(t)[before:] {
		t.Errorf("security invariant: original reverse socket received forbidden %s %s (HTTP status=%d)", frame.Method, frame.Path, status)
		if forbiddenToken != "" && frame.Header.Get("Authorization") == "Bearer "+forbiddenToken {
			t.Errorf("security invariant: original reverse socket received protected node token in %s %s", frame.Method, frame.Path)
		}
	}
	if body == generationSecurityPeerBody {
		t.Errorf("security invariant: forbidden %s returned original reverse peer response (HTTP status=%d)", method, status)
	}
}

func TestHubReverseGenerationSecurityCurrentConnectionControl(t *testing.T) {
	p := newGenerationSecurityPeer(t)
	p.assertControl(t)
	p.assertNotForwarded(t, http.MethodGet, "/unauthenticated-get", "", "")
}

func (p *generationSecurityPeer) assertTokenRejected(t *testing.T, token string) {
	t.Helper()
	conn, resp, err := p.dial(token)
	if conn != nil {
		_ = conn.Close()
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token handshake: error=%v response=%v; want HTTP 401", err, resp)
	}
}

func TestHubReverseGenerationSecurityStaleConnection(t *testing.T) {
	for _, mutation := range []struct {
		name        string
		deleteFirst bool
		token       string
	}{
		{name: "token_rotation", token: generationSecurityNewToken},
		{name: "delete_and_reregister", deleteFirst: true, token: generationSecurityNewToken},
		{name: "delete_and_reregister_same_token", deleteFirst: true, token: generationSecurityOldToken},
	} {
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			t.Run(mutation.name+"/"+method, func(t *testing.T) {
				p := newGenerationSecurityPeer(t)
				p.assertControl(t)
				wantStatus := http.StatusOK
				if mutation.deleteFirst {
					status, body := p.request(t, http.MethodDelete, "/api/register-node/"+generationSecurityNodeID, generationSecurityRegToken, "")
					if status != http.StatusOK {
						t.Fatalf("unregister status = %d, body=%q", status, body)
					}
					p.assertNotForwarded(t, http.MethodGet, "/while-deleted", generationSecurityHubToken, "")
					p.assertTokenRejected(t, generationSecurityOldToken)
					wantStatus = http.StatusCreated
				}
				p.register(t, mutation.token, wantStatus)

				// A rotated token is rejected on NEW handshakes. Reusing the
				// same token permits a fresh handshake, but must not resurrect
				// a socket from the deleted registration's generation.
				if mutation.token != generationSecurityOldToken {
					p.assertTokenRejected(t, generationSecurityOldToken)
				}

				hubToken := generationSecurityHubToken
				if method == http.MethodOptions {
					hubToken = "" // Public OPTIONS must not revive stale-node authority.
				}
				// Do not connect a replacement peer: that would replace the old
				// socket by ID and conceal the generation-confusion vulnerability.
				p.assertNotForwarded(t, method, "/after-replacement", hubToken, mutation.token)
			})
		}
	}
}
