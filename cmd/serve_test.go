package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

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

func TestResolveSettingsWithPlatform_WebForServe(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettingsWithPlatform(cfg, nil, CLIFlags{SystemMessage: "serve={{platform}}"}, "", "", "", 0, 20, agents.TemplatePlatformWeb)
	if err != nil {
		t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
	}
	if settings.SystemPrompt != "serve=web" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "serve=web")
	}
}

func TestResolveSettingsWithPlatform_TelegramForServeWiring(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettingsWithPlatform(cfg, nil, CLIFlags{SystemMessage: "serve={{platform}}"}, "", "", "", 0, 20, agents.TemplatePlatformTelegram)
	if err != nil {
		t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
	}
	if settings.SystemPrompt != "serve=telegram" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "serve=telegram")
	}
}

func TestParseResponsesInput_FunctionCallOutput(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"}
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
	if msgs[1].Role != llm.RoleTool {
		t.Fatalf("second role = %s, want tool", msgs[1].Role)
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.ID != "call_1" {
		t.Fatalf("missing tool result id")
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

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, true, wireSpawn)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if engine == nil {
		t.Fatalf("engine = nil")
	}
	if toolMgr == nil {
		t.Fatalf("toolMgr = nil")
	}
	if !toolMgr.ApprovalMgr.YoloMode {
		t.Fatalf("toolMgr.ApprovalMgr.YoloMode = false, want true")
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

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, false, wireSpawn)
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

func TestHandleResponses_GeneratesSessionIDHeaderWhenMissing(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr: manager,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Header().Get("x-session-id")); got == "" {
		t.Fatalf("x-session-id header missing")
	}
}

func TestResolveServeAuthMode(t *testing.T) {
	mode, err := resolveServeAuthMode(false, "bearer", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "bearer" {
		t.Fatalf("mode = %q, want bearer", mode)
	}

	mode, err = resolveServeAuthMode(false, "none", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	mode, err = resolveServeAuthMode(false, "bearer", true, true)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	if _, err := resolveServeAuthMode(true, "bearer", true, true); err == nil {
		t.Fatalf("expected conflict error when --auth and --allow-no-auth disagree")
	}

	if _, err := resolveServeAuthMode(true, "invalid", false, false); err == nil {
		t.Fatalf("expected invalid auth mode error")
	}
}
