package livecall

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSDP = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"

// TestLiveProcess is a real subprocess using newline-delimited app-server RPC.
// Assertions here fail the handshake rather than relying on a live Codex binary.
func TestLiveProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "live-helper" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	fail := func() { os.Exit(2) }
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_HOME") != os.Getenv("HOME") {
		fail()
	}
	send := func(v any) {
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			fail()
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	step := 0
	methods := []string{"initialize", "initialized", "account/login/start", "thread/start", "thread/realtime/start"}
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result Tokens          `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			fail()
		}
		if req.Method == "" {
			if mode != "refresh" || req.Result.AccessToken != "refreshed" || req.Result.AccountID != "account" {
				fail()
			}
			send(map[string]any{"method": "thread/realtime/sdp", "params": map[string]string{"threadId": "thread", "sdp": testSDP}})
			continue
		}
		if step == len(methods) && req.Method == "test/closed" {
			send(map[string]any{"method": "thread/realtime/closed", "params": map[string]string{"threadId": "thread"}})
			continue
		}
		if step >= len(methods) || req.Method != methods[step] {
			fail()
		}
		step++
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			if !reflect.DeepEqual(req.Params["capabilities"], map[string]any{"experimentalApi": true}) {
				fail()
			}
		case "initialized":
			continue
		case "account/login/start":
			if !reflect.DeepEqual(req.Params, map[string]any{"type": "chatgptAuthTokens", "accessToken": "secret-test-token", "chatgptAccountId": "account"}) {
				fail()
			}
		case "thread/start":
			if req.Params["ephemeral"] != true || req.Params["sandbox"] != "read-only" || req.Params["approvalPolicy"] != "never" || req.Params["cwd"] != os.Getenv("HOME") {
				fail()
			}
			result = map[string]any{"thread": map[string]string{"id": "thread"}}
		case "thread/realtime/start":
			expected := map[string]any{"threadId": "thread", "transport": map[string]any{"type": "webrtc", "sdp": testSDP}, "version": "v3", "outputModality": "audio", "clientManagedHandoffs": true, "includeStartupContext": false, "model": "gpt-live-1-codex", "prompt": "app instructions", "initialItems": []any{map[string]any{"role": "user", "text": "hello"}}}
			if !reflect.DeepEqual(req.Params, expected) {
				fail()
			}
			if mode == "error" {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32602, "message": "secret-test-token"}})
				continue
			}
			if mode == "exit" {
				return
			}
			if mode == "malformed" {
				fmt.Fprintln(os.Stdout, "not JSON secret-test-token")
				continue
			}
			if mode == "refresh" {
				send(map[string]any{"id": "refresh-request", "method": "account/chatgptAuthTokens/refresh", "params": map[string]string{"reason": "unauthorized", "previousAccountId": "account"}})
			} else {
				// SDP is allowed to precede the empty RPC acknowledgement.
				thread := "thread"
				if mode == "wrong-thread" {
					thread = "another-thread"
				}
				send(map[string]any{"method": "thread/realtime/sdp", "params": map[string]string{"threadId": thread, "sdp": testSDP}})
			}
		}
		send(map[string]any{"id": req.ID, "result": result})
	}
}
func testManager(t *testing.T, mode string) *Manager {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Path: path, Tokens: func(context.Context, bool) (Tokens, error) {
		return Tokens{AccessToken: "secret-test-token", AccountID: "account"}, nil
	}, command: func(ctx context.Context, path string, args ...string) *exec.Cmd {
		expected := []string{"app-server", "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `history.persistence="none"`, "-c", "analytics.enabled=false"}
		if !reflect.DeepEqual(args, expected) {
			t.Errorf("unexpected command args: %v", args)
		}
		return exec.CommandContext(ctx, path, "-test.run=^TestLiveProcess$", "--", "live-helper", mode)
	}}
	t.Cleanup(m.Close)
	return m
}
func testOffer() Offer {
	return Offer{SDP: testSDP, Instructions: "app instructions", InitialItems: []Item{{Role: "user", Text: "hello"}}}
}
func onlyProcess(t *testing.T, m *Manager) *process {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 1 {
		t.Fatalf("sessions=%d", len(m.sessions))
	}
	for p := range m.sessions {
		return p
	}
	return nil
}
func waitDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop")
	}
}
func TestLiveLifecycle(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	m := testManager(t, "ok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer, err := m.Start(ctx, testOffer())
	if err != nil {
		t.Fatal(err)
	}
	p := onlyProcess(t, m)
	if answer.Protocol != "frameless-bidi-v3" || answer.DelegationCompletion != "context_append" ||
		answer.SDP != testSDP || time.Until(answer.ExpiresAt) > TTL {
		t.Fatalf("bad answer: %+v", answer)
	}
	cancel() // HTTP request completion must not own the sideband lifetime.
	select {
	case <-p.ctx.Done():
		t.Fatal("sideband canceled with completed request")
	default:
	}
	if err := p.send(map[string]any{"method": "test/closed"}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, p.done)
	m.Close()
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatalf("temporary home remains: %v", err)
	}
	if _, err := m.Start(context.Background(), testOffer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("start after shutdown: %v", err)
	}
}
func TestLiveTTL(t *testing.T) {
	m := testManager(t, "ok")
	m.ttl = 500 * time.Millisecond
	_, err := m.Start(context.Background(), testOffer())
	if err != nil {
		t.Fatal(err)
	}
	p := onlyProcess(t, m)
	waitDone(t, p.done)
	if !errors.Is(p.ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(p.ctx.Err())
	}
}
func TestLiveSetupFailures(t *testing.T) {
	for _, mode := range []string{"error", "exit", "malformed", "wrong-thread"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t, mode)
			var mu sync.Mutex
			var logs []string
			m.Log = func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
			_, err := m.Start(context.Background(), testOffer())
			if err == nil {
				t.Fatal("expected failure")
			}
			m.Close()
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(strings.Join(logs, " ")+err.Error(), "secret-test-token") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestLiveRefresh(t *testing.T) {
	m := testManager(t, "refresh")
	m.Tokens = func(_ context.Context, force bool) (Tokens, error) {
		token := "secret-test-token"
		if force {
			token = "refreshed"
		}
		return Tokens{AccessToken: token, AccountID: "account"}, nil
	}
	if _, err := m.Start(context.Background(), testOffer()); err != nil {
		t.Fatal(err)
	}
}
func TestLiveCapacityAndCancellation(t *testing.T) {
	m := testManager(t, "ok")
	entered := make(chan struct{}, MaxSessions)
	m.Tokens = func(ctx context.Context, _ bool) (Tokens, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return Tokens{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, MaxSessions)
	for range MaxSessions {
		go func() { _, err := m.Start(ctx, testOffer()); results <- err }()
	}
	for range MaxSessions {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("setup not reached")
		}
	}
	if _, err := m.Start(ctx, testOffer()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	cancel()
	for range MaxSessions {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("expected cancellation")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancel did not finish")
		}
	}
	m.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 {
		t.Fatal("sessions leaked")
	}
}
func TestLiveUnavailable(t *testing.T) {
	for _, path := range []string{"", "/no/such/codex"} {
		m := &Manager{Path: path, Tokens: func(context.Context, bool) (Tokens, error) {
			t.Fatal("tokens requested before executable available")
			return Tokens{}, nil
		}}
		if _, err := m.Start(context.Background(), testOffer()); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		m.Close()
	}
}
func TestLiveOfferValidation(t *testing.T) {
	cases := []Offer{{}, {SDP: testSDP, Instructions: strings.Repeat("x", 16385)}, {SDP: testSDP, InitialItems: []Item{{Role: "system", Text: "hello"}}}, {SDP: testSDP, InitialItems: []Item{{Role: "user", Text: "\x00"}}}, {SDP: testSDP, InitialItems: []Item{{Role: "user", Text: strings.Repeat("x", 8193)}}}, {SDP: testSDP, InitialItems: make([]Item, 129)}}
	for _, o := range cases {
		if o.Validate() == nil {
			t.Fatal("accepted invalid offer")
		}
	}
	if err := testOffer().Validate(); err != nil {
		t.Fatal(err)
	}
}
