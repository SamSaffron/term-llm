package cmd

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type stagedStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	events <-chan llm.Event
}

func (s *stagedStream) Recv() (llm.Event, error) {
	select {
	case event, ok := <-s.events:
		if !ok {
			return llm.Event{}, io.EOF
		}
		return event, nil
	default:
	}

	select {
	case <-s.ctx.Done():
		return llm.Event{}, s.ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			return llm.Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *stagedStream) Close() error {
	s.cancel()
	return nil
}

type stagedProvider struct {
	mu            sync.Mutex
	requests      []llm.Request
	firstChunk    string
	secondChunk   string
	firstSent     chan struct{}
	releaseSecond chan struct{}
	closeFirst    sync.Once
}

type shutdownBlockingStream struct {
	ctx           context.Context
	release       <-chan struct{}
	cancelled     chan struct{}
	cancelledOnce sync.Once
}

func (s *shutdownBlockingStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	s.cancelledOnce.Do(func() { close(s.cancelled) })
	<-s.release
	return llm.Event{}, s.ctx.Err()
}

func (s *shutdownBlockingStream) Close() error {
	return nil
}

type shutdownBlockingProvider struct {
	started     chan struct{}
	cancelled   chan struct{}
	release     chan struct{}
	cleanupDone chan struct{}
	startOnce   sync.Once
	cleanupOnce sync.Once
}

func newShutdownBlockingProvider() *shutdownBlockingProvider {
	return &shutdownBlockingProvider{
		started:     make(chan struct{}),
		cancelled:   make(chan struct{}),
		release:     make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
}

func (p *shutdownBlockingProvider) Name() string {
	return "shutdown-blocking"
}

func (p *shutdownBlockingProvider) Credential() string {
	return "test"
}

func (p *shutdownBlockingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *shutdownBlockingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.startOnce.Do(func() { close(p.started) })
	return &shutdownBlockingStream{ctx: ctx, release: p.release, cancelled: p.cancelled}, nil
}

func (p *shutdownBlockingProvider) CleanupMCP() {
	p.cleanupOnce.Do(func() { close(p.cleanupDone) })
}

type countingListModelsProvider struct {
	name   string
	models []llm.ModelInfo
	calls  int32
}

func (p *countingListModelsProvider) Name() string { return p.name }

func (p *countingListModelsProvider) Credential() string { return "mock" }

func (p *countingListModelsProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (p *countingListModelsProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return nil, errors.New("not used in test")
}

func (p *countingListModelsProvider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	atomic.AddInt32(&p.calls, 1)
	out := make([]llm.ModelInfo, len(p.models))
	copy(out, p.models)
	return out, nil
}

func (p *countingListModelsProvider) CallCount() int {
	return int(atomic.LoadInt32(&p.calls))
}

func newStagedProvider(firstChunk, secondChunk string) *stagedProvider {
	return &stagedProvider{
		firstChunk:    firstChunk,
		secondChunk:   secondChunk,
		firstSent:     make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (p *stagedProvider) Name() string {
	return "staged"
}

func (p *stagedProvider) Credential() string {
	return "test"
}

func (p *stagedProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *stagedProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventTextDelta, Text: p.firstChunk}:
		}
		p.closeFirst.Do(func() { close(p.firstSent) })

		select {
		case <-streamCtx.Done():
			return
		case <-p.releaseSecond:
		}

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventTextDelta, Text: p.secondChunk}:
		}

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 1, OutputTokens: 2}}:
		}
	}()

	return &stagedStream{ctx: streamCtx, cancel: cancel, events: ch}, nil
}

type cancelPreserveProvider struct {
	mu            sync.Mutex
	requests      []llm.Request
	secondStarted chan struct{}
	secondOnce    sync.Once
}

func (p *cancelPreserveProvider) Name() string       { return "cancel-preserve" }
func (p *cancelPreserveProvider) Credential() string { return "test" }
func (p *cancelPreserveProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}
func (p *cancelPreserveProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	switch call {
	case 1:
		return &serveRuntimeTestStream{events: []llm.Event{{Type: llm.EventToolCall, Tool: &llm.ToolCall{
			ID:        "call-preserved-context",
			Name:      "slow_tool",
			Arguments: json.RawMessage(`{}`),
		}}}}, nil
	case 2:
		p.secondOnce.Do(func() { close(p.secondStarted) })
		return &serveRuntimeBlockingStream{ctx: ctx}, nil
	default:
		return &serveRuntimeTestStream{events: []llm.Event{{Type: llm.EventTextDelta, Text: "follow-up saw context"}}}, nil
	}
}

func (p *cancelPreserveProvider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

func newServeHTTPTestServer(srv *serveServer) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", srv.handleResponses)
	mux.HandleFunc("/v1/responses/", srv.handleResponseByID)
	return httptest.NewServer(mux)
}

func TestServeServerStopIsIdempotent(t *testing.T) {
	s := &serveServer{
		server:     &http.Server{},
		shutdownCh: make(chan struct{}),
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func readSSEEvent(t *testing.T, scanner *bufio.Scanner) (string, string, bool) {
	t.Helper()

	var eventName string
	dataLines := make([]string, 0, 1)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return eventName, strings.Join(dataLines, "\n"), true
		}
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	if eventName == "" && len(dataLines) == 0 {
		return "", "", false
	}
	return eventName, strings.Join(dataLines, "\n"), true
}

func TestResolvePlatforms(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		configPlatform []string
		want           []string
		wantErr        string
	}{
		{name: "single web", args: []string{"web"}, want: []string{"web"}},
		{name: "single telegram", args: []string{"telegram"}, want: []string{"telegram"}},
		{name: "multiple platforms", args: []string{"telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "all three", args: []string{"web", "jobs", "telegram"}, want: []string{"web", "jobs", "telegram"}},
		{name: "unknown platform", args: []string{"slack"}, wantErr: `unknown platform "slack"`},
		{name: "mixed valid and invalid", args: []string{"web", "invalid"}, wantErr: `unknown platform "invalid"`},
		{name: "case insensitive", args: []string{"WEB", "Telegram"}, want: []string{"web", "telegram"}},
		{name: "dedup", args: []string{"telegram", "telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "whitespace trimmed", args: []string{" web ", " telegram "}, want: []string{"web", "telegram"}},
		{name: "no args no config", args: nil, wantErr: "no platforms specified"},
		{name: "args override config", args: []string{"web"}, configPlatform: []string{"telegram"}, want: []string{"web"}},
		{name: "config fallback", args: nil, configPlatform: []string{"telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "config dedup", args: nil, configPlatform: []string{"web", "web"}, want: []string{"web"}},
		{name: "config unknown", args: nil, configPlatform: []string{"slack"}, wantErr: `unknown platform "slack"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePlatforms(tt.args, tt.configPlatform)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseSidebarSessionCategories(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		defaultAll bool
		want       []string
		wantErr    string
	}{
		{name: "default all", raw: "", defaultAll: true, want: []string{"all"}},
		{name: "empty allowed", raw: "", defaultAll: false, want: nil},
		{name: "dedup", raw: "chat, web, chat", defaultAll: true, want: []string{"chat", "web"}},
		{name: "all wins", raw: "chat,all,web", defaultAll: true, want: []string{"all"}},
		{name: "invalid", raw: "chat,nope", defaultAll: true, wantErr: "invalid --sidebar-sessions value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSidebarSessionCategories(tt.raw, tt.defaultAll)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlatformContains(t *testing.T) {
	tests := []struct {
		name      string
		platforms []string
		target    string
		want      bool
	}{
		{name: "found", platforms: []string{"web", "telegram"}, target: "telegram", want: true},
		{name: "not found", platforms: []string{"web", "jobs"}, target: "telegram", want: false},
		{name: "empty list", platforms: nil, target: "web", want: false},
		{name: "single match", platforms: []string{"web"}, target: "web", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := platformContains(tt.platforms, tt.target); got != tt.want {
				t.Fatalf("platformContains(%v, %q) = %v, want %v", tt.platforms, tt.target, got, tt.want)
			}
		})
	}
}

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "/ui", want: "/ui"},
		{name: "custom", input: "/chat", want: "/chat"},
		{name: "nested", input: "/app/v2", want: "/app/v2"},
		{name: "trailing slash stripped", input: "/chat/", want: "/chat"},
		{name: "multiple trailing slashes", input: "/chat///", want: "/chat"},
		{name: "no leading slash added", input: "chat", want: "/chat"},
		{name: "whitespace trimmed", input: "  /chat  ", want: "/chat"},
		{name: "root rejected", input: "/", wantErr: true},
		{name: "empty rejected", input: "", wantErr: true},
		{name: "whitespace only rejected", input: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeBasePath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeBasePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestServeServerConfig_RouteHelpers(t *testing.T) {
	tests := []struct {
		basePath   string
		wantUI     string
		wantImages string
		wantFiles  string
	}{
		{"/ui", "/ui/", "/ui/images/", "/ui/files/"},
		{"/chat", "/chat/", "/chat/images/", "/chat/files/"},
		{"/app/v2", "/app/v2/", "/app/v2/images/", "/app/v2/files/"},
	}
	for _, tt := range tests {
		cfg := serveServerConfig{basePath: tt.basePath}
		if got := cfg.uiRoute(); got != tt.wantUI {
			t.Errorf("basePath=%q uiRoute()=%q, want %q", tt.basePath, got, tt.wantUI)
		}
		if got := cfg.imagesRoute(); got != tt.wantImages {
			t.Errorf("basePath=%q imagesRoute()=%q, want %q", tt.basePath, got, tt.wantImages)
		}
		if got := cfg.filesRoute(); got != tt.wantFiles {
			t.Errorf("basePath=%q filesRoute()=%q, want %q", tt.basePath, got, tt.wantFiles)
		}
	}
}

func TestServeServerConfigAgentAvailable(t *testing.T) {
	dedicated := serveServerConfig{agentName: "jarvis"}
	if !dedicated.agentAvailable("jarvis") {
		t.Fatal("dedicated agent was unavailable")
	}
	for _, name := range []string{"", "developer", "missing"} {
		if dedicated.agentAvailable(name) {
			t.Errorf("dedicated agentAvailable(%q) = true, want false", name)
		}
	}

	multiAgent := serveServerConfig{agentNames: []string{"developer", "reviewer"}}
	for _, name := range []string{"developer", "reviewer"} {
		if !multiAgent.agentAvailable(name) {
			t.Errorf("multi-agent agentAvailable(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "jarvis", "missing"} {
		if multiAgent.agentAvailable(name) {
			t.Errorf("multi-agent agentAvailable(%q) = true, want false", name)
		}
	}
}

func TestHandleResponsesAcceptsDedicatedAgentForFreshThread(t *testing.T) {
	srv := newTestServeServer("thread response")
	defer srv.sessionMgr.Close()
	srv.cfg.agentName = "jarvis"
	var createdAgent string
	srv.agentRuntimeFactory = func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		agentName := request.Agent
		createdAgent = agentName
		runtime, err := srv.sessionMgr.factory(ctx)
		if runtime != nil {
			runtime.agentName = agentName
		}
		return runtime, err
	}

	code, response := doResponsesFirstParty(t, srv, `{"input":"continue in a new thread","agent":"jarvis","client_message_id":"thread-message"}`, "thread-child")
	if code != http.StatusOK {
		t.Fatalf("response status = %d, want 200: %#v", code, response)
	}
	if createdAgent != "jarvis" {
		t.Fatalf("created agent = %q, want jarvis", createdAgent)
	}

	code, response = doResponsesFirstParty(t, srv, `{"input":"continue in a new thread","agent":"developer","client_message_id":"foreign-agent-message"}`, "foreign-agent-thread")
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(response), "agent is not available") {
		t.Fatalf("foreign agent status/response = %d %#v, want unavailable-agent 400", code, response)
	}
}

func TestRuntimeForFreshAgentProviderRequestUsesSelectedAgent(t *testing.T) {
	mgr := newServeSessionManager(time.Hour, 10, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer mgr.Close()

	var createdAgent string
	srv := &serveServer{
		cfg:        serveServerConfig{},
		sessionMgr: mgr,
		agentRuntimeFactory: func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			agentName := request.Agent
			createdAgent = agentName
			return &serveRuntime{agentName: agentName}, nil
		},
	}
	rt, stateful, err := srv.runtimeForFreshAgentProviderRequest(context.Background(), "sess-agent", "", "reviewer")
	if err != nil {
		t.Fatalf("runtimeForFreshAgentProviderRequest: %v", err)
	}
	if !stateful {
		t.Fatal("selected-agent runtime should be stateful")
	}
	if createdAgent != "reviewer" || rt.agentName != "reviewer" {
		t.Fatalf("created agent = %q, runtime agent = %q; want reviewer", createdAgent, rt.agentName)
	}
}

func TestRuntimeForProviderRequestReplacesMismatchedCachedAgent(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted := &session.Session{ID: "sess-agent-restart", Provider: "test", Model: "model", Agent: "developer"}
	if err := store.Create(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}

	mgr := newServeSessionManager(time.Hour, 10, func(context.Context) (*serveRuntime, error) { return &serveRuntime{}, nil })
	defer mgr.Close()
	putTestSession(mgr, persisted.ID, &serveRuntime{agentName: "", maxTurns: 50})

	created := 0
	srv := &serveServer{
		cfg:        serveServerConfig{},
		cfgRef:     &config.Config{DefaultProvider: "test"},
		store:      store,
		sessionMgr: mgr,
		runtimeFactory: func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			return nil, errors.New("generic runtime factory should not be used")
		},
		agentRuntimeFactory: func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			agentName := request.Agent
			created++
			return &serveRuntime{agentName: agentName, maxTurns: 2000}, nil
		},
	}

	rt, stateful, err := srv.runtimeForProviderRequest(context.Background(), persisted.ID, "test")
	if err != nil {
		t.Fatalf("runtimeForProviderRequest: %v", err)
	}
	if !stateful || rt.agentName != "developer" || rt.maxTurns != 2000 || created != 1 {
		t.Fatalf("stateful=%v agent=%q maxTurns=%d created=%d", stateful, rt.agentName, rt.maxTurns, created)
	}
	cached, ok := mgr.Get(persisted.ID)
	if !ok || cached != rt {
		t.Fatal("replacement runtime was not cached")
	}
}

func TestCreateRequestRuntimeRejectsWrongAgentIdentity(t *testing.T) {
	srv := &serveServer{agentRuntimeFactory: func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		return &serveRuntime{agentName: ""}, nil
	}}
	if _, err := srv.createRequestRuntime(context.Background(), serveRuntimeRequest{Provider: "", Model: "", Agent: "developer"}); err == nil || !strings.Contains(err.Error(), "does not match requested agent") {
		t.Fatalf("createRequestRuntime error = %v, want identity mismatch", err)
	}
}

func TestSyncPersistedSessionRuntimeDoesNotClearNamedAgent(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted := &session.Session{ID: "sess-no-agent-downgrade", Provider: "test", Model: "model", Agent: "developer"}
	if err := store.Create(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store}
	rt := &serveRuntime{providerKey: "test", defaultModel: "model"}
	srv.syncPersistedSessionRuntime(context.Background(), persisted.ID, rt, "model", "", "", false, "", false)

	got, err := store.Get(context.Background(), persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "developer" {
		t.Fatalf("agent = %q, want developer", got.Agent)
	}
}

func TestCustomBasePath_EndToEnd(t *testing.T) {
	// Handlers are called with paths already stripped of basePath by
	// http.StripPrefix in the mux. So "/" is the SPA root, "/app.css" is
	// a static asset, and "/images/" is the images route.
	srv := &serveServer{
		cfg: serveServerConfig{
			ui:              true,
			basePath:        "/chat",
			uiTitle:         "My Lab",
			sidebarSessions: []string{"chat", "web"},
			agentName:       "jarvis",
			agentNames:      []string{"developer", "reviewer"},
		},
	}

	// 1. / serves the SPA with the injected prefix
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/ status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Prefix should be JSON-escaped (a proper JS string literal, not a template literal)
	if !strings.Contains(body, `TERM_LLM_UI_PREFIX="/chat"`) {
		t.Errorf("/ should inject JSON-escaped TERM_LLM_UI_PREFIX, got:\n%s",
			body[strings.Index(body, "TERM_LLM")-20:strings.Index(body, "TERM_LLM")+60])
	}
	if !strings.Contains(body, `<base href="/chat/">`) {
		t.Error("/ should inject <base> tag with basePath")
	}
	if !strings.Contains(body, `TERM_LLM_SIDEBAR_SESSIONS=["chat","web"]`) {
		t.Error("/ should inject TERM_LLM_SIDEBAR_SESSIONS")
	}
	if !strings.Contains(body, `TERM_LLM_LOCATION_SHARING_ENABLED=true`) {
		t.Error("/ should enable location sharing by default")
	}
	if !strings.Contains(body, `TERM_LLM_AGENT_NAME="jarvis"`) {
		t.Error("/ should inject TERM_LLM_AGENT_NAME")
	}
	if !strings.Contains(body, `TERM_LLM_AGENT_NAMES=["developer","reviewer"]`) {
		t.Error("/ should inject TERM_LLM_AGENT_NAMES")
	}
	if !strings.Contains(body, `TERM_LLM_UI_TITLE="My Lab"`) {
		t.Error("/ should inject TERM_LLM_UI_TITLE")
	}
	if !strings.Contains(body, `TERM_LLM_UI_VERSION=`) {
		t.Error("/ should inject TERM_LLM_UI_VERSION")
	}

	// 2. /dist/app.css serves the generated static asset.
	req = httptest.NewRequest(http.MethodGet, "/dist/app.css", nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/dist/app.css status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), ".app{") {
		t.Error("/dist/app.css should contain minified app styles")
	}

	// 3. /images/ serves images via handleImage (empty filename → 404)
	req = httptest.NewRequest(http.MethodGet, "/images/", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/images/ (empty filename) status = %d, want 404", rr.Code)
	}

	// 4. Traversal attempt also rejected
	req = httptest.NewRequest(http.MethodGet, "/images/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/images/ traversal status = %d, want 404", rr.Code)
	}
}

func TestBuildIndexHTMLDisablesApprovalControlsForYoloLaunch(t *testing.T) {
	for _, tc := range []struct {
		mode tools.ApprovalMode
		want string
	}{
		{mode: tools.ModeAuto, want: `window.TERM_LLM_APPROVALS_ENABLED=true`},
		{mode: tools.ModeYolo, want: `window.TERM_LLM_APPROVALS_ENABLED=false`},
	} {
		srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, approvalDefault: tc.mode}
		if body := string(srv.renderIndexHTML()); !strings.Contains(body, tc.want) {
			t.Fatalf("mode %s index missing %q", tc.mode, tc.want)
		}
	}
}

func TestBuildIndexHTMLBootstrapsWorktreeCapabilityAndReusesGitRoot(t *testing.T) {
	tests := []struct {
		name       string
		cwd        string
		wantGlobal string
		wantStatus int
	}{
		{name: "git cwd", cwd: newGitRepoForBindingTest(t), wantGlobal: `window.TERM_LLM_WORKTREES_ENABLED=true`, wantStatus: http.StatusOK},
		{name: "non-git cwd", cwd: t.TempDir(), wantGlobal: `window.TERM_LLM_WORKTREES_ENABLED=false`, wantStatus: http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootCalls := 0
			srv := &serveServer{
				cfg: serveServerConfig{basePath: "/ui"},
				worktreeRootFn: func() (string, error) {
					rootCalls++
					return tt.cwd, nil
				},
			}

			body := string(srv.renderIndexHTML())
			if !strings.Contains(body, tt.wantGlobal) {
				t.Fatalf("index missing %q", tt.wantGlobal)
			}

			rec := httptest.NewRecorder()
			srv.handleWorktrees(rec, httptest.NewRequest(http.MethodGet, "/v1/worktrees", nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("worktree list status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if rootCalls != 1 {
				t.Fatalf("worktree root detection called %d times, want once across bootstrap and request", rootCalls)
			}
		})
	}
}

func TestBuildIndexHTMLDisablesLocationSharing(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", locationSharingDisabled: true}}
	body := string(srv.buildIndexHTML(""))
	if !strings.Contains(body, `TERM_LLM_LOCATION_SHARING_ENABLED=false`) {
		t.Fatal("index should disable location sharing when configured")
	}
}

func TestServeHTTPHandler_MountsJobsOnlyAtRoot(t *testing.T) {
	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui"},
		jobsV2: &jobsV2Manager{},
	}
	h := srv.httpHandler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/ui/healthz", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/ui/healthz status = %d, want 404", rr.Code)
	}
}

func TestServeCORSExposesUIVersionHeader(t *testing.T) {
	srv := &serveServer{
		cfg: serveServerConfig{
			corsOrigins: []string{"https://example.com"},
		},
	}
	h := srv.cors(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()

	h(rr, req)

	if got := rr.Header().Get("X-Term-LLM-UI-Version"); got == "" {
		t.Fatal("X-Term-LLM-UI-Version header missing")
	}
	exposeHeaders := strings.ToLower(rr.Header().Get("Access-Control-Expose-Headers"))
	for _, name := range []string{"x-term-llm-ui-version", "x-term-llm-response-status"} {
		if !strings.Contains(exposeHeaders, name) {
			t.Fatalf("Access-Control-Expose-Headers = %q, want %s", exposeHeaders, name)
		}
	}
	allowHeaders := strings.ToLower(rr.Header().Get("Access-Control-Allow-Headers"))
	for _, name := range []string{"x-term-llm-ui-version", strings.ToLower(requestSessionIDHeader), legacyRequestSessionIDHeader} {
		if !strings.Contains(allowHeaders, strings.ToLower(name)) {
			t.Fatalf("Access-Control-Allow-Headers = %q, want %s", allowHeaders, name)
		}
	}
}

func TestServeHTTPHandler_MountsWebUnderBasePath(t *testing.T) {
	srv := &serveServer{
		cfg: serveServerConfig{ui: true, basePath: "/chat"},
	}
	h := srv.httpHandler()

	req := httptest.NewRequest(http.MethodGet, "/chat/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/chat/healthz status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("/healthz status = %d, want 307", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/chat/" {
		t.Fatalf("/healthz redirect location = %q, want %q", loc, "/chat/")
	}
}

func TestNormalizeBasePath_ProducesValidRoutes(t *testing.T) {
	// Verify that normalizeBasePath output always produces valid route helpers
	// and that handleUI/handleImage parse paths correctly.
	inputs := []struct {
		raw          string
		wantBasePath string
	}{
		{"chat", "/chat"},    // no leading slash → added
		{"/chat/", "/chat"},  // trailing slash → stripped
		{"  /app  ", "/app"}, // whitespace → trimmed
		{"/a/b/c", "/a/b/c"}, // nested path
		{"/ui", "/ui"},       // default
	}

	for _, tt := range inputs {
		bp, err := normalizeBasePath(tt.raw)
		if err != nil {
			t.Fatalf("normalizeBasePath(%q) unexpected error: %v", tt.raw, err)
		}
		if bp != tt.wantBasePath {
			t.Fatalf("normalizeBasePath(%q) = %q, want %q", tt.raw, bp, tt.wantBasePath)
		}

		cfg := serveServerConfig{ui: true, basePath: bp}

		// Route helpers produce valid patterns (non-empty, start with /)
		if ui := cfg.uiRoute(); ui == "" || ui[0] != '/' || ui[len(ui)-1] != '/' {
			t.Errorf("basePath=%q: uiRoute()=%q invalid", bp, ui)
		}
		if img := cfg.imagesRoute(); img == "" || img[0] != '/' || img[len(img)-1] != '/' {
			t.Errorf("basePath=%q: imagesRoute()=%q invalid", bp, img)
		}

		// handleUI serves SPA at "/" (basePath stripped by StripPrefix in mux)
		srv := &serveServer{cfg: cfg}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		srv.handleUI(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("basePath=%q: handleUI(/) status=%d, want 200", bp, rr.Code)
		}

		// handleUI serves generated assets after the base path is stripped.
		req = httptest.NewRequest(http.MethodGet, "/dist/app.css", nil)
		rr = httptest.NewRecorder()
		srv.handleUI(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("basePath=%q: handleUI(/dist/app.css) status=%d, want 200", bp, rr.Code)
		}

		// handleImage rejects empty filename at "/images/" (basePath stripped)
		req = httptest.NewRequest(http.MethodGet, "/images/", nil)
		rr = httptest.NewRecorder()
		srv.handleImage(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("basePath=%q: handleImage(/images/) status=%d, want 404", bp, rr.Code)
		}
	}
}

func TestNormalizeBasePath_RejectsInvalid(t *testing.T) {
	// These must all be rejected — if they weren't, they'd produce
	// empty or root-only routes that would panic or misbehave.
	invalid := []string{"/", "", "   ", "///"}
	for _, input := range invalid {
		_, err := normalizeBasePath(input)
		if err == nil {
			t.Errorf("normalizeBasePath(%q) should have returned an error", input)
		}
	}
}

func TestSingleServeTemplatePlatform(t *testing.T) {
	tests := []struct {
		name      string
		platforms []string
		want      string
	}{
		{name: "web only", platforms: []string{"web"}, want: "web"},
		{name: "telegram only", platforms: []string{"telegram"}, want: "telegram"},
		{name: "jobs only", platforms: []string{"jobs"}, want: "jobs"},
		{name: "web and telegram", platforms: []string{"web", "telegram"}, want: ""},
		{name: "web and jobs", platforms: []string{"web", "jobs"}, want: ""},
		{name: "unknown ignored", platforms: []string{"web", "foo"}, want: "web"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := singleServeTemplatePlatform(tt.platforms); got != tt.want {
				t.Fatalf("singleServeTemplatePlatform(%v) = %q, want %q", tt.platforms, got, tt.want)
			}
		})
	}
}

func TestHandleUI_ReturnsEmbeddedStaticAsset(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	// Paths are as seen by the handler after StripPrefix removes basePath.
	tests := []struct {
		name        string
		path        string
		contentType string
		bodySnippet string
	}{
		{name: "css", path: "/dist/app.css", contentType: "text/css; charset=utf-8", bodySnippet: ".app{"},
		{name: "module_js", path: "/dist/app.js", contentType: "text/javascript; charset=utf-8", bodySnippet: "term_llm_token"},
		{name: "lazy_chunk_js", path: "/dist/chunks/katex.js", contentType: "text/javascript; charset=utf-8", bodySnippet: "katex"},
		{name: "manifest", path: "/manifest.webmanifest", contentType: "", bodySnippet: `"display": "standalone"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rr := httptest.NewRecorder()

			srv.handleUI(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); tt.contentType != "" && !strings.HasPrefix(got, tt.contentType) {
				t.Fatalf("content-type = %q, want %s", got, tt.contentType)
			}
			if tt.bodySnippet != "" && !strings.Contains(rr.Body.String(), tt.bodySnippet) {
				t.Fatalf("expected %q in asset response, got %q", tt.bodySnippet, rr.Body.String())
			}
		})
	}
}

func TestHandleUI_UnknownScriptAndStyleDoNotReturnSPAShell(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	for _, path := range []string{"/missing.js", "/dist/chunks/missing.mjs", "/missing.css", "/dist/assets/missing.woff2", "/dist/assets/missing.png", "/dist/chunks/missing"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		srv.handleUI(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rr.Code)
		}
		if strings.Contains(rr.Body.String(), `<div id="root">`) {
			t.Errorf("%s returned SPA shell", path)
		}
	}
}

func TestHandleUI_VersionedAssetCaching(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	// Versioned generated asset gets immutable caching.
	req := httptest.NewRequest(http.MethodGet, "/dist/chunks/katex.js?v="+serveui.AssetVersion(), nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("versioned asset cache-control = %q, want immutable", got)
	}

	// Unversioned asset gets no-cache.
	req = httptest.NewRequest(http.MethodGet, "/dist/app.css", nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("unversioned asset cache-control = %q, want no-cache", got)
	}
}

func TestHandleUI_StaticAssetCompressionAndConditionalCaching(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()
	wantBody, err := serveui.StaticAsset("dist/app.css")
	if err != nil {
		t.Fatalf("StaticAsset(dist/app.css): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dist/app.css?v="+version, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("vary = %q, want Accept-Encoding", got)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected ETag on static asset")
	}
	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	gotBody, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll gzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("decompressed body mismatch")
	}

	req = httptest.NewRequest(http.MethodGet, "/dist/app.css?v="+version, nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain content-encoding = %q, want empty", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), wantBody) {
		t.Fatalf("plain body mismatch")
	}

	req = httptest.NewRequest(http.MethodGet, "/dist/app.css?v="+version, nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("conditional response body length = %d, want 0", rr.Body.Len())
	}

	req = httptest.NewRequest(http.MethodHead, "/dist/app.css?v="+version, nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatalf("expected ETag on HEAD response")
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", rr.Body.Len())
	}
}

func TestHandleUI_ServiceWorkerCompressionKeepsNoCache(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control = %q, want no-cache", got)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatalf("expected ETag on service worker")
	}
}

func TestHandleUI_IndexVersionsCacheableAssetsAndKeepsCanonicalModule(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		`href="manifest.webmanifest?v=` + version + `"`,
		`href="icon-512.png?v=` + version + `"`,
		`href="dist/app.css?v=` + version + `"`,
		`type="module" src="dist/app.js"`,
		`.startup-splash{`,
		`@keyframes startup-spin{`,
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in body", snippet)
		}
	}
	if strings.Contains(body, `src="dist/app.js?v=`) {
		t.Fatal("did not expect a version query on the canonical application module")
	}
	if strings.Index(body, `.startup-splash{`) > strings.Index(body, `href="dist/app.css?v=`+version+`"`) {
		t.Fatalf("expected inline startup styles before generated CSS link")
	}
	for _, snippet := range []string{
		`vendor/marked`,
		`vendor/dompurify`,
		`dist/chunks/katex.js`,
	} {
		if strings.Contains(body, snippet) {
			t.Fatalf("did not expect eager optional markdown asset %q in index", snippet)
		}
	}
}

func TestHandleUI_ServiceWorkerVersionsShellCache(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()

	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		`term-llm-shell-` + version,
		`'./manifest.webmanifest?v=` + version + `'`,
		`'./icon-512.png?v=` + version + `'`,
		`'./dist/app.css?v=` + version + `'`,
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in body", snippet)
		}
	}
	for _, snippet := range []string{
		`'./dist/app.js`,
		`'./dist/chunks/vendor.js`,
		`'./dist/chunks/webrtc.js`,
		`'./dist/chunks/katex.js`,
		`'./dist/chunks/highlight.js`,
	} {
		if strings.Contains(body, snippet) {
			t.Fatalf("did not expect lazy optional asset %q in shell precache", snippet)
		}
	}
}

func TestUiAssetCacheReusesGzipAcrossRequests(t *testing.T) {
	data := []byte("compressible content for cache test: " + strings.Repeat("abcdefgh", 64))
	e1 := uiGetOrBuildEntry(data, true)
	e2 := uiGetOrBuildEntry(data, true)
	if e1 != e2 {
		t.Fatal("expected same *uiAssetCacheEntry pointer on repeated call with same content")
	}
	if e1.etag == "" {
		t.Fatal("expected non-empty ETag")
	}
	if len(e1.compressed) == 0 {
		t.Fatal("expected non-empty compressed bytes")
	}
	zr, err := gzip.NewReader(bytes.NewReader(e1.compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = zr.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("decompressed bytes mismatch")
	}
	// Non-compressible entry (different data) must have nil compressed bytes.
	nonCompData := []byte("binary-like content for non-compressible test")
	e3 := uiGetOrBuildEntry(nonCompData, false)
	if e3.compressed != nil {
		t.Fatal("expected nil compressed bytes for non-compressible entry")
	}
}

func TestWriteJSONGzip_CompressesLargeResponse(t *testing.T) {
	payload := map[string]any{"data": strings.Repeat("abcdef", 200)}
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rr := httptest.NewRecorder()

	writeJSONGzip(rr, req, http.StatusOK, payload)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll gzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(decompressed, &got); err != nil {
		t.Fatalf("decode decompressed JSON: %v", err)
	}
	if got["data"] != payload["data"] {
		t.Fatalf("round trip mismatch")
	}
}

func TestWriteJSONGzip_NoCompressionWithoutAcceptEncoding(t *testing.T) {
	payload := map[string]any{"data": strings.Repeat("abcdef", 200)}
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	rr := httptest.NewRecorder()

	writeJSONGzip(rr, req, http.StatusOK, payload)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("content-encoding = %q, want empty", got)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode plain JSON: %v", err)
	}
	if got["data"] != payload["data"] {
		t.Fatalf("round trip mismatch")
	}
}

func TestWriteJSONGzip_SkipsCompressionForSmallPayload(t *testing.T) {
	payload := map[string]any{"ok": true}
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()

	writeJSONGzip(rr, req, http.StatusOK, payload)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("content-encoding = %q, want empty", got)
	}
	if rr.Body.Len() > jsonGzipMinBytes {
		t.Fatalf("test payload is not small: %d", rr.Body.Len())
	}
	var got map[string]bool
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode plain JSON: %v", err)
	}
	if !got["ok"] {
		t.Fatalf("round trip mismatch")
	}
}

func TestServeServer_RenderIndexHTMLCached(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat"}}
	a := srv.renderIndexHTML()
	b := srv.renderIndexHTML()
	if len(a) == 0 {
		t.Fatal("renderIndexHTML returned empty slice")
	}
	if &a[0] != &b[0] {
		t.Fatal("expected renderIndexHTML to return the same cached backing array on repeated calls")
	}
	if !bytes.Contains(a, []byte(`window.TERM_LLM_UI_PREFIX`)) {
		t.Fatal("rendered index HTML missing UI prefix script")
	}
}

func TestParseResponsesInput_String(t *testing.T) {
	msgs, replaceHistory, err := parseResponsesInput(json.RawMessage(`"hello"`))
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msgs[0].Role)
	}
	if got := msgs[0].Parts[0].Text; got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}
}

func TestParseResponsesInput_PreservesPerItemClientMessageIDs(t *testing.T) {
	msgs, replaceHistory, err := parseResponsesInput(json.RawMessage(`[
		{"type":"message","role":"user","client_message_id":"msg-first","content":"first"},
		{"type":"message","role":"user","client_message_id":"msg-second","content":"second"}
	]`))
	if err != nil {
		t.Fatalf("parseResponsesInput: %v", err)
	}
	if !replaceHistory {
		t.Fatal("ordinary multi-message input unexpectedly changed from replacement semantics")
	}
	if len(msgs) != 2 || msgs[0].ClientMessageID != "msg-first" || msgs[1].ClientMessageID != "msg-second" {
		t.Fatalf("parsed client identities = %#v", msgs)
	}
}

func TestPrepareResponseClientMessageIDs_ValidatesFirstPartyBatch(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, ClientMessageID: "msg-first"},
		{Role: llm.RoleUser, ClientMessageID: "msg-second"},
	}
	isBatch, err := prepareResponseClientMessageIDs(messages, "msg-second", true)
	if err != nil || !isBatch {
		t.Fatalf("valid batch: isBatch=%v err=%v", isBatch, err)
	}

	for _, test := range []struct {
		name      string
		messages  []llm.Message
		requestID string
	}{
		{name: "missing item id", messages: []llm.Message{{Role: llm.RoleUser, ClientMessageID: "msg-first"}, {Role: llm.RoleUser}}, requestID: "msg-second"},
		{name: "duplicate item id", messages: []llm.Message{{Role: llm.RoleUser, ClientMessageID: "same"}, {Role: llm.RoleUser, ClientMessageID: "same"}}, requestID: "same"},
		{name: "wrong final id", messages: []llm.Message{{Role: llm.RoleUser, ClientMessageID: "msg-first"}, {Role: llm.RoleUser, ClientMessageID: "msg-second"}}, requestID: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := prepareResponseClientMessageIDs(test.messages, test.requestID, true); err == nil {
				t.Fatal("expected batch identity validation error")
			}
		})
	}
}

func TestParseResponsesInput_DeveloperDoesNotSuppressServerSystemPrompt(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"message","role":"developer","content":"Be concise"},
		{"type":"message","role":"user","content":"hello"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleDeveloper {
		t.Fatalf("first role = %s, want developer", msgs[0].Role)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		systemPrompt: "server system prompt",
	}
	_, err = rt.Run(context.Background(), true, replaceHistory, msgs, llm.Request{})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 3 {
		t.Fatalf("request message count = %d, want 3", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleSystem || provider.Requests[0].Messages[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("first request message = %+v, want server system prompt", provider.Requests[0].Messages[0])
	}
	if provider.Requests[0].Messages[1].Role != llm.RoleDeveloper || provider.Requests[0].Messages[1].Parts[0].Text != "Be concise" {
		t.Fatalf("second request message = %+v, want developer prompt", provider.Requests[0].Messages[1])
	}
	if provider.Requests[0].Messages[2].Role != llm.RoleUser || provider.Requests[0].Messages[2].Parts[0].Text != "hello" {
		t.Fatalf("third request message = %+v, want user prompt", provider.Requests[0].Messages[2])
	}
}

func TestParseResponsesInput_ImageContent(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
			{"type":"input_text","text":"describe this image"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msg.Role)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartImage {
		t.Fatalf("parts[0].type = %s, want image", msg.Parts[0].Type)
	}
	if msg.Parts[0].ImageData == nil {
		t.Fatalf("parts[0].ImageData = nil")
	}
	if msg.Parts[0].ImageData.MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", msg.Parts[0].ImageData.MediaType)
	}
	if msg.Parts[0].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("base64 = %q, want aGVsbG8=", msg.Parts[0].ImageData.Base64)
	}
	if msg.Parts[0].ImagePath == "" {
		t.Fatalf("parts[0].ImagePath is empty, want saved upload path")
	}
	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("uploads dir has %d files, want 1", len(entries))
	}
	got, err := os.ReadFile(msg.Parts[0].ImagePath)
	if err != nil {
		t.Fatalf("read saved image: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("saved image bytes = %q, want hello", got)
	}
	if msg.Parts[1].Type != llm.PartText {
		t.Fatalf("parts[1].type = %s, want text", msg.Parts[1].Type)
	}
	if msg.Parts[1].Text != "describe this image" {
		t.Fatalf("parts[1].text = %q, want %q", msg.Parts[1].Text, "describe this image")
	}
}

func TestParseResponsesInput_FileUploadSavesToDisk(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	// base64 of "hello world"
	b64 := "aGVsbG8gd29ybGQ="
	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_file","file_data":"data:application/pdf;base64,` + b64 + `","filename":"doc.pdf"},
			{"type":"input_text","text":"summarize this"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if len(msg.Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartFile {
		t.Fatalf("parts[0].type = %s, want file", msg.Parts[0].Type)
	}
	if msg.Parts[0].FileData == nil {
		t.Fatalf("parts[0].FileData = nil")
	}
	if msg.Parts[0].FileData.MediaType != "application/pdf" {
		t.Fatalf("media type = %q, want application/pdf", msg.Parts[0].FileData.MediaType)
	}
	if msg.Parts[0].FileData.Base64 != b64 {
		t.Fatalf("base64 = %q, want %q", msg.Parts[0].FileData.Base64, b64)
	}
	if !strings.Contains(msg.Parts[0].Text, "doc.pdf") {
		t.Fatalf("parts[0].text = %q, should mention doc.pdf", msg.Parts[0].Text)
	}
	if msg.Parts[1].Text != "summarize this" {
		t.Fatalf("parts[1].text = %q, want %q", msg.Parts[1].Text, "summarize this")
	}

	// Verify file was actually written to disk with correct content
	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("uploads dir has %d files, want 1", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(uploadsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("file content = %q, want %q", got, "hello world")
	}
	// Verify restrictive permissions
	info, _ := entries[0].Info()
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("file permissions = %o, want no group/other access", perm)
	}
	// Non-native fallback must expose a usable saved path to tools.
	if !strings.Contains(msg.Parts[0].Text, msg.Parts[0].FilePath) {
		t.Fatalf("parts[0].text missing upload path: %q", msg.Parts[0].Text)
	}
}

func TestParseResponsesInput_TextFileUploadEmbedsFallback(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	b64 := base64.StdEncoding.EncodeToString([]byte("name,count\napples,3\n"))
	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_file","file_data":"data:application/octet-stream;base64,` + b64 + `","filename":"data.csv"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Parts) != 1 {
		t.Fatalf("messages = %#v, want one file part", msgs)
	}
	part := msgs[0].Parts[0]
	if part.Type != llm.PartFile {
		t.Fatalf("part type = %s, want file", part.Type)
	}
	if part.FileData == nil || part.FileData.MediaType != "text/csv" {
		t.Fatalf("file data = %#v, want inferred text/csv", part.FileData)
	}
	if !strings.Contains(part.Text, "--- BEGIN USER-PROVIDED FILE: data.csv (text/csv) ---") ||
		!strings.Contains(part.Text, "name,count") ||
		!strings.Contains(part.Text, "```csv") ||
		!strings.Contains(part.Text, "--- END USER-PROVIDED FILE: data.csv ---") {
		t.Fatalf("fallback text = %q, want marked embedded csv content", part.Text)
	}
	if strings.Contains(part.Text, dataHome) {
		t.Fatalf("fallback text leaks upload storage path: %q", part.Text)
	}
}

func TestParseResponsesInput_InvalidBase64ReturnsError(t *testing.T) {

	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_file","file_data":"data:application/pdf;base64,!!!invalid!!!","filename":"bad.pdf"}
		]}
	]`)
	_, _, err := parseResponsesInput(payload)
	if err == nil {
		t.Fatalf("expected error for invalid base64, got nil")
	}
	if !strings.Contains(err.Error(), "bad.pdf") {
		t.Fatalf("error = %q, should mention filename", err.Error())
	}
}

func TestAbbreviatePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got := abbreviatePath(home + "/foo/bar.txt")
	if got != "~/foo/bar.txt" {
		t.Fatalf("abbreviatePath(%q) = %q, want %q", home+"/foo/bar.txt", got, "~/foo/bar.txt")
	}
	// Paths outside home are returned unchanged
	got = abbreviatePath("/tmp/other.txt")
	if got != "/tmp/other.txt" {
		t.Fatalf("abbreviatePath(%q) = %q, want unchanged", "/tmp/other.txt", got)
	}
}

func TestParseDataURL(t *testing.T) {
	mt, b64 := parseDataURL("data:image/jpeg;base64,/9j/4AAQ")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg", mt)
	}
	if b64 != "/9j/4AAQ" {
		t.Fatalf("base64 = %q, want /9j/4AAQ", b64)
	}

	mt, b64 = parseDataURL("not-a-data-url")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for invalid data URL, got %q %q", mt, b64)
	}

	mt, b64 = parseDataURL("data:text/plain;charset=utf-8,hello")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for non-base64 data URL, got %q %q", mt, b64)
	}
}

func TestParseResponsesInput_FunctionCallOutputDoesNotReplaceHistory(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[1].Role != llm.RoleTool {
		t.Fatalf("second role = %s, want tool", msgs[1].Role)
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.ID != "call_1" {
		t.Fatalf("missing tool result id")
	}
}

func TestParseResponsesInput_FunctionCallHistoryWithUserReplacesHistory(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"},
		{"type":"message","role":"user","content":"what next?"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}
	if msgs[2].Role != llm.RoleUser {
		t.Fatalf("third role = %s, want user", msgs[2].Role)
	}
}

func TestParseChatMessages_ToolCallAndToolResult(t *testing.T) {
	msgs, replaceHistory, err := parseChatMessages([]chatMessage{
		{
			Role:    "assistant",
			Content: json.RawMessage(`"running"`),
			ToolCalls: []chatToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "read_file",
					Arguments: `{"path":"a.txt"}`,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Content:    json.RawMessage(`"done"`),
		},
	})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("first role = %s, want assistant", msgs[0].Role)
	}
	if msgs[1].Role != llm.RoleTool {
		t.Fatalf("second role = %s, want tool", msgs[1].Role)
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.Name != "read_file" {
		t.Fatalf("tool result name missing")
	}
}

func TestPopulateMissingToolResultNames_UsesHistory(t *testing.T) {
	messages, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    json.RawMessage(`"done"`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false for tool-only follow-up")
	}

	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:        "call_1",
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"a.txt"}`),
			},
		}},
	}}

	populateMissingToolResultNames(messages, history)

	if got := messages[0].Parts[0].ToolResult.Name; got != "read_file" {
		t.Fatalf("tool result name = %q, want %q", got, "read_file")
	}
}

func TestPopulateResponsesToolResultNames_UsesRuntimeHistoryBeforeStore(t *testing.T) {
	messages := []llm.Message{{
		Role: llm.RoleTool,
		Parts: []llm.Part{{
			Type: llm.PartToolResult,
			ToolResult: &llm.ToolResult{
				ID: "call_1",
			},
		}},
	}}
	runtime := &serveRuntime{history: []llm.Message{{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:   "call_1",
				Name: "read_file",
			},
		}},
	}}}
	store := newServeRuntimeTestStore()
	srv := &serveServer{store: store}

	srv.populateResponsesToolResultNames(context.Background(), "sess-runtime", runtime, messages)

	if got := messages[0].Parts[0].ToolResult.Name; got != "read_file" {
		t.Fatalf("tool result name = %q, want %q", got, "read_file")
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("GetMessages calls = %d, want 0 when runtime history resolves all names", store.getMessagesCalls)
	}
}

func TestPopulateResponsesToolResultNames_FallsBackToStoreForUnresolvedNames(t *testing.T) {
	messages := []llm.Message{{
		Role: llm.RoleTool,
		Parts: []llm.Part{{
			Type: llm.PartToolResult,
			ToolResult: &llm.ToolResult{
				ID: "call_2",
			},
		}},
	}}
	store := newServeRuntimeTestStore()
	store.messages["sess-store"] = []session.Message{*session.NewMessage("sess-store", llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:   "call_2",
				Name: "bash",
			},
		}},
	}, 0)}
	srv := &serveServer{store: store}

	srv.populateResponsesToolResultNames(context.Background(), "sess-store", nil, messages)

	if got := messages[0].Parts[0].ToolResult.Name; got != "bash" {
		t.Fatalf("tool result name = %q, want %q", got, "bash")
	}
	if store.getMessagesCalls != 1 {
		t.Fatalf("GetMessages calls = %d, want 1 when runtime cannot resolve names", store.getMessagesCalls)
	}
}

type pagedResponsesToolNameStore struct {
	*serveRuntimeTestStore
	pageCalls int
}

func (s *pagedResponsesToolNameStore) GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageCalls++
	msgs := s.messages[sessionID]
	out := make([]session.Message, 0)
	if limit > 0 {
		out = make([]session.Message, 0, limit)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if beforeSeq > 0 && msgs[i].Sequence >= beforeSeq {
			continue
		}
		out = append(out, msgs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func TestPopulateResponsesToolResultNames_UsesReversePagedStoreLookup(t *testing.T) {
	messages := []llm.Message{{
		Role: llm.RoleTool,
		Parts: []llm.Part{{
			Type: llm.PartToolResult,
			ToolResult: &llm.ToolResult{
				ID: "call_older",
			},
		}},
	}}
	base := newServeRuntimeTestStore()
	store := &pagedResponsesToolNameStore{serveRuntimeTestStore: base}
	for i := 0; i < 300; i++ {
		msg := llm.AssistantText(fmt.Sprintf("filler-%d", i))
		if i == 42 {
			msg = llm.Message{
				Role: llm.RoleAssistant,
				Parts: []llm.Part{{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:   "call_older",
						Name: "grep",
					},
				}},
			}
		}
		store.messages["sess-paged"] = append(store.messages["sess-paged"], *session.NewMessage("sess-paged", msg, i))
	}
	srv := &serveServer{store: store}

	srv.populateResponsesToolResultNames(context.Background(), "sess-paged", nil, messages)

	if got := messages[0].Parts[0].ToolResult.Name; got != "grep" {
		t.Fatalf("tool result name = %q, want %q", got, "grep")
	}
	if store.pageCalls < 2 {
		t.Fatalf("GetMessagesPageDescending calls = %d, want a reverse-paged scan", store.pageCalls)
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("GetMessages calls = %d, want 0 when reverse pager is available", store.getMessagesCalls)
	}
}

func TestParseChatMessages_DeveloperDoesNotSuppressServerSystemPrompt(t *testing.T) {
	msgs, replaceHistory, err := parseChatMessages([]chatMessage{
		{Role: "developer", Content: json.RawMessage(`"Be concise"`)},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleDeveloper {
		t.Fatalf("first role = %s, want developer", msgs[0].Role)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		systemPrompt: "server system prompt",
	}
	_, err = rt.Run(context.Background(), true, replaceHistory, msgs, llm.Request{})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 3 {
		t.Fatalf("request message count = %d, want 3", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleSystem || provider.Requests[0].Messages[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("first request message = %+v, want server system prompt", provider.Requests[0].Messages[0])
	}
	if provider.Requests[0].Messages[1].Role != llm.RoleDeveloper || provider.Requests[0].Messages[1].Parts[0].Text != "Be concise" {
		t.Fatalf("second request message = %+v, want developer prompt", provider.Requests[0].Messages[1])
	}
	if provider.Requests[0].Messages[2].Role != llm.RoleUser || provider.Requests[0].Messages[2].Parts[0].Text != "hello" {
		t.Fatalf("third request message = %+v, want user prompt", provider.Requests[0].Messages[2])
	}
}

func TestParseChatMessages_UserImageContent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	msgs, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}},
			{"type":"text","text":"describe this"}
		]`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msgs[0].Role)
	}
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Type != llm.PartImage {
		t.Fatalf("parts[0].type = %s, want image", msgs[0].Parts[0].Type)
	}
	if msgs[0].Parts[0].ImageData == nil || msgs[0].Parts[0].ImageData.MediaType != "image/png" || msgs[0].Parts[0].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("parts[0].image = %#v, want png data URL", msgs[0].Parts[0].ImageData)
	}
	if msgs[0].Parts[1].Type != llm.PartText || msgs[0].Parts[1].Text != "describe this" {
		t.Fatalf("parts[1] = %+v, want trailing text", msgs[0].Parts[1])
	}
}

func TestParseChatMessages_AssistantImageContent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	msgs, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"text","text":"looking"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
		]`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("role = %s, want assistant", msgs[0].Role)
	}
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Type != llm.PartText || msgs[0].Parts[0].Text != "looking" {
		t.Fatalf("parts[0] = %+v, want leading text", msgs[0].Parts[0])
	}
	if msgs[0].Parts[1].Type != llm.PartImage {
		t.Fatalf("parts[1].type = %s, want image", msgs[0].Parts[1].Type)
	}
	if msgs[0].Parts[1].ImageData == nil || msgs[0].Parts[1].ImageData.MediaType != "image/png" || msgs[0].Parts[1].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("parts[1].image = %#v, want png data URL", msgs[0].Parts[1].ImageData)
	}
}

func TestParseToolChoice(t *testing.T) {
	if got := parseToolChoice(json.RawMessage(`"none"`)); got.Mode != llm.ToolChoiceNone {
		t.Fatalf("mode = %s, want none", got.Mode)
	}
	if got := parseToolChoice(json.RawMessage(`{"type":"function","name":"shell"}`)); got.Mode != llm.ToolChoiceName || got.Name != "shell" {
		t.Fatalf("name choice = %#v", got)
	}
}

func TestServeAuthMiddleware(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}}
	h := srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lowercase bearer: status = %d, want 204", rr.Code)
	}
}

type serveSessionBlockingCleanupProvider struct {
	started     chan struct{}
	release     chan struct{}
	done        chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	doneOnce    sync.Once
}

func newServeSessionBlockingCleanupProvider() *serveSessionBlockingCleanupProvider {
	return &serveSessionBlockingCleanupProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (p *serveSessionBlockingCleanupProvider) Name() string       { return "blocking-cleanup" }
func (p *serveSessionBlockingCleanupProvider) Credential() string { return "test" }
func (p *serveSessionBlockingCleanupProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}
func (p *serveSessionBlockingCleanupProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("not used")
}
func (p *serveSessionBlockingCleanupProvider) CleanupMCP() {
	p.startedOnce.Do(func() { close(p.started) })
	<-p.release
	p.doneOnce.Do(func() { close(p.done) })
}
func (p *serveSessionBlockingCleanupProvider) unblock() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func TestServeSessionManager_GetOrCreateSingleFactoryCall(t *testing.T) {
	var calls int32
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(25 * time.Millisecond)
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	const workers = 12
	results := make(chan *serveRuntime, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rt, err := manager.GetOrCreate(context.Background(), "same-id")
			if err != nil {
				errs <- err
				return
			}
			results <- rt
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrCreate error: %v", err)
		}
	}

	var first *serveRuntime
	for rt := range results {
		if first == nil {
			first = rt
			continue
		}
		if rt != first {
			t.Fatalf("expected all calls to return same runtime pointer")
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestServeSessionManager_EvictExpiredSkipsActiveRun(t *testing.T) {
	manager := newServeSessionManager(10*time.Millisecond, 10, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := &serveRuntime{}
	rt.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	state := &runtimeInterruptState{cancel: cancel, done: make(chan struct{})}
	rt.setActiveInterrupt(state)

	manager.mu.Lock()
	manager.sessions["busy"] = rt
	manager.mu.Unlock()

	manager.evictExpired()

	manager.mu.Lock()
	_, ok := manager.sessions["busy"]
	manager.mu.Unlock()
	if !ok {
		t.Fatal("expected active expired session to remain in manager")
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions after active session sweep = %d, want 0", got)
	}
	select {
	case <-ctx.Done():
		t.Fatal("expected active session not to be cancelled during eviction sweep")
	default:
	}

	rt.clearActiveInterrupt(state)
	manager.evictExpired()

	manager.mu.Lock()
	_, ok = manager.sessions["busy"]
	manager.mu.Unlock()
	if ok {
		t.Fatal("expected inactive expired session to be evicted")
	}
	if got := evictions.Load(); got != 1 {
		t.Fatalf("evictions after inactive session sweep = %d, want 1", got)
	}
}

func TestServeSessionManager_EvictExpiredBoundsBlockingCleanup(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	manager.retirementTimeout = 50 * time.Millisecond
	defer manager.Close()

	provider := newServeSessionBlockingCleanupProvider()
	t.Cleanup(func() {
		provider.unblock()
		select {
		case <-provider.done:
		case <-time.After(time.Second):
			t.Error("blocking cleanup did not finish after release")
		}
	})
	blocked := &serveRuntime{provider: provider}
	blocked.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())

	manager.mu.Lock()
	manager.sessions["blocked"] = blocked
	manager.mu.Unlock()

	done := make(chan struct{})
	go func() {
		manager.evictExpired()
		close(done)
	}()

	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("blocking cleanup did not start")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expired-session eviction remained blocked in provider cleanup")
	}

	followup, followupProvider := newCloseTrackingServeRuntime()
	followup.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	manager.mu.Lock()
	manager.sessions["followup"] = followup
	manager.mu.Unlock()

	manager.evictExpired()
	if !followupProvider.closed.Load() {
		t.Fatal("subsequent expired runtime was not cleaned up")
	}
	manager.mu.Lock()
	_, exists := manager.sessions["followup"]
	manager.mu.Unlock()
	if exists {
		t.Fatal("subsequent expired runtime was not evicted")
	}
}

func TestServeSessionManager_CloseBoundsBlockingCleanup(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	manager.retirementTimeout = 50 * time.Millisecond
	provider := newServeSessionBlockingCleanupProvider()
	t.Cleanup(func() {
		provider.unblock()
		select {
		case <-provider.done:
		case <-time.After(time.Second):
			t.Error("blocking cleanup did not finish after release")
		}
	})
	putTestSession(manager, "blocked", &serveRuntime{provider: provider})
	done := make(chan struct{})
	go func() {
		manager.Close()
		close(done)
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("blocking cleanup did not start")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close remained blocked in provider cleanup")
	}
}

func TestServeSessionManager_CloseContextDoesNotBlockOnStuckRuntime(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	t.Cleanup(manager.Close)

	runCtx, runCancel := context.WithCancel(context.Background())
	rt := &serveRuntime{}
	state := &runtimeInterruptState{cancel: runCancel, done: make(chan struct{})}
	rt.setActiveInterrupt(state)
	rt.mu.Lock()
	t.Cleanup(func() {
		rt.mu.Unlock()
		rt.clearActiveInterrupt(state)
		runCancel()
	})
	putTestSession(manager, "busy", rt)

	closeCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	started := time.Now()
	go func() {
		manager.CloseContext(closeCtx)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("CloseContext took %s with an expired close context", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseContext blocked on a runtime that never released its mutex")
	}

	select {
	case <-runCtx.Done():
	default:
		t.Fatal("CloseContext did not cancel the active runtime before waiting")
	}

	manager.mu.Lock()
	closed := manager.closed
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()
	if !closed {
		t.Fatal("expected manager to be marked closed")
	}
	if sessionCount != 0 {
		t.Fatalf("sessions after CloseContext = %d, want 0", sessionCount)
	}
}

func TestServeSessionManager_GetOrCreateSkipsEvictingActiveRunAtCapacity(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 2, func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	var evicted *serveRuntime
	manager.onEvict = func(rt *serveRuntime) {
		evicted = rt
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	idle := &serveRuntime{}
	idle.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.sessions["idle"] = idle
	manager.mu.Unlock()

	created, err := manager.GetOrCreate(context.Background(), "new")
	if err != nil {
		t.Fatalf("GetOrCreate error: %v", err)
	}
	if created == nil {
		t.Fatal("expected created runtime")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, idleOK := manager.sessions["idle"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if idleOK {
		t.Fatal("expected idle session to be evicted instead of active session")
	}
	if !newOK {
		t.Fatal("expected new session to be stored in manager")
	}
	if sessionCount != 2 {
		t.Fatalf("session count = %d, want 2", sessionCount)
	}
	if evicted != idle {
		t.Fatal("expected idle session to be evicted")
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity eviction")
	default:
	}
}

func TestServeSessionManager_GetOrCreateBoundsCapacityCleanup(t *testing.T) {
	created := &serveRuntime{}
	manager := newServeSessionManager(time.Minute, 1, func(context.Context) (*serveRuntime, error) {
		return created, nil
	})
	manager.retirementTimeout = 50 * time.Millisecond
	defer manager.Close()

	provider := newServeSessionBlockingCleanupProvider()
	t.Cleanup(func() {
		provider.unblock()
		select {
		case <-provider.done:
		case <-time.After(time.Second):
			t.Error("blocking cleanup did not finish after release")
		}
	})
	idle := &serveRuntime{provider: provider}
	idle.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	manager.mu.Lock()
	manager.sessions["idle"] = idle
	manager.mu.Unlock()

	type result struct {
		rt  *serveRuntime
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		rt, err := manager.GetOrCreate(context.Background(), "new")
		resultCh <- result{rt: rt, err: err}
	}()

	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("capacity cleanup did not start")
	}
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("GetOrCreate error: %v", got.err)
		}
		if got.rt != created {
			t.Fatalf("GetOrCreate runtime = %p, want %p", got.rt, created)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GetOrCreate remained blocked in evicted runtime cleanup")
	}
}

func TestServeSessionManager_GetOrCreateAdmitsWhenAllCachedSessionsAreBusy(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 1, func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.mu.Unlock()

	created, err := manager.GetOrCreate(context.Background(), "new")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if created == nil {
		t.Fatal("expected new runtime to be admitted")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if !newOK {
		t.Fatal("expected new session to be stored while cached sessions are busy")
	}
	if sessionCount != 2 {
		t.Fatalf("session count = %d, want temporary overflow to 2", sessionCount)
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity check")
	default:
	}
}

func TestServeSessionManager_GetOrCreateWithAdmitsWhenAllCachedSessionsAreBusy(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 1, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.mu.Unlock()

	created, err := manager.GetOrCreateWith(context.Background(), "new", func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateWith: %v", err)
	}
	if created == nil {
		t.Fatal("expected new runtime to be admitted")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if !newOK {
		t.Fatal("expected new session to be stored while cached sessions are busy")
	}
	if sessionCount != 2 {
		t.Fatalf("session count = %d, want temporary overflow to 2", sessionCount)
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity check")
	default:
	}
}

func TestServeSessionManager_ReplaceIdleWith_RestoresExistingSessionWhenCreateFails(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	existing := &serveRuntime{
		pendingApprovals: map[string]*servePendingApproval{
			"appr_1": {
				ApprovalID: "appr_1",
				Path:       "/tmp/file.txt",
				Options: []tools.ApprovalOption{{
					Label:  "Allow once",
					Choice: tools.ApprovalChoiceOnce,
				}},
				CreatedAt: time.Now(),
				responseC: make(chan serveApprovalSubmission, 1),
			},
		},
	}
	putTestSession(manager, "sess", existing)

	replaceErr := errors.New("replacement failed")
	got, err := manager.ReplaceIdleWith(
		context.Background(),
		"sess",
		func(existing *serveRuntime) bool { return true },
		func(ctx context.Context) (*serveRuntime, error) {
			return nil, replaceErr
		},
	)
	if !errors.Is(err, replaceErr) {
		t.Fatalf("ReplaceIdleWith error = %v, want %v", err, replaceErr)
	}
	if got != nil {
		t.Fatalf("ReplaceIdleWith runtime = %v, want nil", got)
	}

	restored, ok := manager.Get("sess")
	if !ok {
		t.Fatal("expected existing session to remain in manager after replacement failure")
	}
	if restored != existing {
		t.Fatal("expected existing runtime pointer to be preserved after replacement failure")
	}
	if prompts := existing.pendingApprovalPrompts(); len(prompts) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(prompts))
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
}

type serveSessionCloseTrackingProvider struct {
	closed atomic.Bool
}

func (p *serveSessionCloseTrackingProvider) Name() string       { return "close-tracking" }
func (p *serveSessionCloseTrackingProvider) Credential() string { return "test" }
func (p *serveSessionCloseTrackingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}
func (p *serveSessionCloseTrackingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &serveRuntimeTestStream{}, nil
}
func (p *serveSessionCloseTrackingProvider) CleanupMCP() { p.closed.Store(true) }

func newCloseTrackingServeRuntime() (*serveRuntime, *serveSessionCloseTrackingProvider) {
	provider := &serveSessionCloseTrackingProvider{}
	rt := &serveRuntime{provider: provider}
	rt.Touch()
	return rt, provider
}

func TestServeSessionManager_BeginSwapInstallsCandidateAndReturnsPrevious(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, _ := newCloseTrackingServeRuntime()
	candidate, _ := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	gotCandidate, gotPrevious, commit, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	defer rollback()
	if gotCandidate != candidate {
		t.Fatalf("candidate = %p, want %p", gotCandidate, candidate)
	}
	if gotPrevious != previous {
		t.Fatalf("previous = %p, want %p", gotPrevious, previous)
	}
	current, ok := manager.Get("swap")
	if !ok || current != candidate {
		t.Fatalf("manager current = %p ok=%v, want candidate %p", current, ok, candidate)
	}
	if commit == nil || rollback == nil {
		t.Fatal("expected commit and rollback callbacks")
	}
}

func TestServeSessionManager_BeginSwapRollbackRestoresPreviousAndClosesCandidate(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, prevProvider := newCloseTrackingServeRuntime()
	candidate, candProvider := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	_, _, _, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	rollback()
	current, ok := manager.Get("swap")
	if !ok || current != previous {
		t.Fatalf("manager current = %p ok=%v, want previous %p", current, ok, previous)
	}
	if !candProvider.closed.Load() {
		t.Fatal("expected rollback to close candidate")
	}
	if prevProvider.closed.Load() {
		t.Fatal("expected rollback to keep previous open")
	}
}

func TestServeSessionManager_BeginSwapCommitKeepsCandidateAndClosesPrevious(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, prevProvider := newCloseTrackingServeRuntime()
	candidate, candProvider := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	_, _, commit, _, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	commit()
	current, ok := manager.Get("swap")
	if !ok || current != candidate {
		t.Fatalf("manager current = %p ok=%v, want candidate %p", current, ok, candidate)
	}
	if !prevProvider.closed.Load() {
		t.Fatal("expected commit to close previous")
	}
	if candProvider.closed.Load() {
		t.Fatal("expected commit to keep candidate open")
	}
}

func TestServeSessionManager_BeginSwapBusyPreviousReturnsBusy(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, _ := newCloseTrackingServeRuntime()
	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	previous.setActiveInterrupt(busyState)
	defer previous.clearActiveInterrupt(busyState)
	putTestSession(manager, "swap", previous)

	created := false
	candidate, previousOut, commit, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		created = true
		rt, _ := newCloseTrackingServeRuntime()
		return rt, nil
	})
	if !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("BeginSwap error = %v, want %v", err, errServeSessionBusy)
	}
	if created {
		t.Fatal("factory should not be called for busy previous runtime")
	}
	if candidate != nil || previousOut != nil || commit != nil || rollback != nil {
		t.Fatalf("expected nil results on busy error")
	}
}

func TestRequireJSONContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	if err := requireJSONContentType(req); err == nil {
		t.Fatalf("expected error for missing Content-Type")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	if err := requireJSONContentType(req); err == nil {
		t.Fatalf("expected error for non-json Content-Type")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if err := requireJSONContentType(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewServeEngineWithTools_ConfiguresToolManagerAndSpawnWiring(t *testing.T) {
	cfg := &config.Config{}
	settings := SessionSettings{Tools: tools.ReadFileToolName}
	provider := llm.NewMockProvider("mock")

	wireCalls := 0
	gotYolo := false
	wireSpawn := func(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
		wireCalls++
		if cfg == nil {
			t.Fatalf("cfg = nil")
		}
		if toolMgr == nil {
			t.Fatalf("toolMgr = nil")
		}
		gotYolo = yoloMode
		return nil
	}

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, "mock", "mock-model", true, false, wireSpawn, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if engine == nil {
		t.Fatalf("engine = nil")
	}
	if toolMgr == nil {
		t.Fatalf("toolMgr = nil")
	}
	if !toolMgr.ApprovalMgr.YoloEnabled() {
		t.Fatalf("toolMgr.ApprovalMgr.YoloEnabled() = false, want true")
	}
	if wireCalls != 1 {
		t.Fatalf("wireCalls = %d, want 1", wireCalls)
	}
	if !gotYolo {
		t.Fatalf("yolo mode not passed to spawn wiring")
	}
	if _, ok := engine.Tools().Get(tools.ReadFileToolName); !ok {
		t.Fatalf("expected %q tool to be registered on engine", tools.ReadFileToolName)
	}
}

func TestNewServeEngineWithTools_SkipsToolManagerWhenToolsDisabled(t *testing.T) {
	cfg := &config.Config{}
	settings := SessionSettings{}
	provider := llm.NewMockProvider("mock")

	wireCalls := 0
	wireSpawn := func(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
		wireCalls++
		return nil
	}

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, "mock", "mock-model", false, false, wireSpawn, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if engine == nil {
		t.Fatalf("engine = nil")
	}
	if toolMgr != nil {
		t.Fatalf("toolMgr != nil, want nil")
	}
	if wireCalls != 0 {
		t.Fatalf("wireCalls = %d, want 0", wireCalls)
	}
}

func TestNewServeEngineWithTools_ConfiguresContextManagement(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "serve-test-model", InputLimit: 1234}})
	defer llm.RegisterConfigLimits(nil)

	cfg := &config.Config{AutoCompact: true}
	provider := llm.NewMockProvider("mock")

	engine, _, err := newServeEngineWithTools(cfg, SessionSettings{}, provider, "mock", "serve-test-model", false, false, nil, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if got := engine.InputLimit(); got != 1234 {
		t.Fatalf("engine.InputLimit() = %d, want 1234", got)
	}

	compactionConfig := reflect.ValueOf(engine).Elem().FieldByName("compactionConfig")
	if !compactionConfig.IsValid() {
		t.Fatalf("compactionConfig field not found")
	}
	if compactionConfig.IsNil() {
		t.Fatalf("compactionConfig = nil, want enabled when auto_compact is true")
	}
}

func TestNewServeEngineWithTools_TracksContextWhenAutoCompactDisabled(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "serve-test-model", InputLimit: 1234}})
	defer llm.RegisterConfigLimits(nil)

	cfg := &config.Config{AutoCompact: false}
	provider := llm.NewMockProvider("mock")

	engine, _, err := newServeEngineWithTools(cfg, SessionSettings{}, provider, "mock", "serve-test-model", false, false, nil, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if got := engine.InputLimit(); got != 1234 {
		t.Fatalf("engine.InputLimit() = %d, want 1234", got)
	}

	compactionConfig := reflect.ValueOf(engine).Elem().FieldByName("compactionConfig")
	if !compactionConfig.IsValid() {
		t.Fatalf("compactionConfig field not found")
	}
	if !compactionConfig.IsNil() {
		t.Fatalf("compactionConfig != nil, want tracking-only when auto_compact is false")
	}
}

func TestServeRuntimeRun_ReconfiguresContextManagementForRequestModel(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{
		{Provider: "mock", Model: "default-model", InputLimit: 1000},
		{Provider: "mock", Model: "override-model", InputLimit: 2000},
	})
	defer llm.RegisterConfigLimits(nil)

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	engine := llm.NewEngine(provider, nil)
	engine.ConfigureContextManagement(provider, "mock", "default-model", false)

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "default-model",
	}
	rt.Touch()

	_, err := rt.Run(context.Background(), false, false, []llm.Message{llm.UserText("hello")}, llm.Request{
		SessionID: "request-model-override",
		Model:     "override-model",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := engine.InputLimit(); got != 2000 {
		t.Fatalf("engine.InputLimit() = %d, want 2000 after request model override", got)
	}
}

type blockingModelSwitchTool struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingModelSwitchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "blocking_model_switch", Description: "Waits for a model switch", Schema: map[string]any{"type": "object"}}
}

func (t *blockingModelSwitchTool) Execute(ctx context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	close(t.started)
	select {
	case <-ctx.Done():
		return llm.ToolOutput{}, ctx.Err()
	case <-t.release:
		return llm.TextOutput("released"), nil
	}
}

func (t *blockingModelSwitchTool) Preview(_ json.RawMessage) string { return "" }

func TestServeRuntimePersistsModelSwitchAtProviderTurnBoundary(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").
		WithCapabilities(llm.Capabilities{ToolCalls: true}).
		AddToolCall("before", "blocking_model_switch", map[string]any{}).
		AddToolCall("after", "echo", map[string]any{}).
		AddTextResponse("done")
	blocking := &blockingModelSwitchTool{started: make(chan struct{}), release: make(chan struct{})}
	registry := llm.NewToolRegistry()
	registry.Register(blocking)
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)
	runtime := &serveRuntime{
		provider: provider, providerKey: "mock", defaultModel: "old-model",
		engine: engine, store: store,
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), true, false, []llm.Message{llm.UserText("work")}, llm.Request{
			SessionID: "model-switch-boundary", Model: "old-model", ReasoningEffort: "high", MaxTurns: 4,
			Tools: []llm.ToolSpec{blocking.Spec(), (&echoTool{}).Spec()},
		})
		done <- runErr
	}()
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool execution")
	}
	engine.QueueRequestRuntimeSwitch("new-model", "medium")
	close(blocking.release)
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for switched run")
	}

	messages, err := store.GetMessages(context.Background(), "model-switch-boundary", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	markerIndex := -1
	markerCount := 0
	beforeIndex := -1
	afterIndex := -1
	for index, message := range messages {
		if marker, ok := llm.ParseModelSwapMarker(message.ToLLMMessage()); ok {
			markerCount++
			markerIndex = index
			if marker.FromModel != "old-model" || marker.ToModel != "new-model" || marker.FromEffort != "high" || marker.ToEffort != "medium" || marker.BoundaryID == "" {
				t.Fatalf("marker = %#v", marker)
			}
		}
		for _, part := range message.Parts {
			if part.ToolResult != nil && part.ToolResult.ID == "before" {
				beforeIndex = index
			}
			if part.ToolCall != nil && part.ToolCall.ID == "after" {
				afterIndex = index
			}
		}
	}
	if markerCount != 1 || beforeIndex < 0 || markerIndex <= beforeIndex || afterIndex <= markerIndex {
		t.Fatalf("durable boundary order/count before=%d marker=%d after=%d count=%d messages=%#v", beforeIndex, markerIndex, afterIndex, markerCount, messages)
	}
}

// echoTool is a minimal tool for testing the agentic loop in serve.
type echoTool struct{}

func (e *echoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "echo",
		Description: "Echoes input",
		Schema:      map[string]any{"type": "object"},
	}
}

func (e *echoTool) Execute(_ context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("echoed"), nil
}

func (e *echoTool) Preview(_ json.RawMessage) string { return "" }

type secondEchoTool struct{}

func (e *secondEchoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "second_echo",
		Description: "Echoes input a second way",
		Schema:      map[string]any{"type": "object"},
	}
}

func (e *secondEchoTool) Execute(_ context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("second echoed"), nil
}

func (e *secondEchoTool) Preview(_ json.RawMessage) string { return "" }

func TestResponseServerTools_FirstPartyUIHeaderIncludesServerTools(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/chat/v1/responses", nil)
	if isFirstPartyUIResponseRequest(req) {
		t.Fatal("request without UI version header was treated as first-party UI")
	}

	req.Header.Set("X-Term-LLM-UI-Version", "a154f9083d98")
	if !isFirstPartyUIResponseRequest(req) {
		t.Fatal("request with UI version header was not treated as first-party UI")
	}
}

func TestResponseServerTools_IncludeServerToolsKeepsAllRegisteredTools(t *testing.T) {
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	registry.Register(&secondEchoTool{})
	provider := llm.NewMockProvider("mock")
	rt := &serveRuntime{engine: llm.NewEngine(provider, registry)}

	requested := map[string]bool{"client_music_tool": true}
	filtered := responseServerTools(rt, requested, false)
	if len(filtered) != 0 {
		t.Fatalf("filtered server tools = %d, want 0 for client-only requested tools", len(filtered))
	}

	included := responseServerTools(rt, requested, true)
	got := map[string]bool{}
	for _, spec := range included {
		got[spec.Name] = true
	}
	if !got["echo"] || !got["second_echo"] {
		t.Fatalf("included server tools = %v, want echo and second_echo", got)
	}
}

func TestAppendResponsePassthroughTools_AddsClientToolsWithoutDuplicatingServerTools(t *testing.T) {
	serverTools := []llm.ToolSpec{{Name: "echo"}}
	passthrough := []llm.ToolSpec{{Name: "echo"}, {Name: "client_music_tool"}}

	tools := appendResponsePassthroughTools(serverTools, passthrough, nil)
	if len(tools) != 2 {
		t.Fatalf("tool count = %d, want 2", len(tools))
	}
	if tools[0].Name != "echo" || tools[1].Name != "client_music_tool" {
		t.Fatalf("tools = %#v, want echo plus client_music_tool", tools)
	}
}

func TestServeRuntimeRun_PersistsToolCallMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Script: turn 1 = tool call, turn 2 = final text after tool result
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{
		SessionID: "toolcall-persist-test",
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}
	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("call the echo tool"),
	}, req)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Fetch persisted messages
	msgs, err := store.GetMessages(context.Background(), "toolcall-persist-test", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}

	// Expect: user, assistant(tool_call), tool(result), assistant(text)
	var hasToolCall bool
	var hasToolResult bool
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall && p.ToolCall != nil && p.ToolCall.Name == "echo" {
				hasToolCall = true
			}
			if p.Type == llm.PartToolResult && p.ToolResult != nil && p.ToolResult.ID == "call-1" {
				hasToolResult = true
			}
		}
	}
	if !hasToolCall {
		t.Fatalf("persisted messages missing assistant tool_call part; messages: %d", len(msgs))
	}
	if !hasToolResult {
		t.Fatalf("persisted messages missing tool_result part; messages: %d", len(msgs))
	}
}

func TestServeRuntimeRun_PersistsSessionAndMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").AddTextResponse("hello from serve")
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
		search:       true,
		toolsSetting: tools.ReadFileToolName,
		mcpSetting:   "playwright",
		agentName:    "reviewer",
	}
	rt.Touch()

	req := llm.Request{
		SessionID: "serve-session-1",
		MaxTurns:  3,
	}
	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("test persistence"),
	}, req)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	sess, err := store.Get(context.Background(), "serve-session-1")
	if err != nil {
		t.Fatalf("Get session failed: %v", err)
	}
	if sess == nil {
		t.Fatalf("session was not persisted")
	}
	if sess.Summary == "" {
		t.Fatalf("session summary was not set")
	}

	msgs, err := store.GetMessages(context.Background(), "serve-session-1", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("message count = %d, want >= 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("first role = %s, want user", msgs[0].Role)
	}
	if msgs[len(msgs)-1].Role != llm.RoleAssistant {
		t.Fatalf("last role = %s, want assistant", msgs[len(msgs)-1].Role)
	}
}

func TestInterruptMessageSerializesPersistenceWithCancellation(t *testing.T) {
	rt := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("engine"), nil)}
	persistStarted := make(chan struct{})
	allowPersist := make(chan struct{})
	state := &runtimeInterruptState{
		cancel: func() {},
		done:   make(chan struct{}),
		persistPendingSteering: func(context.Context, llm.QueuedSteering) error {
			close(persistStarted)
			<-allowPersist
			return nil
		},
	}
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)

	interruptDone := make(chan error, 1)
	go func() {
		_, _, err := rt.InterruptMessage(
			context.Background(), llm.UserText("change course"), "change course", "race-1", nil, interruptDeliverySteer,
		)
		interruptDone <- err
	}()
	<-persistStarted
	cancelled := make(chan bool, 1)
	go func() {
		ok, err := rt.cancelPendingSteering(context.Background(), "session", "race-1")
		cancelled <- ok && err == nil
	}()
	select {
	case <-cancelled:
		t.Fatal("cancellation passed persistence before the steering was queued")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowPersist)
	if err := <-interruptDone; err != nil {
		t.Fatalf("InterruptMessage: %v", err)
	}
	if ok := <-cancelled; !ok {
		t.Fatal("serialized cancellation did not remove the queued steering")
	}
	if entries := rt.engine.ListPendingSteering(); len(entries) != 0 {
		t.Fatalf("engine pending entries after cancellation = %#v", entries)
	}
}

func TestInterruptMessagePersistsPendingUntilDurableCommit(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sessionID = "sess-durable-steering"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Provider: "mock", Model: "mock"}); err != nil {
		t.Fatal(err)
	}

	rt := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("engine"), nil), store: store}
	state := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.configurePendingSteeringPersistence(state, sessionID)
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)

	action, _, err := rt.InterruptMessage(
		context.Background(), llm.UserText("change course"), "change course", "web-pending-1", nil, interruptDeliverySteer,
	)
	if err != nil || action != llm.InterruptSteer {
		t.Fatalf("InterruptMessage action=%q err=%v", action, err)
	}
	pendingStore, ok := session.AsPendingSteeringStore(store)
	if !ok {
		t.Fatal("SQLite store does not expose pending steering persistence")
	}
	entries, err := pendingStore.ListPendingSteering(context.Background(), sessionID)
	if err != nil || len(entries) != 1 || entries[0].ID != "web-pending-1" || entries[0].Message.ClientMessageID != "web-pending-1" {
		t.Fatalf("durable pending entries=%#v err=%v", entries, err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionState(rr, req, sessionID)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"id":"web-pending-1"`) {
		t.Fatalf("state status/body=%d %s", rr.Code, rr.Body.String())
	}

	committed := llm.UserText("change course")
	committed.ClientMessageID = "web-pending-1"
	result := rt.appendMessagesDetailed(context.Background(), sessionID, []llm.Message{committed}, 0)
	if !result.Complete {
		t.Fatalf("committed steering persistence=%#v", result)
	}
	entries, err = pendingStore.ListPendingSteering(context.Background(), sessionID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("pending entries after commit=%#v err=%v", entries, err)
	}

	if _, _, err := rt.InterruptMessage(
		context.Background(), llm.UserText("one more"), "one more", "web-abandoned-2", nil, interruptDeliverySteer,
	); err != nil {
		t.Fatalf("queue abandoned steering: %v", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	rt.discardPendingSteering(cleanupCtx, sessionID)
	cleanupCancel()
	entries, err = pendingStore.ListPendingSteering(context.Background(), sessionID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("pending entries after run cleanup=%#v err=%v", entries, err)
	}

	detached := llm.UserText("cancel from another tab")
	detached.ClientMessageID = "web-detached-3"
	if err := pendingStore.SavePendingSteering(context.Background(), session.PendingSteering{
		SessionID: sessionID, ID: detached.ClientMessageID, Message: detached, DisplayText: llm.MessageText(detached),
	}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID+"/steering/"+detached.ClientMessageID, nil)
	rr = httptest.NewRecorder()
	srv.handleSessionSteeringCancel(rr, req, sessionID, detached.ClientMessageID)
	if rr.Code != http.StatusOK {
		t.Fatalf("detached cancel status/body=%d %s", rr.Code, rr.Body.String())
	}
	entries, err = pendingStore.ListPendingSteering(context.Background(), sessionID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("pending entries after detached cancel=%#v err=%v", entries, err)
	}

	rt.clearActiveInterrupt(state)
	idle := llm.UserText("cancel from loaded idle runtime")
	idle.ClientMessageID = "web-idle-4"
	if err := pendingStore.SavePendingSteering(context.Background(), session.PendingSteering{
		SessionID: sessionID, ID: idle.ClientMessageID, Message: idle, DisplayText: llm.MessageText(idle),
	}); err != nil {
		t.Fatal(err)
	}
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, rt)
	idleServer := &serveServer{store: store, sessionMgr: manager}
	req = httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID+"/steering/"+idle.ClientMessageID, nil)
	rr = httptest.NewRecorder()
	idleServer.handleSessionSteeringCancel(rr, req, sessionID, idle.ClientMessageID)
	if rr.Code != http.StatusOK {
		t.Fatalf("idle runtime cancel status/body=%d %s", rr.Code, rr.Body.String())
	}

	filtered := llm.UserText("already committed")
	filtered.ClientMessageID = "web-filtered-4"
	if err := pendingStore.SavePendingSteering(context.Background(), session.PendingSteering{
		SessionID: sessionID, ID: filtered.ClientMessageID, Message: filtered, DisplayText: llm.MessageText(filtered),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(context.Background(), sessionID, session.NewMessage(sessionID, filtered, -1)); err != nil {
		t.Fatal(err)
	}
	entries, err = pendingStore.ListPendingSteering(context.Background(), sessionID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("committed client id was still listed as pending: entries=%#v err=%v", entries, err)
	}

	restored := llm.UserText("restore failed follow-up")
	restored.ClientMessageID = "web-claimed-5"
	if err := pendingStore.SavePendingSteering(context.Background(), session.PendingSteering{
		SessionID: sessionID, ID: restored.ClientMessageID, Message: restored, DisplayText: llm.MessageText(restored),
	}); err != nil {
		t.Fatal(err)
	}
	rt.engine.QueueSteering(llm.QueuedSteering{ID: restored.ClientMessageID, Message: restored})
	if claim := rt.engine.ClaimSteeringEntry(restored.ClientMessageID); claim != llm.SteeringClaimed {
		t.Fatalf("claim status=%q", claim)
	}
	rt.releaseClaimedPendingSteering(context.Background(), sessionID, []string{restored.ClientMessageID})
	engineEntries := rt.engine.ListPendingSteering()
	if len(engineEntries) != 1 || engineEntries[0].ID != restored.ClientMessageID {
		t.Fatalf("restored engine steering=%#v", engineEntries)
	}
}

func TestHandleSessionInterrupt_DeduplicatesRetriedImageSteering(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	rt := &serveRuntime{engine: engine, providerKey: "mock"}
	state := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)
	putTestSession(mgr, "sess-merge", rt)

	srv := &serveServer{sessionMgr: mgr}
	body := `{"message":"please inspect this image","steering_id":"web-1","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8=","filename":"img.png"}]}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-merge/interrupt", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200; body=%s", i+1, rr.Code, rr.Body.String())
		}
		if i == 0 {
			// A transport retry can arrive after the active run has already ended.
			rt.clearActiveInterrupt(state)
		}
	}
	entries := engine.ListPendingSteering()
	if len(entries) != 1 {
		t.Fatalf("pending entries after duplicate steering ID = %d, want 1", len(entries))
	}

	mismatch := `{"message":"different message","steering_id":"web-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-merge/interrupt", strings.NewReader(mismatch))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("mismatched idempotency payload status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}

	deliveryMismatch := `{"message":"please inspect this image","steering_id":"web-1","delivery":"steer","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8=","filename":"img.png"}]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-merge/interrupt", strings.NewReader(deliveryMismatch))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("mismatched idempotency delivery status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}

	parts := entries[0].Message.Parts
	if len(parts) != 2 || parts[0].Type != llm.PartImage || parts[1].Type != llm.PartText || parts[1].Text != "please inspect this image" {
		t.Fatalf("queued parts = %#v, want image plus message text", parts)
	}
}

func TestHandleSessionInterrupt_DeliveryDispatch(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	tests := []struct {
		name       string
		delivery   string
		wantStatus int
		wantAction string
		wantCancel int
		wantQueued int
	}{
		{name: "omitted auto dispatches explicit command", wantStatus: http.StatusOK, wantAction: "cancel", wantCancel: 1},
		{name: "auto dispatches explicit command", delivery: `,"delivery":"auto"`, wantStatus: http.StatusOK, wantAction: "cancel", wantCancel: 1},
		{name: "steer bypasses auto dispatch", delivery: `,"delivery":"steer"`, wantStatus: http.StatusOK, wantAction: "steer", wantQueued: 1},
		{name: "unknown rejected", delivery: `,"delivery":"rush"`, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newServeSessionManager(time.Minute, 10, nil)
			defer mgr.Close()
			engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
			cancelCalls := 0
			rt := &serveRuntime{engine: engine, providerKey: "mock"}
			state := &runtimeInterruptState{cancel: func() { cancelCalls++ }, done: make(chan struct{})}
			rt.setActiveInterrupt(state)
			defer rt.clearActiveInterrupt(state)
			putTestSession(mgr, "sess-delivery", rt)

			srv := &serveServer{sessionMgr: mgr}
			body := fmt.Sprintf(`{"message":"/stop","steering_id":"delivery-1"%s}`, tt.delivery)
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-delivery/interrupt", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if tt.wantAction != "" && !strings.Contains(rr.Body.String(), `"action":"`+tt.wantAction+`"`) {
				t.Fatalf("body = %s, want action %q", rr.Body.String(), tt.wantAction)
			}
			if cancelCalls != tt.wantCancel {
				t.Fatalf("cancel calls = %d, want %d", cancelCalls, tt.wantCancel)
			}
			if pending := engine.ListPendingSteering(); len(pending) != tt.wantQueued {
				t.Fatalf("pending = %#v, want %d entries", pending, tt.wantQueued)
			}
		})
	}
}

func TestHandleSessionInterrupt_ValidatesDeliveryBeforeContentIO(t *testing.T) {
	srv := &serveServer{}
	body := `{"delivery":"rush","steering_id":"delivery-early","content":[{"type":"input_file","filename":"bad.pdf","file_data":"%%%"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-delivery/interrupt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleSessionInterrupt(rr, req, "sess-delivery")

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unsupported interrupt delivery") {
		t.Fatalf("status/body = %d %s, want delivery validation before content parsing", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "bad.pdf") {
		t.Fatalf("content parsing ran before delivery validation: %s", rr.Body.String())
	}
}

func TestInterruptMessage_DeliveryControlsFastClassifier(t *testing.T) {
	for _, tt := range []struct {
		name          string
		delivery      interruptDelivery
		wantAction    llm.InterruptAction
		wantFastTurns int
		wantQueued    int
	}{
		{name: "auto invokes fast classifier", delivery: interruptDeliveryAuto, wantAction: llm.InterruptCancel, wantFastTurns: 1},
		{name: "steer bypasses fast classifier", delivery: interruptDeliverySteer, wantAction: llm.InterruptSteer, wantQueued: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := llm.NewEngine(llm.NewMockProvider("engine"), nil)
			fastProvider := llm.NewMockProvider("classifier").AddTextResponse("cancel")
			rt := &serveRuntime{engine: engine}
			state := &runtimeInterruptState{done: make(chan struct{}), cancel: func() {}}
			rt.setActiveInterrupt(state)
			defer rt.clearActiveInterrupt(state)

			action, replayed, err := rt.InterruptMessage(
				context.Background(),
				llm.UserText("change course"),
				"change course",
				"delivery-direct-"+string(tt.delivery),
				fastProvider,
				tt.delivery,
			)
			if err != nil {
				t.Fatalf("InterruptMessage: %v", err)
			}
			if replayed {
				t.Fatal("first dispatch unexpectedly replayed")
			}
			if action != tt.wantAction {
				t.Fatalf("action = %v, want %v", action, tt.wantAction)
			}
			if got := fastProvider.CurrentTurn(); got != tt.wantFastTurns {
				t.Fatalf("fast provider turns = %d, want %d", got, tt.wantFastTurns)
			}
			if pending := engine.ListPendingSteering(); len(pending) != tt.wantQueued {
				t.Fatalf("pending = %#v, want %d", pending, tt.wantQueued)
			}
		})
	}
}

func TestInterruptMessage_DeduplicatesConcurrentRetry(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("engine"), nil)
	fastProvider := llm.NewMockProvider("classifier")
	fastProvider.AddTurn(llm.MockTurn{Text: "steer", Delay: 100 * time.Millisecond})
	rt := &serveRuntime{engine: engine}
	state := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)

	type result struct {
		action   llm.InterruptAction
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	call := func() {
		action, replayed, err := rt.InterruptMessage(
			context.Background(),
			llm.UserText("please incorporate this"),
			"please incorporate this",
			"web-concurrent-1",
			fastProvider,
			interruptDeliveryAuto,
		)
		results <- result{action: action, replayed: replayed, err: err}
	}

	go call()
	deadline := time.Now().Add(time.Second)
	for fastProvider.CurrentTurn() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fastProvider.CurrentTurn() != 1 {
		t.Fatal("first interrupt never reached classifier")
	}
	go call()

	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("interrupt errors = %v, %v", first.err, second.err)
	}
	if first.action != llm.InterruptSteer || second.action != llm.InterruptSteer {
		t.Fatalf("actions = %v, %v, want steer", first.action, second.action)
	}
	if first.replayed == second.replayed {
		t.Fatalf("replayed flags = %v, %v, want exactly one replay", first.replayed, second.replayed)
	}
	if got := fastProvider.CurrentTurn(); got != 1 {
		t.Fatalf("classifier calls = %d, want 1", got)
	}
	if pending := engine.ListPendingSteering(); len(pending) != 1 {
		t.Fatalf("pending steering = %d, want 1", len(pending))
	}
}
