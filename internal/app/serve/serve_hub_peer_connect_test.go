package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
	"github.com/samsaffron/term-llm/internal/peerconnect"
)

// This is real broker WS + authenticated SDP + Pion + Hub proxy + serve API +
// agent engine execution. The model is explicitly deterministic mockprovider;
// it is not proof of a remote provider, Docker, native SDK or Cloudflare TURN.
func TestHubPeerConnectRunsRealChatAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	registration := strings.Repeat("r", 43)
	broker, err := peerbroker.NewHandler(peerbroker.Config{CreateToken: registration})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(broker)
	defer ts.Close()
	defer broker.Close()
	node := newTestServeServer("peer-real-engine-response")
	defer node.sessionMgr.Close()
	node.cfg.requireAuth = true
	node.cfg.token = "node-private-token"
	backend := httptest.NewServer(node.httpHandler())
	defer backend.Close()
	hubServer := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "agent", Name: "Agent", URL: backend.URL, Token: node.cfg.token}}}), nil)
	hubServer.token = "never-send-hub-token"
	hubServer.requireAuth = true
	linkCh := make(chan pairing.Link, 1)
	approvals := 0
	host, err := peerconnect.NewHost(ctx, peerconnect.Config{
		BrokerURL: ts.URL, RegistrationToken: registration, StateDir: filepath.Join(t.TempDir(), "peer"),
		Handler: hubServer.peerHandler(func(r *http.Request) bool {
			grant, ok := peerconnect.AuthenticatedGrant(r.Context())
			return ok && grant.Scope == pairing.ScopeHubChat
		}, func(id string) bool { return id == "agent" }),
		Approver: pairing.ApproverFunc(func(_ context.Context, p pairing.PairingPrompt) (bool, error) {
			approvals++
			return p.DeviceName == "Integration device", nil
		}),
		OnPairingLink: func(link pairing.Link) error { linkCh <- link; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("host did not stop")
		}
	}()
	var link pairing.Link
	select {
	case link = <-linkCh:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if strings.Contains(strings.Split(link.String(), "#")[0], "ps=") {
		t.Fatal("pair PSK in query")
	}
	device, err := pairing.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signalling, err := peerbroker.NewClient(ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := signalling.Dial(ctx, link.BrokerSessionID(), link.Capability)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	endpoint, err := peer.NewEndpoint(peer.EndpointConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	client, err := endpoint.NewClient(peer.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := endpoint.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := pairing.StartPairing(link, device, "Integration device", offer.SDP, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sendPeerSignal(t, socket, attempt.Request())
	var answer pairing.PairAnswer
	receivePeerSignal(t, ctx, socket, &answer)
	verified, err := attempt.VerifyAnswer(&answer)
	if err != nil {
		t.Fatal(err)
	}
	if err = endpoint.AcceptAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: verified.AnswerSDP}); err != nil {
		t.Fatal(err)
	}
	if err = client.WaitOpen(ctx); err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: client, CheckRedirect: hubDoNotFollowRedirects}
	// Real Hub metadata route, not a static fixture.
	response := peerRequest(t, ctx, httpClient, "GET", "/healthz", "")
	health, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(health), `"role":"hub"`) {
		t.Fatalf("Hub health: %d %s %v", response.StatusCode, health, err)
	}
	// The real serve handler invokes the engine and streams generated run events.
	response = peerRequest(t, ctx, httpClient, "POST", "/node/agent/v1/responses", `{"input":"unique peer integration prompt","stream":true}`)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	var text strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil {
				text.WriteString(event.Delta)
			}
		}
	}
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "response.completed") || text.String() != "peer-real-engine-response" {
		t.Fatalf("chat: %d %s %v", response.StatusCode, body, err)
	}
	if strings.Contains(string(body), hubServer.token) || strings.Contains(string(body), node.cfg.token) {
		t.Fatal("server credentials leaked")
	}
	// Unsupported administrator scope is a route denial, not revoked identity.
	response = peerRequest(t, ctx, httpClient, "GET", "/auth/security", "")
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 403 || response.Header.Get("X-Term-LLM-Peer-Error") != "scope_denied" {
		t.Fatalf("scope denial %d %v", response.StatusCode, response.Header)
	}
	response = peerRequest(t, ctx, httpClient, "GET", "/healthz", "")
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("scope denial destroyed paired identity")
	}
	// Real fresh-challenge reconnect on the same live broker rendezvous.
	endpoint2, err := peer.NewEndpoint(peer.EndpointConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint2.Close()
	client2, err := endpoint2.NewClient(peer.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	offer2, err := endpoint2.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sendPeerSignal(t, socket, pairing.NewReconnectHello(device, verified.GrantID))
	var challenge pairing.ReconnectChallenge
	receivePeerSignal(t, ctx, socket, &challenge)
	retry, err := pairing.StartReconnect(&challenge, verified.HostKey, device, verified.GrantID, offer2.SDP, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sendPeerSignal(t, socket, retry.Request())
	var reconnect pairing.ReconnectAnswer
	receivePeerSignal(t, ctx, socket, &reconnect)
	accepted, err := retry.VerifyAnswer(&reconnect)
	if err != nil {
		t.Fatal(err)
	}
	if err = endpoint2.AcceptAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: accepted}); err != nil {
		t.Fatal(err)
	}
	if err = client2.WaitOpen(ctx); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("approval count %d", approvals)
	}
	// Revocation is persisted and consulted for every new HTTP request.
	if err = host.Grants().Revoke(pairing.B64(verified.GrantID).String(), time.Now()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://paired.invalid/healthz", nil)
	response, err = (&http.Client{Transport: client2}).Do(req)
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatalf("revoked grant accepted: %d", response.StatusCode)
		}
	}
}

func sendPeerSignal(t *testing.T, c *peerbroker.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Send(b); err != nil {
		t.Fatal(err)
	}
}
func receivePeerSignal(t *testing.T, ctx context.Context, c *peerbroker.Conn, out any) {
	t.Helper()
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetReadDeadline(d)
	}
	for {
		f, err := c.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == peerbroker.FrameSignal {
			if err = json.Unmarshal(f.Body, out); err != nil {
				t.Fatal(err)
			}
			return
		}
		if f.Type == peerbroker.FrameError {
			t.Fatalf("broker error: %s", f.Error)
		}
	}
}
func peerRequest(t *testing.T, ctx context.Context, c *http.Client, method, path, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, method, "http://paired.invalid"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
