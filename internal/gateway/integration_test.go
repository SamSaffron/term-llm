package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/search"
)

type recordingUsage struct {
	mu      sync.Mutex
	records []UsageRecord
}

func (r *recordingUsage) Record(record UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

type echoTool struct{}

func (echoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "echo", Description: "echo", Schema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}
}
func (echoTool) Preview(json.RawMessage) string { return "echo" }
func (echoTool) Execute(_ context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &payload)
	return llm.TextOutput("satellite:" + payload.Text), nil
}

type inlineProvider struct {
	mu       sync.Mutex
	response llm.ToolExecutionResponse
}

func (*inlineProvider) Name() string       { return "inline" }
func (*inlineProvider) Credential() string { return "mock" }
func (*inlineProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true, InlineToolLoop: true, ManagesOwnContext: true, OrderedInlineToolEvents: true}
}
func (p *inlineProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &inlineProviderStream{ctx: ctx, provider: p}, nil
}

type inlineProviderStream struct {
	ctx      context.Context
	provider *inlineProvider
	step     int
	response chan llm.ToolExecutionResponse
}

func (s *inlineProviderStream) Recv() (llm.Event, error) {
	s.step++
	switch s.step {
	case 1:
		s.response = make(chan llm.ToolExecutionResponse, 1)
		return llm.Event{Type: llm.EventTextDelta, Text: "before "}, nil
	case 2:
		return llm.Event{Type: llm.EventToolCall, ToolCallID: "inline-call", ToolName: "echo", Tool: &llm.ToolCall{ID: "inline-call", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}, ToolResponse: s.response}, nil
	case 3:
		select {
		case response := <-s.response:
			s.provider.mu.Lock()
			s.provider.response = response
			s.provider.mu.Unlock()
			if response.Err != nil {
				return llm.Event{Type: llm.EventTextDelta, Text: "tool-error"}, nil
			}
			return llm.Event{Type: llm.EventTextDelta, Text: "after " + response.Result.Content}, nil
		case <-s.ctx.Done():
			return llm.Event{}, s.ctx.Err()
		}
	case 4:
		return llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 10, OutputTokens: 4}}, nil
	default:
		return llm.Event{}, io.EOF
	}
}
func (*inlineProviderStream) Close() error { return nil }

type fakeSearcher struct{}

func (fakeSearcher) Search(context.Context, string, int) ([]search.Result, error) {
	return []search.Result{{Title: "Gateway result", URL: "https://example.com", Snippet: "central"}}, nil
}

type fakeFetcher struct{}

func (fakeFetcher) FetchURL(context.Context, string) (string, error) { return "central fetch", nil }

type setupFailureProvider struct {
	mu       sync.Mutex
	attempts int
	block    bool
}

func (*setupFailureProvider) Name() string                   { return "setup-failure" }
func (*setupFailureProvider) Credential() string             { return "mock" }
func (*setupFailureProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *setupFailureProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("500 Internal Server Error")
}

func (p *setupFailureProvider) attemptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

type gatewayFixture struct {
	server   *httptest.Server
	gateway  *Server
	central  *config.Config
	clients  *ClientStore
	client   Client
	token    string
	usage    *recordingUsage
	provider llm.Provider
}

func newGatewayFixture(t *testing.T, providerType config.ProviderType, provider llm.Provider, toolTimeout time.Duration) *gatewayFixture {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range []string{"claude", "grok", "cursor-agent", "gemini"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CURSOR_API_KEY", "test-cursor-key")
	dir := t.TempDir()
	clients, err := OpenClientStore(filepath.Join(dir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	client, token, err := clients.Add("satellite-a", Policy{AllowCLI: true, AllowSearch: true, AllowFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := OpenStateSealer(filepath.Join(dir, "state.key"))
	if err != nil {
		t.Fatal(err)
	}
	central := &config.Config{DefaultProvider: "remote", Providers: map[string]config.ProviderConfig{"remote": {Type: providerType, Model: "model-a", Models: []string{"model-a"}, APIKey: "super-secret-provider-key"}}}
	usage := &recordingUsage{}
	server, err := NewServer(ServerConfig{
		Config: central, Clients: clients, Sealer: sealer, Usage: usage,
		ProviderFactory: func(*config.Config, string, string) (llm.Provider, error) { return provider, nil },
		Searcher:        fakeSearcher{}, FetchTool: llm.NewReadURLToolWithFetcher(fakeFetcher{}),
		Policy: Policy{AllowCLI: true, AllowSearch: true, AllowFetch: true}, ToolTimeout: toolTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &gatewayFixture{server: ts, gateway: server, central: central, clients: clients, client: client, token: token, usage: usage, provider: provider}
}

func (f *gatewayFixture) satelliteConfig() *config.Config {
	return &config.Config{Gateway: config.GatewayConfig{URL: f.server.URL, Token: f.token, Search: gatewayBool(true), Fetch: gatewayBool(true), CatalogTTL: "1m", ConnectTimeout: "2s", ResponseTimeout: "2s", ToolTimeout: "2s"}, Providers: map[string]config.ProviderConfig{}}
}

func gatewayBool(value bool) *bool { return &value }

func collectStream(t *testing.T, stream llm.Stream) (string, llm.Usage) {
	t.Helper()
	defer stream.Close()
	var text strings.Builder
	var usage llm.Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.EventTextDelta {
			text.WriteString(event.Text)
		}
		if event.Type == llm.EventUsage && event.Use != nil {
			usage.Add(*event.Use)
		}
	}
	return text.String(), usage
}

func TestGatewayProviderServerSSEFidelityIsolationAndUsage(t *testing.T) {
	mock := llm.NewMockProvider("central").AddTurn(llm.MockTurn{Text: "hello from central", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3, CachedInputTokens: 2}})
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, mock, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	catalogReq, _ := http.NewRequest(http.MethodGet, fixture.server.URL+"/g1/catalog", nil)
	catalogReq.Header.Set("Authorization", "Bearer "+fixture.token)
	catalogReq.Header.Set("Term-LLM-Gateway-Version", "1")
	catalogResp, err := http.DefaultClient.Do(catalogReq)
	if err != nil {
		t.Fatal(err)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	catalogResp.Body.Close()
	if strings.Contains(string(catalogBody), "super-secret-provider-key") {
		t.Fatal("provider credential leaked through catalog")
	}
	stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", SessionID: "sess", WorkingDir: "/satellite/secret", Messages: []llm.Message{llm.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	text, usage := collectStream(t, stream)
	if text != "hello from central" || usage.InputTokens != 7 || usage.CachedInputTokens != 2 {
		t.Fatalf("text/usage = %q %+v", text, usage)
	}
	requests := mock.RecordedRequests()
	if len(requests) != 1 || requests[0].WorkingDir == "" || requests[0].WorkingDir == "/satellite/secret" {
		t.Fatalf("gateway working dir isolation failed: %+v", requests)
	}
	if _, err := os.Stat(requests[0].WorkingDir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral gateway working dir still exists: %s (%v)", requests[0].WorkingDir, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		fixture.usage.mu.Lock()
		count := len(fixture.usage.records)
		fixture.usage.mu.Unlock()
		if count > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fixture.usage.mu.Lock()
	defer fixture.usage.mu.Unlock()
	if len(fixture.usage.records) != 1 || fixture.usage.records[0].ClientID != fixture.client.ID || fixture.usage.records[0].ProviderKey != "remote" || fixture.usage.records[0].RequestID == "" || fixture.usage.records[0].InputTokens != 7 {
		t.Fatalf("usage attribution = %+v", fixture.usage.records)
	}
}

func TestGatewayNormalToolLoopExecutesOnSatellite(t *testing.T) {
	mock := llm.NewMockProvider("central").
		AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"normal"}`)}}}).
		AddTurn(llm.MockTurn{Text: "normal tool complete"})
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, mock, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	registry := llm.NewToolRegistry()
	registry.Register(echoTool{})
	engine := llm.NewEngine(provider, registry)
	stream, err := engine.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("use echo")}, Tools: []llm.ToolSpec{echoTool{}.Spec()}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := collectStream(t, stream)
	if text != "normal tool complete" {
		t.Fatalf("normal tool loop text = %q", text)
	}
	requests := mock.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("central provider requests = %d, want 2", len(requests))
	}
	foundResult := false
	for _, message := range requests[1].Messages {
		for _, part := range message.Parts {
			if part.ToolResult != nil && part.ToolResult.Content == "satellite:normal" {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Fatalf("satellite tool result missing from continuation request: %+v", requests[1].Messages)
	}
}

func TestGatewayInlineToolCallbackExecutesOnSatellite(t *testing.T) {
	inline := &inlineProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeCursorBin, inline, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	registry := llm.NewToolRegistry()
	registry.Register(echoTool{})
	engine := llm.NewEngine(provider, registry)
	stream, err := engine.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("use echo")}, Tools: []llm.ToolSpec{echoTool{}.Spec()}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := collectStream(t, stream)
	if text != "before after satellite:hello" {
		t.Fatalf("inline callback text = %q", text)
	}
	inline.mu.Lock()
	defer inline.mu.Unlock()
	if inline.response.Err != nil || inline.response.Result.Content != "satellite:hello" {
		t.Fatalf("central provider callback response = %+v", inline.response)
	}
}

func TestGatewayConcurrentInlineCallbacksDoNotDeadlock(t *testing.T) {
	inline := &inlineProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeCursorBin, inline, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			registry := llm.NewToolRegistry()
			registry.Register(echoTool{})
			engine := llm.NewEngine(provider, registry)
			stream, err := engine.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("use echo")}, Tools: []llm.ToolSpec{echoTool{}.Spec()}, MaxTurns: 3})
			if err != nil {
				errs <- err
				return
			}
			var text strings.Builder
			for {
				event, recvErr := stream.Recv()
				if recvErr == io.EOF {
					break
				}
				if recvErr != nil {
					errs <- recvErr
					return
				}
				if event.Type == llm.EventTextDelta {
					text.WriteString(event.Text)
				}
			}
			_ = stream.Close()
			results <- text.String()
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case text := <-results:
			if text != "before after satellite:hello" {
				t.Fatalf("concurrent callback text = %q", text)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent gateway callbacks deadlocked")
		}
	}
}

func TestGatewayInlineToolCallbackTimeout(t *testing.T) {
	inline := &inlineProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeCursorBin, inline, 30*time.Millisecond)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("call")}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := stream.Recv()
	if first.Text != "before " {
		t.Fatalf("first event = %+v", first)
	}
	callback, _ := stream.Recv()
	if callback.ToolResponse == nil {
		t.Fatalf("callback event missing response channel: %+v", callback)
	}
	next, err := stream.Recv()
	if err != nil || next.Text != "tool-error" {
		t.Fatalf("post-timeout event = %+v, %v", next, err)
	}
	_ = stream.Close()
}

type abandonedToolResponseProvider struct{ closed chan struct{} }

func (*abandonedToolResponseProvider) Name() string       { return "abandoned-callback" }
func (*abandonedToolResponseProvider) Credential() string { return "mock" }
func (*abandonedToolResponseProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true, InlineToolLoop: true}
}
func (p *abandonedToolResponseProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return &abandonedToolResponseStream{closed: p.closed}, nil
}

type abandonedToolResponseStream struct {
	closed chan struct{}
	sent   bool
	once   sync.Once
}

func (s *abandonedToolResponseStream) Recv() (llm.Event, error) {
	if !s.sent {
		s.sent = true
		return llm.Event{Type: llm.EventToolCall, ToolCallID: "abandoned", ToolName: "echo", ToolResponse: make(chan llm.ToolExecutionResponse)}, nil
	}
	return llm.Event{}, io.EOF
}
func (s *abandonedToolResponseStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestGatewayToolResponseSendUnblocksOnCancellation(t *testing.T) {
	central := &abandonedToolResponseProvider{closed: make(chan struct{})}
	fixture := newGatewayFixture(t, config.ProviderTypeCursorBin, central, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("call")}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil || event.ToolResponse == nil {
		t.Fatalf("callback event = %+v, %v", event, err)
	}
	event.ToolResponse <- llm.ToolExecutionResponse{Result: llm.TextOutput("no receiver")}
	time.Sleep(20 * time.Millisecond)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-central.closed:
	case <-time.After(time.Second):
		t.Fatal("gateway handler remained blocked sending ToolResponse after cancellation")
	}
}

func TestGatewayCrossClientStateAndRunAccessDenied(t *testing.T) {
	mock := llm.NewMockProvider("central").AddTextResponse("one").AddTextResponse("two")
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, mock, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	// A syntactically valid but unauthenticated state is rejected before provider execution.
	if err := provider.ImportProviderState([]byte("forged-state")); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("x")}}); err == nil || !strings.Contains(err.Error(), "invalid_state") {
		t.Fatalf("tampered state error = %v", err)
	}
	other, otherToken, err := fixture.clients.Add("satellite-b", Policy{AllowSearch: true, AllowFetch: true})
	if err != nil || other.ID == fixture.client.ID {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequest(http.MethodDelete, fixture.server.URL+"/g1/runs/not-owned", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+otherToken)
	httpReq.Header.Set("Term-LLM-Gateway-Version", "1")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign run access status = %d, want 404", resp.StatusCode)
	}
}

func TestGatewayProviderStateRoundTripsSealed(t *testing.T) {
	stateful := &statefulProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, stateful, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("turn")}})
		if err != nil {
			t.Fatal(err)
		}
		collectStream(t, stream)
	}
	stateful.mu.Lock()
	defer stateful.mu.Unlock()
	if stateful.imported != "state-1" {
		t.Fatalf("gateway provider state import = %q, want state-1", stateful.imported)
	}
	sealed, ok := provider.ExportProviderState()
	if !ok || strings.Contains(string(sealed), "state-2") {
		t.Fatalf("satellite state is not opaque/sealed: %q, %t", sealed, ok)
	}
}

func receiveGatewayFailure(t *testing.T, stream llm.Stream) error {
	t.Helper()
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if err != nil {
			return err
		}
	}
}

func TestGatewayUpstreamPersistent500HonorsDefaultAttemptBudget(t *testing.T) {
	failing := &setupFailureProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, failing, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stream, err := provider.Stream(t.Context(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("fail")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveGatewayFailure(t, stream); err == nil || !strings.Contains(err.Error(), "provider_upstream_failure") {
		t.Fatalf("persistent gateway failure = %v", err)
	}
	if attempts := failing.attemptCount(); attempts != DefaultUpstreamRetryAttempts {
		t.Fatalf("upstream attempts = %d, want %d", attempts, DefaultUpstreamRetryAttempts)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("persistent 500 took %s, want under 6s", elapsed)
	}
}

func TestGatewayUpstreamElapsedBudgetCancelsHungAttempt(t *testing.T) {
	failing := &setupFailureProvider{block: true}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, failing, time.Second)
	fixture.gateway.cfg.UpstreamRetryAttempts = 5
	fixture.gateway.cfg.UpstreamRetryMaxElapsed = 50 * time.Millisecond
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stream, err := provider.Stream(t.Context(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("hang")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveGatewayFailure(t, stream); err == nil {
		t.Fatal("hung gateway upstream unexpectedly succeeded")
	}
	if attempts := failing.attemptCount(); attempts != 1 {
		t.Fatalf("hung upstream attempts = %d, want 1", attempts)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("hung upstream exceeded elapsed budget: %s", elapsed)
	}
}

func TestGatewayCancellationClosesCentralStream(t *testing.T) {
	blocking := &blockingProvider{canceled: make(chan struct{})}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, blocking, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("block")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.canceled:
	case <-time.After(time.Second):
		t.Fatal("central stream was not canceled")
	}
	deadline := time.Now().Add(time.Second)
	for {
		release, ok := fixture.gateway.acquireInference(fixture.client)
		if ok {
			release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client inference permit was not released after cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		fixture.usage.mu.Lock()
		if len(fixture.usage.records) > 0 {
			record := fixture.usage.records[len(fixture.usage.records)-1]
			fixture.usage.mu.Unlock()
			if record.ErrorCode != "canceled" {
				t.Fatalf("cancellation usage error = %q, want canceled", record.ErrorCode)
			}
			break
		}
		fixture.usage.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("cancellation usage was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayStreamDeathIsControlled(t *testing.T) {
	dead := &streamDeathProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, dead, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("die")}})
	if err != nil {
		t.Fatal(err)
	}
	if event, err := stream.Recv(); err != nil || event.Text != "partial" {
		t.Fatalf("partial event = %+v, %v", event, err)
	}
	if _, err := stream.Recv(); err == nil || !strings.Contains(err.Error(), "provider_upstream_failure") || !strings.Contains(err.Error(), "retry") || strings.Contains(err.Error(), "secret upstream") {
		t.Fatalf("stream death error = %v", err)
	}
	_ = stream.Close()
}

func TestGatewaySearchAndFetch(t *testing.T) {
	mock := llm.NewMockProvider("central").AddTextResponse("unused")
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, mock, time.Second)
	client, err := search.NewGatewayClient(fixture.satelliteConfig().Gateway)
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.Search(context.Background(), "query", 3)
	if err != nil || len(results) != 1 || results[0].Snippet != "central" {
		t.Fatalf("search = %+v, %v", results, err)
	}
	content, err := client.FetchURL(context.Background(), "https://example.com/page")
	if err != nil || content != "central fetch" {
		t.Fatalf("fetch = %q, %v", content, err)
	}
}
