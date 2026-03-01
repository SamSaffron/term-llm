package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

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

func TestParseResponsesInput_ImageContent(t *testing.T) {
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
	if msg.Parts[0].Type != llm.PartText {
		t.Fatalf("parts[0].type = %s, want text", msg.Parts[0].Type)
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
	// Verify abbreviatePath works when path is under home dir
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(msg.Parts[0].Text, home) {
		t.Fatalf("parts[0].text leaks absolute home path: %q", msg.Parts[0].Text)
	}
}

func TestParseResponsesInput_UnsupportedImageSavesToDisk(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	// image/svg+xml is not a supported LLM image type
	b64 := "PHN2Zz48L3N2Zz4=" // base64 of "<svg></svg>"
	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/svg+xml;base64,` + b64 + `","filename":"icon.svg"}
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
	if len(msg.Parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartText {
		t.Fatalf("parts[0].type = %s, want text (saved to disk)", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "icon.svg") {
		t.Fatalf("parts[0].text = %q, should mention icon.svg", msg.Parts[0].Text)
	}

	// Verify file on disk
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
	if string(got) != "<svg></svg>" {
		t.Fatalf("file content = %q, want %q", got, "<svg></svg>")
	}
}

func TestParseResponsesInput_InvalidBase64ReturnsError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

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

func TestServeAuthMiddleware_CookieFallback(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}}
	h := srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// No credentials → 401
	req := httptest.NewRequest(http.MethodGet, "/images/test.png", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}

	// Valid cookie → allowed
	req = httptest.NewRequest(http.MethodGet, "/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("valid cookie: status = %d, want 204", rr.Code)
	}

	// Wrong cookie → 401
	req = httptest.NewRequest(http.MethodGet, "/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "wrong"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", rr.Code)
	}

	// Bearer still works
	req = httptest.NewRequest(http.MethodGet, "/images/test.png", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer: status = %d, want 204", rr.Code)
	}

	// Cookie on POST → rejected (cookie fallback is GET-only)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on POST: status = %d, want 401", rr.Code)
	}

	// URL-encoded cookie → decoded and accepted
	srv2 := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "se+cret/val="}}
	h2 := srv2.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req = httptest.NewRequest(http.MethodGet, "/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "se%2Bcret%2Fval%3D"})
	rr = httptest.NewRecorder()
	h2(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("url-encoded cookie: status = %d, want 204", rr.Code)
	}
}

func TestHandleImage_ServesFileAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cat.png"), []byte("fake-png"), 0644); err != nil {
		t.Fatalf("write test image: %v", err)
	}

	srv := &serveServer{cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = dir

	// Valid file
	req := httptest.NewRequest(http.MethodGet, "/images/cat.png", nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "fake-png" {
		t.Fatalf("body = %q, want %q", got, "fake-png")
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want 'private'", cc)
	}
	if vary := rr.Header().Get("Vary"); vary == "" {
		t.Fatalf("missing Vary header")
	}

	// Path traversal with ..
	req = httptest.NewRequest(http.MethodGet, "/images/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("traversal: status = %d, want 404", rr.Code)
	}

	// Empty filename
	req = httptest.NewRequest(http.MethodGet, "/images/", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty: status = %d, want 404", rr.Code)
	}

	// Nonexistent file
	req = httptest.NewRequest(http.MethodGet, "/images/nope.png", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing: status = %d, want 404", rr.Code)
	}
}

func TestHandleSessions_ListsFromStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "test-session-1",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Summary:   "hello world",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Sessions []struct {
			ID           string `json:"id"`
			Summary      string `json:"summary"`
			CreatedAt    int64  `json:"created_at"`
			MessageCount int    `json:"message_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(body.Sessions))
	}
	if body.Sessions[0].ID != "test-session-1" {
		t.Fatalf("id = %q, want %q", body.Sessions[0].ID, "test-session-1")
	}
	if body.Sessions[0].Summary != "hello world" {
		t.Fatalf("summary = %q, want %q", body.Sessions[0].Summary, "hello world")
	}
}

func TestHandleSessionMessages_ReturnsStructuredParts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-parts",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Message with text + multiple tool calls (the case that was lossy before)
	msg := session.NewMessage("sess-parts", llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "Let me search for that"},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "web_search", Arguments: json.RawMessage(`{"query":"go"}`)}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-2", Name: "read_url", Arguments: json.RawMessage(`{"url":"https://go.dev"}`)}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-parts", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-parts/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				ToolName   string `json:"tool_name"`
				ToolArgs   string `json:"tool_arguments"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", m.Role)
	}
	if len(m.Parts) != 3 {
		t.Fatalf("parts count = %d, want 3", len(m.Parts))
	}

	// Text part
	if m.Parts[0].Type != "text" || m.Parts[0].Text != "Let me search for that" {
		t.Fatalf("part[0] = %+v, want text part", m.Parts[0])
	}
	// First tool call
	if m.Parts[1].Type != "tool_call" || m.Parts[1].ToolName != "web_search" || m.Parts[1].ToolCallID != "call-1" {
		t.Fatalf("part[1] = %+v, want web_search tool_call", m.Parts[1])
	}
	if m.Parts[1].ToolArgs != `{"query":"go"}` {
		t.Fatalf("part[1].args = %q", m.Parts[1].ToolArgs)
	}
	// Second tool call (was lost before due to break)
	if m.Parts[2].Type != "tool_call" || m.Parts[2].ToolName != "read_url" || m.Parts[2].ToolCallID != "call-2" {
		t.Fatalf("part[2] = %+v, want read_url tool_call", m.Parts[2])
	}
}

func TestHandleSessionMessages_OmitsToolResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-tr", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := session.NewMessage("sess-tr", llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "result msg"},
			{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Content: "verbose tool output"}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-tr", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-tr/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Parts []struct{ Type string } `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	for _, p := range body.Messages[0].Parts {
		if p.Type == "tool_result" {
			t.Fatal("tool_result parts should be omitted from API response")
		}
	}
	if len(body.Messages[0].Parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (text only)", len(body.Messages[0].Parts))
	}
}

func TestEnsurePersistedSession_RestoresHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "restore-test",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msgs := []session.Message{
		*session.NewMessage("restore-test", llm.UserText("hello"), -1),
		*session.NewMessage("restore-test", llm.Message{
			Role:  llm.RoleAssistant,
			Parts: []llm.Part{{Type: llm.PartText, Text: "hi there"}},
		}, -1),
	}
	if err := store.ReplaceMessages(ctx, "restore-test", msgs); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Simulate a fresh runtime with empty history
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
	}

	ok := rt.ensurePersistedSession(ctx, "restore-test", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleUser {
		t.Fatalf("history[0].role = %s, want user", rt.history[0].Role)
	}
	if rt.history[1].Role != llm.RoleAssistant {
		t.Fatalf("history[1].role = %s, want assistant", rt.history[1].Role)
	}
	if rt.history[1].Parts[0].Text != "hi there" {
		t.Fatalf("history[1].text = %q, want %q", rt.history[1].Parts[0].Text, "hi there")
	}
}

func TestEnsurePersistedSession_SkipsRestoreWhenHistoryExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "skip-restore",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dbMsg := session.NewMessage("skip-restore", llm.UserText("db message"), -1)
	if err := store.ReplaceMessages(ctx, "skip-restore", []session.Message{*dbMsg}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Runtime already has history — should NOT overwrite
	existing := []llm.Message{llm.UserText("existing")}
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
		history:      existing,
	}

	ok := rt.ensurePersistedSession(ctx, "skip-restore", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 1 {
		t.Fatalf("history len = %d, want 1 (unchanged)", len(rt.history))
	}
	if rt.history[0].Parts[0].Text != "existing" {
		t.Fatalf("history was overwritten")
	}
}
