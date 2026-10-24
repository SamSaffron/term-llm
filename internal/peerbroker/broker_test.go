package peerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestBroker(t *testing.T, cfg Config) (*Handler, *Client, *httptest.Server) {
	t.Helper()
	if cfg.CreateToken == "" {
		cfg.AllowUnauthenticatedCreate = true
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		h.Close()
		srv.Close()
	})
	c, err := NewClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return h, c, srv
}

func mustCreate(t *testing.T, c *Client) *Session {
	t.Helper()
	s, err := c.Create(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func TestMailboxRoundTripAndAck(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)
	if s.HostToken == s.DeviceToken || len(s.HostToken) < 40 || len(s.ID) < 20 {
		t.Fatalf("weak or shared capabilities: %+v", s)
	}
	if strings.Contains(s.String(), s.HostToken) || strings.Contains(s.String(), s.DeviceToken) {
		t.Fatal("Session.String leaks capability")
	}

	seq, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`{"ct":"opaque"}`))
	if err != nil || seq != 1 {
		t.Fatalf("Send = %d, %v", seq, err)
	}
	// Host must not read its own outgoing message.
	if msgs, err := c.Receive(ctx, s.ID, s.HostToken, 0, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("host receive = %v, %v", msgs, err)
	}
	msgs, err := c.Receive(ctx, s.ID, s.DeviceToken, 0, 0)
	if err != nil || len(msgs) != 1 || string(msgs[0].Body) != `{"ct":"opaque"}` {
		t.Fatalf("device receive = %v, %v", msgs, err)
	}
	// Unacked messages are redelivered; acking removes them.
	if msgs, _ = c.Receive(ctx, s.ID, s.DeviceToken, 0, 0); len(msgs) != 1 {
		t.Fatalf("redelivery = %v", msgs)
	}
	if msgs, _ = c.Receive(ctx, s.ID, s.DeviceToken, 1, 0); len(msgs) != 0 {
		t.Fatalf("after ack = %v", msgs)
	}
}

func TestLongPollWakesOnSend(t *testing.T) {
	h, c, _ := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)
	got := make(chan []Message, 1)
	go func() {
		msgs, err := c.Receive(ctx, s.ID, s.HostToken, 0, 10*time.Second)
		if err != nil {
			t.Errorf("Receive: %v", err)
		}
		got <- msgs
	}()
	waitFor(t, hasWaiter(h))
	if _, err := c.Send(ctx, s.ID, s.DeviceToken, json.RawMessage(`"hi"`)); err != nil {
		t.Fatal(err)
	}
	select {
	case msgs := <-got:
		if len(msgs) != 1 || string(msgs[0].Body) != `"hi"` {
			t.Fatalf("got %v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not wake")
	}
}

func hasWaiter(h *Handler) func() bool {
	return func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.waiters > 0
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAuthRequiredEverywhere(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)
	other := mustCreate(t, c)
	bad := []string{"", "nope", other.HostToken, other.DeviceToken, s.HostToken + "x"}
	for _, tok := range bad {
		if _, err := c.Send(ctx, s.ID, tok, json.RawMessage(`1`)); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Send tok=%q err=%v", tok, err)
		}
		if _, err := c.Receive(ctx, s.ID, tok, 0, 0); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Receive err=%v", err)
		}
		if _, err := c.MintTURN(ctx, s.ID, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("MintTURN err=%v", err)
		}
		if err := c.Delete(ctx, s.ID, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Delete err=%v", err)
		}
	}
	// Token in query string is ignored.
	resp, err := http.Get(srv.URL + "/v1/sessions/" + s.ID + "/messages?access_token=" + s.HostToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token status = %d", resp.StatusCode)
	}
}

const testCreateToken = "registration-cap-0123456789abcdefghijklmnop"

func TestCreateToken(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{CreateToken: testCreateToken})
	ctx := context.Background()
	for _, tok := range []string{"", "wrong", testCreateToken + "x"} {
		if _, err := c.Create(ctx, CreateOptions{CreateToken: tok}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if _, err := c.Create(ctx, CreateOptions{CreateToken: testCreateToken}); err != nil {
		t.Fatalf("with token: %v", err)
	}
}

func TestCreateFailsClosedWithoutRegistrationCap(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{CreateToken: "short"},
		{CreateToken: strings.Repeat("a", 40) + " b"},
		{CreateToken: strings.Repeat("a", maxBearerLen+1)},
	} {
		if _, err := NewHandler(cfg); err == nil {
			t.Fatalf("NewHandler(%q) succeeded", cfg.CreateToken)
		}
	}
	h, err := NewHandler(Config{AllowUnauthenticatedCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
}

func TestSessionIDMatchesPairingSID(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{})
	s := mustCreate(t, c)
	raw, err := ParseSessionID(s.ID)
	if err != nil {
		t.Fatalf("ParseSessionID(%q): %v", s.ID, err)
	}
	if FormatSessionID(raw) != s.ID || len(s.ID) != 22 {
		t.Fatalf("round trip %q", s.ID)
	}
	var vec [SessionIDLen]byte
	for i := range vec {
		vec[i] = 4
	}
	if got := FormatSessionID(vec); got != "BAQEBAQEBAQEBAQEBAQEBA" { // contract vector sid
		t.Fatalf("vector sid = %q", got)
	}
	for _, bad := range []string{"", "BAQEBAQEBAQEBAQEBAQEBA==", "BAQEBAQEBAQEBAQEBAQEBB", "BAQEBAQEBAQEBAQEBAQE", "../x"} {
		if _, err := ParseSessionID(bad); err == nil {
			t.Fatalf("ParseSessionID(%q) succeeded", bad)
		}
	}
}

func TestExpiryAndDelete(t *testing.T) {
	clk := newFakeClock()
	h, c, _ := newTestBroker(t, Config{Now: clk.Now})
	ctx := context.Background()
	s, err := c.Create(ctx, CreateOptions{TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ExpiresAt.Sub(clk.Now()); got != 2*time.Minute {
		t.Fatalf("ttl = %s", got)
	}
	clk.Advance(2 * time.Minute)
	if _, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`1`)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired send: %v", err)
	}
	if h.SessionCount() != 0 {
		t.Fatalf("expired session not cleaned up")
	}

	s2 := mustCreate(t, c)
	if err := c.Delete(ctx, s2.ID, s2.DeviceToken); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Receive(ctx, s2.ID, s2.HostToken, 0, 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted receive: %v", err)
	}
}

func TestSweepRemovesIdleExpiredSessions(t *testing.T) {
	clk := newFakeClock()
	h, c, _ := newTestBroker(t, Config{Now: clk.Now, MaxSessions: 2})
	mustCreate(t, c)
	mustCreate(t, c)
	if _, err := c.Create(context.Background(), CreateOptions{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("session limit: %v", err)
	}
	clk.Advance(MaxSessionTTL + time.Second)
	mustCreate(t, c) // sweep frees capacity
	if h.SessionCount() != 1 {
		t.Fatalf("sessions = %d", h.SessionCount())
	}
}

func TestMessageLimits(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{MaxPendingMessages: 2, MaxMessagesPerSession: 3, MaxMessageBytes: 32})
	ctx := context.Background()
	s := mustCreate(t, c)
	big := json.RawMessage(`"` + strings.Repeat("a", 40) + `"`)
	if _, err := c.Send(ctx, s.ID, s.HostToken, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`1`)); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("full: %v", err)
	}
	if _, err := c.Receive(ctx, s.ID, s.DeviceToken, 2, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`1`)); err != nil {
		t.Fatalf("after ack: %v", err)
	}
	if _, err := c.Send(ctx, s.ID, s.DeviceToken, json.RawMessage(`1`)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("session message cap: %v", err)
	}
}

func TestBadBodies(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{})
	s := mustCreate(t, c)
	for _, body := range []string{``, `{}`, `{"body":null}`, `{"body":1}{"x":2}`, `not json`} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions/"+s.ID+"/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.HostToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q status %d", body, resp.StatusCode)
		}
	}
}

func TestConcurrentWaiterLimitAndSupersede(t *testing.T) {
	h, c, _ := newTestBroker(t, Config{MaxWaiters: 1})
	ctx := context.Background()
	s := mustCreate(t, c)
	first := make(chan error, 1)
	go func() {
		msgs, err := c.Receive(ctx, s.ID, s.HostToken, 0, 20*time.Second)
		if err == nil && len(msgs) != 0 {
			err = errors.New("unexpected messages")
		}
		first <- err
	}()
	waitFor(t, hasWaiter(h))
	if _, err := c.Receive(ctx, s.ID, s.DeviceToken, 0, 5*time.Second); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("waiter limit: %v", err)
	}
	// Deleting the session releases the waiter with session_closed.
	if err := c.Delete(ctx, s.ID, s.DeviceToken); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("first waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released")
	}
}

func TestSupersededWaiterReturnsEmpty(t *testing.T) {
	h, c, _ := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)
	first := make(chan []Message, 1)
	go func() {
		msgs, _ := c.Receive(ctx, s.ID, s.HostToken, 0, 20*time.Second)
		first <- msgs
	}()
	waitFor(t, hasWaiter(h))
	second := make(chan []Message, 1)
	go func() {
		msgs, _ := c.Receive(ctx, s.ID, s.HostToken, 0, 20*time.Second)
		second <- msgs
	}()
	select {
	case msgs := <-first:
		if len(msgs) != 0 {
			t.Fatalf("superseded got %v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first waiter not superseded")
	}
	if _, err := c.Send(ctx, s.ID, s.DeviceToken, json.RawMessage(`2`)); err != nil {
		t.Fatal(err)
	}
	if msgs := <-second; len(msgs) != 1 {
		t.Fatalf("second got %v", msgs)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	clk := newFakeClock()
	_, c, _ := newTestBroker(t, Config{Now: clk.Now, RatePerSecond: 1, RateBurst: 2})
	ctx := context.Background()
	mustCreate(t, c)
	mustCreate(t, c)
	if _, err := c.Create(ctx, CreateOptions{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate: %v", err)
	}
	clk.Advance(time.Second)
	mustCreate(t, c)
}

type stubMinter struct {
	calls int
	ttl   time.Duration
	err   error
}

func (m *stubMinter) MintICEServers(_ context.Context, ttl time.Duration) (*ICECredentials, error) {
	m.calls++
	m.ttl = ttl
	if m.err != nil {
		return nil, m.err
	}
	return &ICECredentials{ICEServers: []ICEServer{{URLs: []string{"turns:turn.example:443"}, Username: "u", Credential: "c"}}}, nil
}

func TestTURNEndpoint(t *testing.T) {
	ctx := context.Background()
	t.Run("unavailable", func(t *testing.T) {
		_, c, _ := newTestBroker(t, Config{})
		s := mustCreate(t, c)
		if _, err := c.MintTURN(ctx, s.ID, s.HostToken); !errors.Is(err, ErrTURNUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("quota", func(t *testing.T) {
		m := &stubMinter{}
		_, c, _ := newTestBroker(t, Config{TURNMinter: m, MaxTURNPerSession: 2, TURNTTL: 7 * time.Minute})
		s := mustCreate(t, c)
		for i := 0; i < 2; i++ {
			creds, err := c.MintTURN(ctx, s.ID, s.DeviceToken)
			if err != nil || len(creds.ICEServers) != 1 {
				t.Fatalf("mint: %v %v", creds, err)
			}
		}
		if _, err := c.MintTURN(ctx, s.ID, s.HostToken); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("quota: %v", err)
		}
		if m.calls != 2 || m.ttl != 7*time.Minute {
			t.Fatalf("calls=%d ttl=%s", m.calls, m.ttl)
		}
	})
	t.Run("error not reflected", func(t *testing.T) {
		m := &stubMinter{err: errors.New("upstream said SECRET-TOKEN")}
		_, c, _ := newTestBroker(t, Config{TURNMinter: m})
		s := mustCreate(t, c)
		_, err := c.MintTURN(ctx, s.ID, s.HostToken)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != CodeTURNFailed || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad ttl config", func(t *testing.T) {
		if _, err := NewHandler(Config{TURNTTL: time.Hour}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClientRejectsInsecureBaseURL(t *testing.T) {
	for _, u := range []string{"http://broker.example", "https://user:pw@broker.example", "ftp://x", "https://b.example?x=1"} {
		if _, err := NewClient(u, nil); err == nil {
			t.Errorf("NewClient(%q) succeeded", u)
		}
	}
	for _, u := range []string{"https://broker.example/base", "http://127.0.0.1:9", "http://localhost:1"} {
		if _, err := NewClient(u, nil); err != nil {
			t.Errorf("NewClient(%q): %v", u, err)
		}
	}
}

func TestClientRejectsPathInjection(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{})
	if _, err := c.Send(context.Background(), "../x", "t", json.RawMessage(`1`)); err == nil || !strings.Contains(err.Error(), "invalid session ID") {
		t.Fatalf("got %v", err)
	}
}

func TestClosedBroker(t *testing.T) {
	h, c, _ := newTestBroker(t, Config{})
	s := mustCreate(t, c)
	h.Close()
	_, err := c.Send(context.Background(), s.ID, s.HostToken, json.RawMessage(`1`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != CodeBrokerClosed {
		t.Fatalf("got %v", err)
	}
}
