package peer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

type pairConfig struct {
	handler            http.Handler
	gate               func(context.Context) error
	authorize          func(*http.Request) error
	limits             Limits
	onClose            func()
	observer           DiagnosticObserver
	now                func() time.Time
	testOnBackpressure func()
	// rawInit, when set, opens a raw channel instead of a Client.
	rawLabel string
	rawInit  *webrtc.DataChannelInit
	// endpoint overrides the loopback host-candidate endpoint config.
	endpoint *EndpointConfig
}

type pair struct {
	srvEP  *Endpoint
	cliEP  *Endpoint
	client *Client
	raw    *webrtc.DataChannel
	frames chan *Frame
	closed chan struct{}
}

func loopbackSettings() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetInterfaceFilter(func(name string) bool { return name == "lo" || name == "lo0" })
	return se
}

func allowAll(*http.Request) error         { return nil }
func gateOpen(context.Context) error       { return nil }
func testCtx(t *testing.T) context.Context { return t.Context() }

// newPair connects two real Pion peers over loopback: a serving endpoint
// running cfg.handler and a requesting endpoint.
func newPair(t *testing.T, cfg pairConfig) *pair {
	t.Helper()
	ctx, cancel := context.WithTimeout(testCtx(t), 20*time.Second)
	defer cancel()
	if cfg.gate == nil {
		cfg.gate = gateOpen
	}
	if cfg.authorize == nil {
		cfg.authorize = allowAll
	}
	epCfg := EndpointConfig{SettingEngine: loopbackSettings()}
	if cfg.endpoint != nil {
		epCfg = *cfg.endpoint
	}
	srvEP, err := NewEndpoint(epCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvEP.Close() })
	cliEP, err := NewEndpoint(epCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliEP.Close() })

	srv, err := NewServer(ServerConfig{
		Handler: cfg.handler, SessionGate: cfg.gate, Authorize: cfg.authorize, Limits: cfg.limits,
		OnClose: cfg.onClose, Observer: cfg.observer, Now: cfg.now, Logf: t.Logf, testOnBackpressure: cfg.testOnBackpressure,
	})
	if err != nil {
		t.Fatal(err)
	}
	srvEP.Serve(t.Context(), srv)

	p := &pair{srvEP: srvEP, cliEP: cliEP, frames: make(chan *Frame, 64), closed: make(chan struct{})}
	opened := make(chan struct{})
	if cfg.rawLabel != "" {
		p.raw, err = cliEP.PeerConnection().CreateDataChannel(cfg.rawLabel, cfg.rawInit)
		if err != nil {
			t.Fatal(err)
		}
		p.raw.OnOpen(func() { close(opened) })
		p.raw.OnClose(func() { close(p.closed) })
		p.raw.OnMessage(func(msg webrtc.DataChannelMessage) {
			var f Frame
			if err := json.Unmarshal(msg.Data, &f); err != nil {
				t.Errorf("bad frame from server: %v", err)
				return
			}
			p.frames <- &f
		})
	} else {
		p.client, err = cliEP.NewClient(ClientConfig{Limits: cfg.limits})
		if err != nil {
			t.Fatal(err)
		}
	}

	offer, err := cliEP.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := srvEP.AcceptOffer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := cliEP.AcceptAnswer(answer); err != nil {
		t.Fatal(err)
	}
	if p.client != nil {
		if err := p.client.WaitOpen(ctx); err != nil {
			t.Fatalf("wait open: %v", err)
		}
	} else {
		select {
		case <-opened:
		case <-ctx.Done():
			t.Fatal("raw channel did not open")
		}
	}
	return p
}

func (p *pair) httpClient() *http.Client { return &http.Client{Transport: p.client} }

func (p *pair) sendRaw(t *testing.T, f *Frame) {
	t.Helper()
	f.Version = ProtocolVersion
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.raw.SendText(string(b)); err != nil {
		t.Fatal(err)
	}
}

// collect reads frames for id until response_end.
func (p *pair) collect(t *testing.T, id string) (start *Frame, body []byte, end *Frame) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.ID != id {
				continue
			}
			if f.Version != ProtocolVersion {
				t.Fatalf("frame version %d", f.Version)
			}
			switch f.Type {
			case TypeResponseStart:
				start = f
			case TypeResponseBody:
				body = append(body, f.Body...)
			case TypeResponseEnd:
				return start, body, f
			}
		case <-timeout:
			t.Fatalf("timed out waiting for response %s", id)
		}
	}
}

func TestBridgeStreamsSSEWithSanitisedRequest(t *testing.T) {
	release := make(chan struct{})
	var seen atomic.Pointer[http.Request]
	var seenBody atomic.Value
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody.Store(string(b))
		seen.Store(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	p := newPair(t, pairConfig{handler: handler})

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://ignored.example/api/events?x=1", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer stolen")
	req.Header.Set("Cookie", "session=stolen")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("Proxy-Authorization", "Basic x")
	req.Header.Set("X-Custom", "kept")
	resp, err := p.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d header %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("Connection") != "" {
		t.Fatal("hop-by-hop response header leaked")
	}
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first event before handler finished: %q, %v", line, err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest %q, %v", rest, err)
	}

	r := seen.Load()
	if r.URL.Path != "/api/events" || r.URL.RawQuery != "x=1" || r.Host != "localhost" || r.RemoteAddr != "webrtc-peer" {
		t.Fatalf("request target: path=%q query=%q host=%q remote=%q", r.URL.Path, r.URL.RawQuery, r.Host, r.RemoteAddr)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Proxy-Authorization"} {
		if r.Header.Get(h) != "" {
			t.Errorf("header %s reached handler", h)
		}
	}
	if r.Header.Get("X-Custom") != "kept" || seenBody.Load() != "hello" {
		t.Fatalf("custom header %q body %q", r.Header.Get("X-Custom"), seenBody.Load())
	}
}

func TestBridgeCancellationReachesHandler(t *testing.T) {
	cancelled := make(chan struct{}, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
	p := newPair(t, pairConfig{handler: handler})

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/wait", nil)
		resp, err := p.httpClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		cancel()
		if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancel: %v", err)
		}
		waitSignal(t, cancelled)
	})
	t.Run("body close", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/wait", nil)
		resp, err := p.httpClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		waitSignal(t, cancelled)
	})
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never observed cancellation")
	}
}

func TestBridgeSessionGateAndRouteAuthorization(t *testing.T) {
	var authenticated atomic.Bool
	var calls atomic.Int32
	var authorizedPaths []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "ok")
	})
	p := newPair(t, pairConfig{
		handler: handler,
		gate: func(context.Context) error {
			if !authenticated.Load() {
				return errors.New("no session")
			}
			return nil
		},
		authorize: func(r *http.Request) error {
			authorizedPaths = append(authorizedPaths, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/admin") {
				return errors.New("denied")
			}
			return nil
		},
	})
	get := func(path string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		resp, err := p.httpClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b), resp.Header
	}

	if code, _, header := get("/api/x"); code != http.StatusUnauthorized || header.Get(PeerErrorHeader) != PeerErrorAuthorizationRevoked {
		t.Fatalf("unauthenticated session: status=%d peer-error=%q", code, header.Get(PeerErrorHeader))
	}
	if len(authorizedPaths) != 0 {
		t.Fatal("route authorizer ran before session gate")
	}
	authenticated.Store(true)
	if code, _, header := get("/admin/secret"); code != http.StatusForbidden || header.Get(PeerErrorHeader) != PeerErrorScopeDenied {
		t.Fatalf("denied route: status=%d peer-error=%q", code, header.Get(PeerErrorHeader))
	}
	if code, body, _ := get("/api/x"); code != http.StatusOK || body != "ok" {
		t.Fatalf("allowed route: %d %q", code, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times; only the authorised request may reach it", calls.Load())
	}
}

func TestBridgeRejectsUnsafeRequests(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	p := newPair(t, pairConfig{
		handler:  handler,
		limits:   Limits{MaxFrameBytes: 8 << 10, MaxRequestBodyBytes: 16},
		rawLabel: ChannelLabel,
	})
	cases := []struct {
		name   string
		frame  Frame
		status int
	}{
		{"scheme relative", Frame{Method: "GET", Path: "//evil.example/x"}, 400},
		{"absolute", Frame{Method: "GET", Path: "http://evil.example/x"}, 400},
		{"dot segments", Frame{Method: "GET", Path: "/a/../b"}, 400},
		{"encoded dots", Frame{Method: "GET", Path: "/a/%2E%2E/b"}, 400},
		{"encoded slash", Frame{Method: "GET", Path: "/a%2fb"}, 400},
		{"backslash", Frame{Method: "GET", Path: "/a\\b"}, 400},
		{"empty", Frame{Method: "GET", Path: ""}, 400},
		{"bad header", Frame{Method: "GET", Path: "/x", Header: http.Header{"X-Bad": {"a\r\nInjected: 1"}}}, 400},
		{"method", Frame{Method: "CONNECT", Path: "/x"}, 405},
		{"body limit", Frame{Method: "POST", Path: "/x", Body: bytes.Repeat([]byte("a"), 17)}, 413},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.frame
			f.Type = TypeRequest
			f.ID = "r" + string(rune('a'+i))
			p.sendRaw(t, &f)
			start, _, end := p.collect(t, f.ID)
			if start == nil || start.Status != tc.status || end.Error != "" {
				t.Fatalf("start %+v end %+v", start, end)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("handler reached %d times", calls.Load())
	}

	// A frame over the bridge frame limit (below the transport's message
	// limit) is a protocol violation that closes the channel.
	big := strings.Repeat("a", 16<<10)
	if err := p.raw.SendText(`{"version":1,"type":"request","id":"big","method":"GET","path":"/` + big + `"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("server kept channel open after oversized frame")
	}
}

func TestBridgeConcurrencyLimitAndPanic(t *testing.T) {
	block := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/block":
			w.(http.Flusher).Flush()
			<-block
		case "/panic":
			panic("boom")
		}
	})
	p := newPair(t, pairConfig{handler: handler, limits: Limits{MaxConcurrentStreams: 1}, rawLabel: ChannelLabel})
	p.sendRaw(t, &Frame{Type: TypeRequest, ID: "1", Method: "GET", Path: "/block"})
	waitStart(t, p, "1")
	p.sendRaw(t, &Frame{Type: TypeRequest, ID: "1", Method: "GET", Path: "/panic"})
	select {
	case f := <-p.frames:
		if f.ID != "1" || f.Type != TypeResponseEnd || f.Error != "duplicate request id" {
			t.Fatalf("duplicate id: %+v", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no duplicate-id rejection")
	}
	p.sendRaw(t, &Frame{Type: TypeRequest, ID: "2", Method: "GET", Path: "/panic"})
	if start, _, _ := p.collect(t, "2"); start.Status != http.StatusServiceUnavailable {
		t.Fatalf("over limit: %d", start.Status)
	}
	close(block)
	p.collect(t, "1")
	p.sendRaw(t, &Frame{Type: TypeRequest, ID: "3", Method: "GET", Path: "/panic"})
	if start, _, end := p.collect(t, "3"); start.Status != http.StatusInternalServerError || end.Error == "" {
		t.Fatalf("panic: start %+v end %+v", start, end)
	}
}

func waitStart(t *testing.T, p *pair, id string) {
	t.Helper()
	select {
	case f := <-p.frames:
		if f.ID != id || f.Type != TypeResponseStart {
			t.Fatalf("unexpected frame %+v", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no response_start")
	}
}

func TestBridgeLargeBodyUnderBackpressure(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<19) // 8 MiB
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	limits := Limits{MaxBufferedBytes: 64 << 10}
	p := newPair(t, pairConfig{handler: handler, limits: limits})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/big", nil)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil || n != int64(len(payload)) {
		t.Fatalf("copied %d of %d: %v", n, len(payload), err)
	}
	if want := sha256.Sum256(payload); !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("body corrupted")
	}
}

func TestServerRejectsUnreliableChannel(t *testing.T) {
	ordered := false
	p := newPair(t, pairConfig{
		handler:  http.NotFoundHandler(),
		rawLabel: ChannelLabel,
		rawInit:  &webrtc.DataChannelInit{Ordered: &ordered},
	})
	select {
	case <-p.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("unordered bridge channel was accepted")
	}
}

func TestNewServerFailsClosed(t *testing.T) {
	h := http.NotFoundHandler()
	for _, cfg := range []ServerConfig{
		{SessionGate: gateOpen, Authorize: allowAll},
		{Handler: h, Authorize: allowAll},
		{Handler: h, SessionGate: gateOpen},
	} {
		if _, err := NewServer(cfg); err == nil {
			t.Fatalf("NewServer(%+v) accepted incomplete config", cfg)
		}
	}
}
