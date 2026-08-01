package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
)

type leaseWebSocketRecorder struct {
	server *httptest.Server

	mu             sync.Mutex
	requests       []map[string]any
	connections    int
	active         int
	closed         int
	handshakeDelay time.Duration
	block          <-chan struct{}
	requestSeen    chan struct{}
	seenOnce       sync.Once
}

func newLeaseWebSocketRecorder(t *testing.T, handshakeDelay time.Duration) *leaseWebSocketRecorder {
	t.Helper()
	recorder := &leaseWebSocketRecorder{handshakeDelay: handshakeDelay, requestSeen: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recorder.handshakeDelay > 0 {
			timer := time.NewTimer(recorder.handshakeDelay)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		recorder.mu.Lock()
		recorder.connections++
		recorder.active++
		recorder.mu.Unlock()
		defer func() {
			_ = conn.Close()
			recorder.mu.Lock()
			recorder.active--
			recorder.closed++
			recorder.mu.Unlock()
		}()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var payload map[string]any
			if json.Unmarshal(data, &payload) != nil {
				return
			}
			recorder.mu.Lock()
			recorder.requests = append(recorder.requests, payload)
			block := recorder.block
			recorder.mu.Unlock()
			recorder.seenOnce.Do(func() { close(recorder.requestSeen) })
			if block != nil {
				<-block
			}
			if strings.Contains(string(data), `"fail"`) {
				_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "partial"})
				_ = conn.WriteJSON(map[string]any{
					"type": "response.failed", "status": http.StatusBadGateway,
					"response": map[string]any{"error": map[string]any{"code": "upstream_failed", "message": "failed"}},
				})
				continue
			}
			_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "ok"})
			_ = conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":    "resp_parent",
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			})
		}
	}))
	t.Cleanup(func() { recorder.server.Close() })
	return recorder
}

func (r *leaseWebSocketRecorder) snapshot() (requests []map[string]any, connections, active, closed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	requests = make([]map[string]any, len(r.requests))
	for i, request := range r.requests {
		copyRequest := make(map[string]any, len(request))
		for key, value := range request {
			copyRequest[key] = value
		}
		requests[i] = copyRequest
	}
	return requests, r.connections, r.active, r.closed
}

type leaseWebSocketProvider struct {
	client   *llm.ResponsesClient
	cleanups *atomic.Int32
}

func newLeaseWebSocketProvider(recorder *leaseWebSocketRecorder, cleanups *atomic.Int32) *leaseWebSocketProvider {
	return &leaseWebSocketProvider{
		client: &llm.ResponsesClient{
			BaseURL: recorder.server.URL, HTTPClient: recorder.server.Client(), UseWebSocket: true,
			WebSocketServerState: true, DisableServerState: true,
		},
		cleanups: cleanups,
	}
}

func (*leaseWebSocketProvider) Name() string                   { return "lease-websocket" }
func (*leaseWebSocketProvider) Credential() string             { return "mock" }
func (*leaseWebSocketProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *leaseWebSocketProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return p.client.Stream(ctx, llm.ResponsesRequest{
		Model: req.Model, Messages: req.Messages, Stream: true, SessionID: req.SessionID,
	}, false)
}
func (p *leaseWebSocketProvider) ResetConversation() {
	p.client.ResetConversation()
	if p.cleanups != nil {
		p.cleanups.Add(1)
	}
}

type leaseGatewayFixture struct {
	server      *Server
	httpServer  *httptest.Server
	config      *config.Config
	clients     *ClientStore
	client      Client
	token       string
	factory     ProviderFactory
	factoryRuns atomic.Int32
}

func newLeaseGatewayFixture(t *testing.T, idle time.Duration, providerKeys []string, factory ProviderFactory) *leaseGatewayFixture {
	t.Helper()
	dir := t.TempDir()
	clients, err := OpenClientStore(dir + "/clients.json")
	if err != nil {
		t.Fatal(err)
	}
	client, token, err := clients.Add("satellite-a", Policy{AllowProviders: providerKeys, MaxConcurrentInference: 8})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := OpenStateSealer(dir + "/state.key")
	if err != nil {
		t.Fatal(err)
	}
	providers := make(map[string]config.ProviderConfig, len(providerKeys))
	for _, key := range providerKeys {
		providers[key] = config.ProviderConfig{
			Type: config.ProviderTypeOpenAI, Model: "model-a", Models: []string{"model-a", "model-b"},
			APIKey: "test-key", UseWebSocket: true,
		}
	}
	central := &config.Config{DefaultProvider: providerKeys[0], Providers: providers}
	fixture := &leaseGatewayFixture{config: central, clients: clients, client: client, token: token, factory: factory}
	server, err := NewServer(ServerConfig{
		Config: central, Clients: clients, Sealer: sealer, ProviderSessionIdleTimeout: idle,
		DisableProviderSessionReuse: idle == 0,
		ProviderFactory: func(cfg *config.Config, provider, model string) (llm.Provider, error) {
			fixture.factoryRuns.Add(1)
			return factory(cfg, provider, model)
		},
		Policy: Policy{AllowProviders: providerKeys}, UpstreamRetryAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = server
	for _, key := range providerKeys {
		server.storeCatalogProvider(protocol.CatalogEntry{
			Key: key, Type: string(config.ProviderTypeOpenAI), AllowUnlistedModels: false,
			Models: []protocol.Model{{ID: "model-a"}, {ID: "model-b"}},
		})
	}
	fixture.httpServer = httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		fixture.httpServer.Close()
		server.Close()
	})
	return fixture
}

func (f *leaseGatewayFixture) satellite(t *testing.T, provider, model string) llm.Provider {
	t.Helper()
	providerClient, err := llm.NewGatewayProvider(&config.Config{Gateway: config.GatewayConfig{
		URL: f.httpServer.URL, Token: f.token, ConnectTimeout: "2s", ResponseTimeout: "2s", ToolTimeout: "2s",
	}}, provider, model)
	if err != nil {
		t.Fatal(err)
	}
	return providerClient
}

func runLeaseTurn(t *testing.T, provider llm.Provider, ctx context.Context, req llm.Request) error {
	t.Helper()
	stream, err := provider.Stream(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		_, err = stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func leaseTurns(sessionID string) (llm.Request, llm.Request) {
	first := llm.Request{Model: "model-a", SessionID: sessionID, Messages: []llm.Message{llm.UserText("first")}}
	second := llm.Request{Model: "model-a", SessionID: sessionID, Messages: []llm.Message{
		llm.UserText("first"), llm.AssistantText("answer"), llm.UserText("second"),
	}}
	return first, second
}

type leaseCleanupProvider struct {
	resets   atomic.Int32
	cleanups atomic.Int32
}

func (*leaseCleanupProvider) Name() string                   { return "lease-cleanup" }
func (*leaseCleanupProvider) Credential() string             { return "mock" }
func (*leaseCleanupProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (*leaseCleanupProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused")
}
func (p *leaseCleanupProvider) ResetConversation() { p.resets.Add(1) }
func (p *leaseCleanupProvider) CleanupMCP()        { p.cleanups.Add(1) }

func TestGatewayProviderSessionCleanupUsesRetryForwarders(t *testing.T) {
	provider := &leaseCleanupProvider{}
	cleanupProviderSession(llm.WrapWithRetry(provider, llm.RetryConfig{MaxAttempts: 1}))
	if provider.resets.Load() != 1 || provider.cleanups.Load() != 1 {
		t.Fatalf("provider reset/cleanup calls = %d/%d, want 1/1", provider.resets.Load(), provider.cleanups.Load())
	}
}

func TestGatewayProviderSessionZeroDisablesReuse(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	fixture := newLeaseGatewayFixture(t, 0, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("disabled-session")
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	_, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 2 || connections != 2 {
		t.Fatalf("zero timeout providers/connections = %d/%d, want 2/2", fixture.factoryRuns.Load(), connections)
	}
}

func TestGatewayProviderSessionNegativeTimeoutRejected(t *testing.T) {
	dir := t.TempDir()
	clients, err := OpenClientStore(dir + "/clients.json")
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := OpenStateSealer(dir + "/state.key")
	if err != nil {
		t.Fatal(err)
	}
	defaulted, err := NewServer(ServerConfig{
		Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: clients, Sealer: sealer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.providerSessions == nil || defaulted.providerSessions.idleTimeout != DefaultProviderSessionIdleTimeout {
		t.Fatalf("ServerConfig zero idle timeout = %#v, want default %s", defaulted.providerSessions, DefaultProviderSessionIdleTimeout)
	}
	defaulted.Close()

	_, err = NewServer(ServerConfig{
		Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: clients, Sealer: sealer,
		ProviderSessionIdleTimeout: -time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("negative provider session timeout error = %v", err)
	}
}

func TestGatewayProviderSessionWarmReuseAndIdleExpiry(t *testing.T) {
	const handshakeDelay = 25 * time.Millisecond
	recorder := newLeaseWebSocketRecorder(t, handshakeDelay)
	var cleanups atomic.Int32
	fixture := newLeaseGatewayFixture(t, 60*time.Millisecond, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, &cleanups), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("session-warm")

	firstStarted := time.Now()
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	firstElapsed := time.Since(firstStarted)
	warmStarted := time.Now()
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	warmElapsed := time.Since(warmStarted)

	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 1 || connections != 1 || len(requests) != 2 {
		t.Fatalf("warm provider/connections/requests = %d/%d/%d, want 1/1/2", fixture.factoryRuns.Load(), connections, len(requests))
	}
	if requests[1]["previous_response_id"] != "resp_parent" {
		t.Fatalf("warm previous_response_id = %#v, want resp_parent (providers=%d connections=%d requests=%#v)", requests[1]["previous_response_id"], fixture.factoryRuns.Load(), connections, requests)
	}
	if input, ok := requests[1]["input"].([]any); !ok || len(input) != 1 || !strings.Contains(fmt.Sprint(input[0]), "second") {
		t.Fatalf("warm continuation input = %#v, want only latest suffix", requests[1]["input"])
	}

	time.Sleep(90 * time.Millisecond)
	coldStarted := time.Now()
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	coldElapsed := time.Since(coldStarted)
	requests, connections, _, _ = recorder.snapshot()
	if fixture.factoryRuns.Load() != 2 || connections != 2 || len(requests) != 3 {
		t.Fatalf("expired provider/connections/requests = %d/%d/%d, want 2/2/3", fixture.factoryRuns.Load(), connections, len(requests))
	}
	if _, ok := requests[2]["previous_response_id"]; ok {
		t.Fatalf("expired request retained previous_response_id: %#v", requests[2])
	}
	if input, ok := requests[2]["input"].([]any); !ok || len(input) != 3 {
		t.Fatalf("expired request input = %#v, want full transcript", requests[2]["input"])
	}
	if cleanups.Load() < 1 {
		t.Fatal("idle expiry did not reset/close the retained provider")
	}
	t.Logf("fake live WebSocket timings: first=%s warm-follow-up=%s expired-cold=%s; handshakes warm=1, after expiry=2", firstElapsed, warmElapsed, coldElapsed)
}

func TestGatewayProviderSessionKeyIsolation(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"alpha", "beta"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	otherClient, otherToken, err := fixture.clients.Add("satellite-b", Policy{AllowProviders: []string{"alpha", "beta"}, MaxConcurrentInference: 8})
	if err != nil {
		t.Fatal(err)
	}
	_ = otherClient
	otherProvider, err := llm.NewGatewayProvider(&config.Config{Gateway: config.GatewayConfig{
		URL: fixture.httpServer.URL, Token: otherToken, ConnectTimeout: "2s", ResponseTimeout: "2s",
	}}, "alpha", "model-a")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		provider llm.Provider
		session  string
	}{
		{fixture.satellite(t, "alpha", "model-a"), "same-session"},
		{fixture.satellite(t, "alpha", "model-a"), "other-session"},
		{fixture.satellite(t, "beta", "model-a"), "same-session"},
		{otherProvider, "same-session"},
	}
	for _, tc := range cases {
		first, second := leaseTurns(tc.session)
		if err := runLeaseTurn(t, tc.provider, t.Context(), first); err != nil {
			t.Fatal(err)
		}
		if err := runLeaseTurn(t, tc.provider, t.Context(), second); err != nil {
			t.Fatal(err)
		}
	}
	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != int32(len(cases)) || connections != len(cases) || len(requests) != 2*len(cases) {
		t.Fatalf("isolated provider/connections/requests = %d/%d/%d, want %d/%d/%d", fixture.factoryRuns.Load(), connections, len(requests), len(cases), len(cases), 2*len(cases))
	}
	for i := 1; i < len(requests); i += 2 {
		if requests[i]["previous_response_id"] != "resp_parent" {
			t.Fatalf("isolated continuation %d lost its own parent: %#v", i, requests[i])
		}
	}
}

func TestGatewayProviderSessionsDifferentKeysRemainConcurrent(t *testing.T) {
	release := make(chan struct{})
	recorder := newLeaseWebSocketRecorder(t, 0)
	recorder.block = release
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	errs := make(chan error, 2)
	for _, sessionID := range []string{"session-a", "session-b"} {
		sessionID := sessionID
		go func() {
			first, _ := leaseTurns(sessionID)
			errs <- runLeaseTurn(t, provider, context.Background(), first)
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		requests, connections, _, _ := recorder.snapshot()
		if len(requests) == 2 && connections == 2 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("different keys did not reach upstream concurrently: requests=%d connections=%d", len(requests), connections)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayProviderSessionSameKeySerializes(t *testing.T) {
	release := make(chan struct{})
	recorder := newLeaseWebSocketRecorder(t, 0)
	recorder.block = release
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("serialized")
	errs := make(chan error, 2)
	go func() { errs <- runLeaseTurn(t, provider, context.Background(), first) }()
	select {
	case <-recorder.requestSeen:
	case <-time.After(time.Second):
		t.Fatal("first upstream request did not start")
	}
	go func() { errs <- runLeaseTurn(t, provider, context.Background(), second) }()

	timer := time.NewTimer(40 * time.Millisecond)
	<-timer.C
	requests, connections, _, _ := recorder.snapshot()
	if len(requests) != 1 || connections != 1 {
		t.Fatalf("same-key concurrent turn reached upstream early: requests=%d connections=%d", len(requests), connections)
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	requests, connections, _, _ = recorder.snapshot()
	if len(requests) != 2 || connections != 1 || fixture.factoryRuns.Load() != 1 {
		t.Fatalf("serialized final requests/connections/providers = %d/%d/%d", len(requests), connections, fixture.factoryRuns.Load())
	}
}

func TestGatewayProviderSessionWarmStateImportIsRejectedAndEvicted(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	var cleanups atomic.Int32
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, &cleanups), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("invalid-state-session")
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	importer, ok := provider.(llm.ProviderStateImporter)
	if !ok {
		t.Fatal("GatewayProvider does not expose sealed state import")
	}
	if err := importer.ImportProviderState([]byte("stale-sealed-state")); err != nil {
		t.Fatal(err)
	}
	if err := runLeaseTurn(t, provider, t.Context(), second); err == nil || !strings.Contains(err.Error(), "must not be re-imported") {
		t.Fatalf("warm stale-state error = %v", err)
	}
	fresh := fixture.satellite(t, "remote", "model-a")
	if err := runLeaseTurn(t, fresh, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 2 || connections != 2 || len(requests) != 2 {
		t.Fatalf("invalid-state providers/connections/requests = %d/%d/%d, want 2/2/2", fixture.factoryRuns.Load(), connections, len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("post-invalid-state request reused response state: %#v", requests[1])
	}
	if cleanups.Load() < 1 {
		t.Fatal("invalid warm state did not clean the retained provider")
	}
}

func TestGatewayProviderSessionFailureEvicts(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	var cleanups atomic.Int32
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, &cleanups), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	failed := llm.Request{Model: "model-a", SessionID: "failed-session", Messages: []llm.Message{llm.UserText("fail")}}
	if err := runLeaseTurn(t, provider, t.Context(), failed); err == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	_, second := leaseTurns("failed-session")
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 2 || connections != 2 || len(requests) != 2 {
		t.Fatalf("post-failure providers/connections/requests = %d/%d/%d, want 2/2/2", fixture.factoryRuns.Load(), connections, len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("post-failure request reused suspect response state: %#v", requests[1])
	}
	if cleanups.Load() < 1 {
		t.Fatal("failed provider was not reset/closed")
	}
}

type cancelLeaseProvider struct {
	start    chan struct{}
	blocking bool
	resets   *atomic.Int32
}

func (*cancelLeaseProvider) Name() string                   { return "cancel-lease" }
func (*cancelLeaseProvider) Credential() string             { return "mock" }
func (*cancelLeaseProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *cancelLeaseProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	if p.start != nil {
		close(p.start)
	}
	if p.blocking {
		return &blockingStream{ctx: ctx, canceled: make(chan struct{})}, nil
	}
	return &oneEventStream{ctx: ctx, event: llm.Event{Type: llm.EventTextDelta, Text: "ok"}}, nil
}
func (p *cancelLeaseProvider) ResetConversation() { p.resets.Add(1) }

func TestGatewayProviderSessionCancellationEvicts(t *testing.T) {
	started := make(chan struct{})
	var instances atomic.Int32
	var resets atomic.Int32
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		instance := instances.Add(1)
		return &cancelLeaseProvider{start: map[bool]chan struct{}{true: started}[instance == 1], blocking: instance == 1, resets: &resets}, nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runLeaseTurn(t, provider, ctx, llm.Request{Model: "model-a", SessionID: "cancel-session", Messages: []llm.Message{llm.UserText("wait")}})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	if err := <-done; err == nil || !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled turn error = %v", err)
	}
	_, second := leaseTurns("cancel-session")
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if instances.Load() != 2 || resets.Load() < 1 {
		t.Fatalf("post-cancel instances/resets = %d/%d, want 2/>=1", instances.Load(), resets.Load())
	}
}

func TestGatewayProviderSessionShutdownClosesWebSockets(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	var cleanups atomic.Int32
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, &cleanups), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, _ := leaseTurns("shutdown-session")
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	fixture.server.Close()
	deadline := time.Now().Add(time.Second)
	for {
		_, _, active, closed := recorder.snapshot()
		if active == 0 && closed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shutdown connection lifecycle active/closed = %d/%d", active, closed)
		}
		time.Sleep(time.Millisecond)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("shutdown provider resets = %d, want 1", cleanups.Load())
	}
}

func TestGatewayProviderSessionDirectAndGatewayPayloadEquivalence(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	first, second := leaseTurns("equivalent-session")

	direct := newLeaseWebSocketProvider(recorder, nil)
	if err := runLeaseTurn(t, direct, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := runLeaseTurn(t, direct, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	direct.ResetConversation()

	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	gatewayProvider := fixture.satellite(t, "remote", "model-a")
	if err := runLeaseTurn(t, gatewayProvider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := runLeaseTurn(t, gatewayProvider, t.Context(), second); err != nil {
		t.Fatal(err)
	}

	requests, _, _, _ := recorder.snapshot()
	if len(requests) != 4 {
		t.Fatalf("payload count = %d, want 4", len(requests))
	}
	for _, index := range []int{0, 1} {
		if !reflect.DeepEqual(requests[index], requests[index+2]) {
			t.Fatalf("direct/gateway payload %d differs\ndirect: %#v\ngateway: %#v", index, requests[index], requests[index+2])
		}
	}

	fixture.server.Close()
	restarted := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	if err := runLeaseTurn(t, restarted.satellite(t, "remote", "model-a"), t.Context(), second); err != nil {
		t.Fatal(err)
	}
	requests, _, _, _ = recorder.snapshot()
	cold := requests[len(requests)-1]
	if _, ok := cold["previous_response_id"]; ok {
		t.Fatalf("cold restart retained previous_response_id: %#v", cold)
	}
	if input, ok := cold["input"].([]any); !ok || len(input) != 3 {
		t.Fatalf("cold restart input = %#v, want full-history fallback", cold["input"])
	}
}

func TestGatewayProviderSessionModelSwapKeepsLeaseButStartsSafeChain(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("model-swap")
	second.Model = "model-b"
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 1 || connections != 1 || len(requests) != 2 {
		t.Fatalf("model swap providers/connections/requests = %d/%d/%d, want 1/1/2", fixture.factoryRuns.Load(), connections, len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("incompatible model swap reused previous_response_id: %#v", requests[1])
	}
	if input, ok := requests[1]["input"].([]any); !ok || len(input) != 3 {
		t.Fatalf("model swap input = %#v, want direct-provider full-history chain", requests[1]["input"])
	}
}

func TestGatewayResponsesEdgeNeverUsesSatelliteProviderSessions(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	for _, prompt := range []string{"first", "second"} {
		body := strings.NewReader(fmt.Sprintf(`{"model":"remote/model-a","input":%q}`, prompt))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.httpServer.URL+"/v1/responses", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+fixture.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Responses edge status/body = %d/%s", resp.StatusCode, data)
		}
	}
	requests, connections, _, _ := recorder.snapshot()
	if fixture.factoryRuns.Load() != 2 || connections != 2 || len(requests) != 2 {
		t.Fatalf("stateless Responses providers/connections/requests = %d/%d/%d, want 2/2/2", fixture.factoryRuns.Load(), connections, len(requests))
	}
	for _, request := range requests {
		if _, ok := request["previous_response_id"]; ok {
			t.Fatalf("stateless Responses request acquired gateway continuation: %#v", request)
		}
	}
}

func TestGatewayProviderSessionEligibilityAndConfigFingerprint(t *testing.T) {
	recorder := newLeaseWebSocketRecorder(t, 0)
	fixture := newLeaseGatewayFixture(t, time.Second, []string{"remote"}, func(*config.Config, string, string) (llm.Provider, error) {
		return newLeaseWebSocketProvider(recorder, nil), nil
	})
	provider := fixture.satellite(t, "remote", "model-a")
	first, second := leaseTurns("config-session")
	if err := runLeaseTurn(t, provider, t.Context(), first); err != nil {
		t.Fatal(err)
	}

	fixture.server.configMu.Lock()
	updated := *fixture.server.config
	updated.Providers = make(map[string]config.ProviderConfig, len(fixture.server.config.Providers))
	for key, value := range fixture.server.config.Providers {
		updated.Providers[key] = value
	}
	providerConfig := updated.Providers["remote"]
	providerConfig.APIKey = "rotated-key"
	updated.Providers["remote"] = providerConfig
	fixture.server.config = &updated
	fixture.server.configGeneration++
	fixture.server.configMu.Unlock()

	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	requests, connections, _, _ := recorder.snapshot()
	if connections != 2 || fixture.factoryRuns.Load() != 2 {
		t.Fatalf("credential/config change connections/providers = %d/%d, want 2/2", connections, fixture.factoryRuns.Load())
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatal("config-changed request reused stale continuation state")
	}

	providerConfig.UseWebSocket = false
	updated.Providers["remote"] = providerConfig
	fixture.server.configMu.Lock()
	fixture.server.config = &updated
	fixture.server.configGeneration++
	fixture.server.configMu.Unlock()
	second.Ephemeral = true
	if err := runLeaseTurn(t, provider, t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if fixture.factoryRuns.Load() != 3 {
		t.Fatalf("non-WebSocket/ephemeral request factory runs = %d, want 3", fixture.factoryRuns.Load())
	}
}
