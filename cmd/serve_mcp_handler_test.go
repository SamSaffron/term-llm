package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	internalmcp "github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
)

const runServeMCPHandlerTestServerEnv = "TERM_LLM_SERVE_MCP_HANDLER_TEST_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(runServeMCPHandlerTestServerEnv) != "" {
		runServeMCPHandlerTestServer()
		os.Exit(0)
	}
	dataHome, err := os.MkdirTemp("", "term-llm-cmd-test-data-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dataHome)
	os.Exit(code)
}

type serveMCPHandlerGreetingParams struct {
	Name string `json:"name"`
}

func runServeMCPHandlerTestServer() {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "serve-mcp-handler-test", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *sdkmcp.CallToolRequest, args serveMCPHandlerGreetingParams) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hi " + args.Name}},
		}, nil, nil
	})
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func writeServeMCPConfig(t *testing.T, servers map[string]internalmcp.ServerConfig) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "term-llm", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.Marshal(internalmcp.Config{Servers: servers})
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
}

func newServeMCPTestStore(t *testing.T) session.Store {
	t.Helper()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type serveMCPFakeTool struct {
	name string
}

func (t serveMCPFakeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: t.name, Description: "fake"}
}

func (t serveMCPFakeTool) Preview(args json.RawMessage) string { return "" }

func (t serveMCPFakeTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("ok"), nil
}

func newServeMCPHandlerTestServer(t *testing.T, store session.Store) (*serveServer, **serveRuntime) {
	t.Helper()
	var created *serveRuntime
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		engine := llm.NewEngine(provider, llm.NewToolRegistry())
		created = &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
			store:        store,
			yoloMode:     true,
		}
		created.Touch()
		return created, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	t.Cleanup(mgr.Close)
	return &serveServer{sessionMgr: mgr, store: store}, &created
}

func decodeServeMCPResponse(t *testing.T, rr *httptest.ResponseRecorder) serveMCPSessionResponse {
	t.Helper()
	var resp serveMCPSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return resp
}

func TestHandleSessionMCPRejectsSideMutationAndNeutralizesLegacySelection(t *testing.T) {
	store := newServeMCPTestStore(t)
	parent := &session.Session{ID: "parent", Kind: session.KindRoot, Status: session.StatusActive}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	side := &session.Session{ID: "side_mcp", Kind: session.KindSide, SideState: session.SideOpen, ParentID: parent.ID, RootID: parent.ID, MCP: "legacy", Status: session.StatusActive}
	if err := store.Create(context.Background(), side); err != nil {
		t.Fatal(err)
	}
	srv, createdPtr := newServeMCPHandlerTestServer(t, store)

	stateReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/side_mcp/state", nil)
	stateRR := httptest.NewRecorder()
	srv.handleSessionByID(stateRR, stateReq)
	if stateRR.Code != http.StatusOK || strings.Contains(stateRR.Body.String(), "legacy") {
		t.Fatalf("side state leaked persisted MCP: status=%d body=%s", stateRR.Code, stateRR.Body.String())
	}

	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/side_mcp/mcp", strings.NewReader(`{"enabled":["anything"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PATCH status = %d body=%s", rr.Code, rr.Body.String())
	}
	if rt := *createdPtr; rt == nil || rt.mcpSetting != "" {
		t.Fatalf("side runtime MCP selection was not clamped: %#v", rt)
	}
	stored, err := store.Get(context.Background(), side.ID)
	if err != nil || stored.MCP != "" {
		t.Fatalf("legacy side MCP was not neutralized: session=%#v err=%v", stored, err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/side_mcp/mcp", nil)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK || len(decodeServeMCPResponse(t, rr).Enabled) != 0 {
		t.Fatalf("GET exposed side MCP: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleSessionMCPGetListsConfiguredServers(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"filesystem": {Command: "term-llm-test-filesystem"},
	})
	srv, _ := newServeMCPHandlerTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess_get/mcp", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeServeMCPResponse(t, rr)
	if len(resp.Servers) != 1 {
		t.Fatalf("servers = %#v, want one", resp.Servers)
	}
	server := resp.Servers[0]
	if server.Name != "filesystem" || !server.Configured || server.Enabled || server.Status != string(internalmcp.StatusStopped) || server.Tools != 0 {
		t.Fatalf("server = %#v, want configured stopped filesystem", server)
	}
	if len(resp.Enabled) != 0 {
		t.Fatalf("enabled = %#v, want empty", resp.Enabled)
	}
}

func TestHandleSessionMCPPatchUnknownServerReturnsBadRequest(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"known": {Command: "term-llm-test-known"},
	})
	srv, _ := newServeMCPHandlerTestServer(t, nil)

	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess_unknown/mcp", strings.NewReader(`{"enabled":["missing"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing") || !strings.Contains(rr.Body.String(), "not configured") {
		t.Fatalf("body = %s, want clear missing-server error", rr.Body.String())
	}
}

func TestHandleSessionMCPPatchEnablesServerAndRegistersTools(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"greeter": {
			Command: os.Args[0],
			Env: map[string]string{
				runServeMCPHandlerTestServerEnv: "1",
			},
		},
	})
	store := newServeMCPTestStore(t)
	srv, createdPtr := newServeMCPHandlerTestServer(t, store)

	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess_enable/mcp", strings.NewReader(`{"enabled":["greeter"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeServeMCPResponse(t, rr)
	if len(resp.Enabled) != 1 || resp.Enabled[0] != "greeter" {
		t.Fatalf("enabled = %#v, want greeter", resp.Enabled)
	}
	if len(resp.Servers) != 1 || !resp.Servers[0].Enabled || resp.Servers[0].Status != string(internalmcp.StatusReady) || resp.Servers[0].Tools != 1 {
		t.Fatalf("servers = %#v, want ready greeter with one tool", resp.Servers)
	}
	rt := *createdPtr
	if rt == nil {
		t.Fatal("runtime was not created")
	}
	if _, ok := rt.engine.Tools().Get("greeter__greet"); !ok {
		t.Fatal("expected greeter__greet tool to be registered")
	}
	if rt.mcpSetting != "greeter" {
		t.Fatalf("mcpSetting = %q, want greeter", rt.mcpSetting)
	}
	stored, err := store.Get(context.Background(), "sess_enable")
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored == nil || stored.MCP != "greeter" {
		t.Fatalf("stored MCP = %#v, want greeter", stored)
	}
}

func TestRuntimeForRequestRestoresPersistedMCPSelection(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"greeter": {
			Command: os.Args[0],
			Env: map[string]string{
				runServeMCPHandlerTestServerEnv: "1",
			},
		},
	})
	store := newServeMCPTestStore(t)
	if err := store.Create(context.Background(), &session.Session{
		ID:        "sess_resume_mcp",
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "greeter",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, store)

	rt, stateful, err := srv.runtimeForRequest(context.Background(), "sess_resume_mcp")
	if err != nil {
		t.Fatalf("runtimeForRequest: %v", err)
	}
	if !stateful {
		t.Fatal("runtimeForRequest stateful = false, want true")
	}
	if rt.mcpSetting != "greeter" {
		t.Fatalf("mcpSetting = %q, want greeter", rt.mcpSetting)
	}
	if _, ok := rt.engine.Tools().Get("greeter__greet"); !ok {
		t.Fatal("expected greeter__greet to be registered from persisted session MCP")
	}
}

func TestRuntimeForRequestContinuesWhenPersistedMCPFailsToStart(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"broken": {Command: "term-llm-test-missing-mcp-command"},
	})
	store := newServeMCPTestStore(t)
	if err := store.Create(context.Background(), &session.Session{
		ID:        "sess_broken_mcp_run",
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "broken",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, store)

	rt, stateful, err := srv.runtimeForRequest(context.Background(), "sess_broken_mcp_run")
	if err != nil {
		t.Fatalf("runtimeForRequest returned error for failed MCP restore: %v", err)
	}
	if !stateful {
		t.Fatal("runtimeForRequest stateful = false, want true")
	}
	if rt.mcpSetting != "broken" {
		t.Fatalf("mcpSetting = %q, want persisted broken server", rt.mcpSetting)
	}
}

func TestHandleSessionMCPGetReturnsStateWhenPersistedServerFails(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"broken": {Command: "term-llm-test-missing-mcp-command"},
	})
	store := newServeMCPTestStore(t)
	if err := store.Create(context.Background(), &session.Session{
		ID:        "sess_broken_mcp_modal",
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "broken",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, store)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess_broken_mcp_modal/mcp", nil)
	getRR := httptest.NewRecorder()
	srv.handleSessionByID(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", getRR.Code, getRR.Body.String())
	}
	resp := decodeServeMCPResponse(t, getRR)
	if len(resp.Enabled) != 1 || resp.Enabled[0] != "broken" {
		t.Fatalf("enabled = %#v, want persisted broken server", resp.Enabled)
	}
	if len(resp.Servers) != 1 || resp.Servers[0].Name != "broken" || !resp.Servers[0].Enabled || resp.Servers[0].Status != string(internalmcp.StatusFailed) || resp.Servers[0].Error == "" {
		t.Fatalf("servers = %#v, want enabled failed broken server", resp.Servers)
	}

	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess_broken_mcp_modal/mcp", strings.NewReader(`{"enabled":[]}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchRR := httptest.NewRecorder()
	srv.handleSessionByID(patchRR, patchReq)
	if patchRR.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchRR.Code, patchRR.Body.String())
	}
	stored, err := store.Get(context.Background(), "sess_broken_mcp_modal")
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored == nil || stored.MCP != "" {
		t.Fatalf("stored MCP = %#v, want empty after disabling failed server", stored)
	}
}

func TestHandleSessionStateReturnsPersistedMCPWhenRuntimeMissing(t *testing.T) {
	store := newServeMCPTestStore(t)
	if err := store.Create(context.Background(), &session.Session{
		ID:        "sess_state_mcp",
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "greeter,filesystem",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess_state_mcp/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	enabled, ok := resp["mcp_enabled"].([]any)
	if !ok || len(enabled) != 2 || enabled[0] != "greeter" || enabled[1] != "filesystem" {
		t.Fatalf("mcp_enabled = %#v, want persisted servers", resp["mcp_enabled"])
	}
}

func TestHandleSessionMCPPatchDisablesAndPersists(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{
		"filesystem": {Command: "term-llm-test-filesystem"},
	})
	store := newServeMCPTestStore(t)
	if err := store.Create(context.Background(), &session.Session{
		ID:        "sess_disable",
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "filesystem",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv, createdPtr := newServeMCPHandlerTestServer(t, store)

	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "sess_disable")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	rt.engine.Tools().Register(serveMCPFakeTool{name: "filesystem__read"})
	rt.mcpSetting = "filesystem"

	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess_disable/mcp", strings.NewReader(`{"enabled":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	rt = *createdPtr
	if _, ok := rt.engine.Tools().Get("filesystem__read"); ok {
		t.Fatal("filesystem__read tool remained registered after disabling filesystem")
	}
	if rt.mcpSetting != "" {
		t.Fatalf("mcpSetting = %q, want empty", rt.mcpSetting)
	}
	stored, err := store.Get(context.Background(), "sess_disable")
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored == nil || stored.MCP != "" {
		t.Fatalf("stored MCP = %#v, want empty", stored)
	}
}

func TestHandleSessionMCPPatchDoesNotSkipPersistedHistoryLoad(t *testing.T) {
	writeServeMCPConfig(t, map[string]internalmcp.ServerConfig{})
	store := newServeMCPTestStore(t)
	const sessionID = "sess_mcp_patch_history"
	if err := store.Create(context.Background(), &session.Session{
		ID:        sessionID,
		Provider:  "mock",
		Model:     "mock-model",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MCP:       "filesystem",
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("old question"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("old answer"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("new answer")
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       llm.NewEngine(provider, llm.NewToolRegistry()),
			defaultModel: "mock-model",
			store:        store,
		}
		rt.Touch()
		return rt, nil
	})
	defer mgr.Close()
	srv := &serveServer{sessionMgr: mgr, store: store}

	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+sessionID+"/mcp", strings.NewReader(`{"enabled":[]}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchRR := httptest.NewRecorder()
	srv.handleSessionByID(patchRR, patchReq)
	if patchRR.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchRR.Code, patchRR.Body.String())
	}

	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("seed GetMessages: %v", err)
	}
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)
	body := `{"input":"new question","previous_response_id":"` + previousID + `"}`
	respReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	respReq.Header.Set("session_id", sessionID)
	respReq.Header.Set("Content-Type", "application/json")
	respRR := httptest.NewRecorder()
	srv.handleResponses(respRR, respReq)
	if respRR.Code != http.StatusOK {
		t.Fatalf("response status = %d body=%s", respRR.Code, respRR.Body.String())
	}

	msgs, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("stored message count = %d, want 4 appended messages", len(msgs))
	}
	want := []struct {
		role llm.Role
		text string
	}{
		{llm.RoleUser, "old question"},
		{llm.RoleAssistant, "old answer"},
		{llm.RoleUser, "new question"},
		{llm.RoleAssistant, "new answer"},
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].TextContent != w.text {
			t.Fatalf("message[%d] = (%s, %q), want (%s, %q)", i, msgs[i].Role, msgs[i].TextContent, w.role, w.text)
		}
	}
	if len(provider.Requests) != 1 || len(provider.Requests[0].Messages) != 3 {
		t.Fatalf("provider requests = %#v, want one request with persisted history", provider.Requests)
	}
}
