package tooldiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalmcp "github.com/samsaffron/term-llm/internal/mcp"
)

const discoveryTestServerEnv = "TERM_LLM_TOOL_DISCOVERY_TEST_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(discoveryTestServerEnv) != "" {
		runDiscoveryTestServer()
		return
	}
	os.Exit(m.Run())
}

func runDiscoveryTestServer() {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "discovery-test", Version: "1"}, &sdkmcp.ServerOptions{PageSize: 4})
	for i := 0; i < 24; i++ {
		name := fmt.Sprintf("realistic_operation_%02d", i)
		description := fmt.Sprintf("Inspect deterministic project record %d with filters and identifiers", i)
		sdkmcp.AddTool(server, &sdkmcp.Tool{Name: name, Description: description}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
			data, _ := json.Marshal(args)
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(data)}}}, nil, nil
		})
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "special_action",
		Description: "Perform the special deterministic action for a selected project",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"project_id": map[string]any{"type": "string"}}, "required": []string{"project_id"}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: fmt.Sprintf("special complete for %v", args["project_id"])}}}, nil, nil
	})
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func startDiscoveryTestManager(t *testing.T) *internalmcp.Manager {
	t.Helper()
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{
		"federation": {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}},
	}})
	if err := manager.Enable(context.Background(), "federation"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := manager.ServerStatus("federation")
		if status == internalmcp.StatusReady {
			if err != nil {
				t.Fatalf("ready with error: %v", err)
			}
			return manager
		}
		if status == internalmcp.StatusFailed {
			t.Fatalf("server failed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for test MCP server")
	return nil
}

func drainStream(t *testing.T, stream llm.Stream) {
	t.Helper()
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	}
}

func toolNames(specs []llm.ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	return names
}

func TestAlwaysLoadPinsOnlyAuthorisedTool(t *testing.T) {
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{
		"pinned": {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}, AlwaysLoad: []string{"special_action", "missing_tool"}},
	}})
	defer manager.StopAll()
	if err := manager.Enable(context.Background(), "pinned"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, statusErr := manager.ServerStatus("pinned")
		if status == internalmcp.StatusReady {
			break
		}
		if status == internalmcp.StatusFailed {
			t.Fatalf("server failed: %v", statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	provider := llm.NewMockProvider("pinned").AddTextResponse("done")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "pinned", Messages: []llm.Message{llm.UserText("hi")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if got := toolNames(provider.RecordedRequests()[0].Tools); len(got) != 1 || got[0] != "pinned__special_action" {
		t.Fatalf("always_load surface = %v", got)
	}
}

func toolResultContent(messages []llm.Message, toolName string) string {
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartToolResult && part.ToolResult != nil && part.ToolResult.Name == toolName {
				return part.ToolResult.Content
			}
		}
	}
	return ""
}

func TestAmbiguousOriginalNameDoesNotActivate(t *testing.T) {
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{
		"alpha": {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}},
		"beta":  {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}},
	}})
	defer manager.StopAll()
	for _, name := range []string{"alpha", "beta"} {
		if err := manager.Enable(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ready := 0
		for _, name := range []string{"alpha", "beta"} {
			status, err := manager.ServerStatus(name)
			if status == internalmcp.StatusFailed {
				t.Fatalf("%s failed: %v", name, err)
			}
			if status == internalmcp.StatusReady {
				ready++
			}
		}
		if ready == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	provider := llm.NewMockProvider("ambiguous")
	provider.AddToolCall("search", ToolSearchName, map[string]any{"tool_names": []string{"special_action"}})
	provider.AddTextResponse("resolved ambiguity")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "ambiguous", Messages: []llm.Message{llm.UserText("load special_action")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2: %#v", len(requests), requests)
	}
	result := toolResultContent(requests[1].Messages, ToolSearchName)
	if !strings.Contains(result, "ambiguous") || !strings.Contains(result, "alpha__special_action") || !strings.Contains(result, "beta__special_action") {
		t.Fatalf("ambiguous search result was not actionable: %q", result)
	}
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("ambiguous")
	if diagnostics.DynamicActive != 0 {
		t.Fatalf("ambiguous original activated a candidate: %+v", diagnostics)
	}
}

func TestDynamicActivationBudgetIsDeterministic(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewMockProvider("budget")
	first, second := make([]string, 8), make([]string, 8)
	for i := 0; i < 8; i++ {
		first[i] = fmt.Sprintf("realistic_operation_%02d", i)
		second[i] = fmt.Sprintf("realistic_operation_%02d", i+8)
	}
	provider.AddToolCall("search-1", ToolSearchName, map[string]any{"tool_names": first})
	provider.AddToolCall("search-2", ToolSearchName, map[string]any{"tool_names": second})
	provider.AddToolCall("search-3", ToolSearchName, map[string]any{"tool_names": []string{"special_action"}})
	provider.AddTextResponse("budget handled")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "budget", Messages: []llm.Message{llm.UserText("load many")}, MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("budget")
	if diagnostics.DynamicActive != MaxActiveDeferred || diagnostics.DeferredCount != 9 {
		t.Fatalf("budget diagnostics = %+v", diagnostics)
	}
	for _, request := range provider.RecordedRequests() {
		if hasTool(request.Tools, "federation__special_action") {
			t.Fatal("tool activated after dynamic budget was exhausted")
		}
	}
}

func TestSampleDeferredToolNamesUsesBoundedDeterministicSpread(t *testing.T) {
	names := make([]string, 75)
	for i := range names {
		names[i] = fmt.Sprintf("tool_%02d", 74-i)
	}
	got := sampleDeferredToolNames(names, 50)
	if len(got) != 50 {
		t.Fatalf("sample size = %d, want 50", len(got))
	}
	if got[0] != "tool_00" || got[len(got)-1] != "tool_74" {
		t.Fatalf("sample did not span sorted catalogue: first=%q last=%q", got[0], got[len(got)-1])
	}
	if one := sampleDeferredToolNames(names, 1); len(one) != 1 || one[0] != "tool_00" {
		t.Fatalf("single sample = %#v, want first sorted name", one)
	}
}

func TestDeferredSurfaceAddsDeveloperToolSearchInstructionOnce(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewMockProvider("deferred-instruction")
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "portable"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{SessionID: "instruction", Messages: []llm.Message{llm.UserText("list the workflows on my site")}, MaxTurns: 4}
	if _, err := planner.BeginRun(context.Background(), provider, &req, "instruction-run"); err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("instruction-run")
	if _, err := planner.PrepareTurn(context.Background(), provider, &req, "instruction-run", 0, 4); err != nil {
		t.Fatal(err)
	}

	count := 0
	developerBeforeUser := false
	for i, message := range req.Messages {
		text := llm.MessageText(message)
		if strings.Contains(text, deferredToolSearchInstructionMarker) {
			count++
			if message.Role != llm.RoleDeveloper {
				t.Fatalf("tool-search instruction role = %q, want developer", message.Role)
			}
			if !strings.Contains(text, "MUST call tool_search") || !strings.Contains(text, "before saying the capability is unavailable") {
				t.Fatalf("tool-search instruction is too weak: %q", text)
			}
			for _, want := range []string{"User loaded these MCP servers", "federation: 25 tools", "Tools provided by discovery-test.", "Example tool names:", "realistic_operation_00", "special_action"} {
				if !strings.Contains(text, want) {
					t.Fatalf("tool-search instruction missing server context %q: %q", want, text)
				}
			}
			developerBeforeUser = i+1 < len(req.Messages) && req.Messages[i+1].Role == llm.RoleUser
		}
	}
	if count != 1 {
		t.Fatalf("tool-search developer instruction count = %d, want 1; messages=%#v", count, req.Messages)
	}
	if !developerBeforeUser {
		t.Fatalf("tool-search developer instruction was not inserted before the latest user message: %#v", req.Messages)
	}
	if specs := engine.ToolDiscoveryActiveSpecs("instruction"); len(specs) != 0 {
		t.Fatalf("unloaded deferred tools appeared active in inspector surface: %v", toolNames(specs))
	}

	updatedSnapshot := *manager.CatalogueSnapshot()
	updatedSnapshot.Tools = append(append([]internalmcp.CatalogTool(nil), updatedSnapshot.Tools...), internalmcp.CatalogTool{
		Server: "browser", Name: "browser__open", NamespaceDescription: "Browser automation and page inspection.",
	})
	updatedServers := []llm.ToolDiscoveryServerDiagnostic{
		{Name: "browser", Total: 1, Deferred: 1},
		{Name: "federation", Total: 25, Deferred: 25},
	}
	ensureDeferredToolSearchInstruction(&req, &updatedSnapshot, updatedServers)
	count = 0
	for _, message := range req.Messages {
		text := llm.MessageText(message)
		if strings.Contains(text, deferredToolSearchInstructionMarker) {
			count++
			for _, want := range []string{"browser: 1 tool", "Browser automation and page inspection.", "Example tool names: browser__open", "federation: 25 tools"} {
				if !strings.Contains(text, want) {
					t.Fatalf("refreshed instruction missing newly loaded MCP context %q: %q", want, text)
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("refreshed tool-search developer instruction count = %d, want 1", count)
	}
}

func TestGenericProviderDeferredDiscoveryAndSessionActivation(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	if got := len(manager.CatalogueSnapshot().Tools); got != 25 {
		t.Fatalf("paginated catalogue count = %d, want 25", got)
	}

	provider := llm.NewMockProvider("generic")
	provider.AddToolCall("search-1", ToolSearchName, map[string]any{"tool_names": []string{"special_action"}})
	provider.AddToolCall("external-1", "federation__special_action", map[string]any{"project_id": "P-42"})
	provider.AddTextResponse("done")
	registry := llm.NewToolRegistry()
	engine := llm.NewEngine(provider, registry)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "auto", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	if specs := registry.AllSpecs(); len(specs) != 0 {
		t.Fatalf("deferred wrappers leaked through AllSpecs: %v", toolNames(specs))
	}
	if specs := registry.AllSpecsIncludingDeferred(); len(specs) != 25 {
		t.Fatalf("executable catalogue size = %d, want 25", len(specs))
	}

	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true,
		SessionID: "session-a", Messages: []llm.Message{llm.UserText("perform the special action")}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	requests := provider.RecordedRequests()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(requests))
	}
	if got := toolNames(requests[0].Tools); len(got) != 1 || got[0] != ToolSearchName {
		t.Fatalf("initial tools = %v, want tool_search only", got)
	}
	if got := toolNames(requests[1].Tools); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("post-search tools = %v, want activated schema without late tool_search", got)
	}
	if got := toolNames(requests[2].Tools); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("final-turn tools = %v, want activated schema without tool_search", got)
	}
	if strings.Contains(llm.MessageText(requests[1].Messages[len(requests[1].Messages)-1]), "input_schema") {
		t.Fatal("tool_search result dumped a full schema")
	}
	diagnostics, ok := engine.ToolDiscoveryDiagnostics("session-a")
	if !ok || diagnostics.ResolvedMode != "deferred" || diagnostics.DynamicActive != 1 || diagnostics.DeferredCount != 24 {
		t.Fatalf("diagnostics = %+v, ok=%v", diagnostics, ok)
	}
	activeSpecs := engine.ToolDiscoveryActiveSpecs("session-a")
	if got := toolNames(activeSpecs); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("active inspector surface = %v, want loaded MCP schema only", got)
	}

	provider.ResetTurns()
	provider.Reset()
	provider.AddTextResponse("still active")
	stream, err = engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "session-a", Messages: []llm.Message{llm.UserText("continue")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	last := provider.RecordedRequests()[0]
	if got := toolNames(last.Tools); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("next-user-turn tools = %v, activation did not persist", got)
	}

	provider.ResetTurns()
	provider.Reset()
	provider.AddTextResponse("isolated")
	stream, err = engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "session-b", Messages: []llm.Message{llm.UserText("unrelated")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if got := toolNames(provider.RecordedRequests()[0].Tools); len(got) != 0 {
		t.Fatalf("activation leaked to another session: %v", got)
	}
}

type resetCountingProvider struct {
	*llm.MockProvider
	resets int
}

func (p *resetCountingProvider) ResetConversation() { p.resets++ }

func TestPolicyTighteningResetsProviderConversation(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &resetCountingProvider{MockProvider: llm.NewMockProvider("stateful")}
	provider.AddTextResponse("first")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "eager", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "policy", Messages: []llm.Message{llm.UserText("first")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if provider.resets != 0 {
		t.Fatalf("initial additive surface reset provider %d times", provider.resets)
	}

	engine.SetAllowedToolsFilter([]string{"federation__special_action"})
	provider.ResetTurns()
	provider.Reset()
	provider.AddTextResponse("second")
	stream, err = engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "policy", Messages: []llm.Message{llm.UserText("second")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if provider.resets != 1 {
		t.Fatalf("policy tightening resets = %d, want 1", provider.resets)
	}
	if got := toolNames(provider.RecordedRequests()[0].Tools); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("tightened surface = %v", got)
	}
}

type bridgeMockProvider struct{ *llm.MockProvider }

func (p *bridgeMockProvider) Credential() string { return "cursor-bin" }

func TestFixedCLIBridgeResolvesEager(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &bridgeMockProvider{MockProvider: llm.NewMockProvider("Cursor CLI test")}
	provider.AddTextResponse("done")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Threshold: 0}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "bridge", Messages: []llm.Message{llm.UserText("hi")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	request := provider.RecordedRequests()[0]
	if got := len(request.Tools); got != 25 {
		t.Fatalf("bridge tools = %d, want complete eager catalogue", got)
	}
	if hasTool(request.Tools, ToolSearchName) {
		t.Fatal("fixed bridge received generic dynamic tool_search")
	}
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("bridge")
	if diagnostics.Strategy != string(StrategyDelegated) || diagnostics.ResolvedMode != string(ModeEager) {
		t.Fatalf("bridge diagnostics = %+v", diagnostics)
	}
}

func TestAutoModeDefersFormerlyEagerToolsAfterCatalogueGrowth(t *testing.T) {
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{
		"alpha": {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}},
		"beta":  {Command: os.Args[0], Env: map[string]string{discoveryTestServerEnv: "1"}},
	}})
	defer manager.StopAll()
	if err := manager.Enable(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	waitReady := func(name string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			status, err := manager.ServerStatus(name)
			if status == internalmcp.StatusReady {
				return
			}
			if status == internalmcp.StatusFailed {
				t.Fatalf("%s failed: %v", name, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", name)
	}
	waitReady("alpha")

	provider := llm.NewMockProvider("growth").AddTextResponse("eager").AddTextResponse("deferred")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "auto", Threshold: 25}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "growth", Messages: []llm.Message{llm.UserText("first")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if got := len(provider.RecordedRequests()[0].Tools); got != 25 {
		t.Fatalf("initial eager surface = %d tools, want 25", got)
	}

	if err := manager.Enable(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	waitReady("beta")
	stream, err = engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "growth", Messages: []llm.Message{llm.UserText("second")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	requests := provider.RecordedRequests()
	if got := toolNames(requests[len(requests)-1].Tools); len(got) != 1 || got[0] != ToolSearchName {
		t.Fatalf("grown deferred surface retained formerly eager schemas: %v", got)
	}
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("growth")
	if diagnostics.ResolvedMode != string(ModeDeferred) || diagnostics.PinnedCount != 0 || diagnostics.DeferredCount != 50 {
		t.Fatalf("growth diagnostics = %+v", diagnostics)
	}
}

func TestAutoEagerAtThresholdAndForcedChoicePinning(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewMockProvider("generic").AddTextResponse("done")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "auto", Threshold: 25}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "eager", Messages: []llm.Message{llm.UserText("hi")}, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	request := provider.RecordedRequests()[0]
	if got := len(request.Tools); got != 25 {
		t.Fatalf("at-threshold eager tools = %d, want 25", got)
	}
	for _, message := range request.Messages {
		if strings.Contains(llm.MessageText(message), deferredToolSearchInstructionMarker) {
			t.Fatalf("eager surface received deferred MCP guidance: %#v", request.Messages)
		}
	}

	provider.ResetTurns()
	provider.Reset()
	provider.AddTextResponse("forced")
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	engine.ResetSessionState("forced")
	stream, err = engine.Stream(context.Background(), llm.Request{
		EnableToolDiscovery: true,
		SessionID:           "forced", Messages: []llm.Message{llm.UserText("force")}, MaxTurns: 1,
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceName, Name: "federation__special_action"},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if got := toolNames(provider.RecordedRequests()[0].Tools); len(got) != 1 || got[0] != "federation__special_action" {
		t.Fatalf("forced deferred tool was not pinned on the final request: %v", got)
	}
}

func TestForcedToolChoicePlannerBypassIsLimitedToAuthorisedDeferredMCP(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()

	tests := []struct {
		name        string
		forced      string
		allowed     []string
		wantErr     bool
		wantVisible string
	}{
		{name: "non MCP missing", forced: "not_a_registered_tool", wantErr: true},
		{name: "denied MCP", forced: "federation__special_action", allowed: []string{}, wantErr: true},
		{name: "authorised deferred MCP", forced: "federation__special_action", allowed: []string{"federation__special_action"}, wantVisible: "federation__special_action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := llm.NewMockProvider(tt.name).AddTextResponse("done")
			engine := llm.NewEngine(provider, nil)
			if tt.allowed != nil {
				engine.SetAllowedToolsFilter(tt.allowed)
			}
			if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine); err != nil {
				t.Fatal(err)
			}
			stream, err := engine.Stream(context.Background(), llm.Request{
				EnableToolDiscovery: true,
				SessionID:           "forced-" + strings.ReplaceAll(tt.name, " ", "-"),
				Messages:            []llm.Message{llm.UserText("force it")},
				ToolChoice:          llm.ToolChoice{Mode: llm.ToolChoiceName, Name: tt.forced},
				MaxTurns:            1,
			})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), tt.forced) {
					t.Fatalf("Stream() error = %v, want rejection naming %q", err, tt.forced)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			drainStream(t, stream)
			requests := provider.RecordedRequests()
			if len(requests) == 0 || !hasTool(requests[0].Tools, tt.wantVisible) {
				t.Fatalf("forced deferred tool was not visible: %#v", requests)
			}
		})
	}
}

type tighteningProvider struct {
	*llm.MockProvider
	tighten   func()
	tightened bool
}

func (p *tighteningProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if !p.tightened {
		p.tightened = true
		p.tighten()
	}
	return p.MockProvider.Stream(ctx, req)
}

func TestPlannerOwnedSearchDoesNotWeakenToolAllowlist(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &tighteningProvider{MockProvider: llm.NewMockProvider("planner-boundary")}
	provider.AddToolCall("search", ToolSearchName, map[string]any{"tool_names": []string{"special_action"}})
	provider.AddTextResponse("done")
	engine := llm.NewEngine(provider, nil)
	provider.tighten = func() { engine.SetAllowedToolsFilter([]string{}) }
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	if planner.AllowsPlannerTool("", ToolSearchName) || planner.AllowsPlannerTool("unknown", ToolSearchName) || planner.AllowsPlannerTool("unknown", "other") {
		t.Fatal("planner authorised a control tool outside its exact active run boundary")
	}
	stream, err := engine.Stream(context.Background(), llm.Request{
		EnableToolDiscovery: true,
		SessionID:           "planner-boundary",
		Messages:            []llm.Message{llm.UserText("perform the special action")},
		MaxTurns:            3,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if engine.IsToolAllowed(ToolSearchName) {
		t.Fatal("planner-owned execution weakened IsToolAllowed after policy tightening")
	}
	requests := provider.RecordedRequests()
	if len(requests) != 2 || !hasTool(requests[0].Tools, ToolSearchName) {
		t.Fatalf("planner-owned search flow = %#v", requests)
	}
	if result := toolResultContent(requests[1].Messages, ToolSearchName); !strings.Contains(result, "unavailable or denied") {
		t.Fatalf("tool_search was not narrowly executed under tightened policy: %q", result)
	}
}

func TestDiscoveryFinalTurnCutoff(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	for _, tt := range []struct {
		maxTurns   int
		wantSearch bool
	}{
		{maxTurns: 1, wantSearch: false},
		{maxTurns: 2, wantSearch: false},
		{maxTurns: 3, wantSearch: true},
		{maxTurns: 4, wantSearch: true},
	} {
		t.Run(fmt.Sprintf("maxTurns=%d", tt.maxTurns), func(t *testing.T) {
			provider := llm.NewMockProvider("cutoff").AddTextResponse("done")
			engine := llm.NewEngine(provider, nil)
			if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine); err != nil {
				t.Fatal(err)
			}
			stream, err := engine.Stream(context.Background(), llm.Request{
				EnableToolDiscovery: true,
				SessionID:           fmt.Sprintf("cutoff-%d", tt.maxTurns),
				Messages:            []llm.Message{llm.UserText("answer")},
				MaxTurns:            tt.maxTurns,
			})
			if err != nil {
				t.Fatal(err)
			}
			drainStream(t, stream)
			requests := provider.RecordedRequests()
			if len(requests) != 1 {
				t.Fatalf("provider requests = %d, want 1", len(requests))
			}
			if got := hasTool(requests[0].Tools, ToolSearchName); got != tt.wantSearch {
				t.Fatalf("tool_search visible = %v, want %v; tools=%v", got, tt.wantSearch, toolNames(requests[0].Tools))
			}
		})
	}
}

func TestForcedSearchRejectedAtAndAfterCutoff(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewMockProvider("forced-cutoff")
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range []struct {
		name       string
		maxTurns   int
		attempt    int
		lastChoice bool
	}{
		{name: "single turn", maxTurns: 1, attempt: 0},
		{name: "two turns", maxTurns: 2, attempt: 0},
		{name: "at cutoff", maxTurns: 3, attempt: 1},
		{name: "forced on final turn", maxTurns: 3, attempt: 2, lastChoice: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runID := fmt.Sprintf("forced-cutoff-%d", i)
			req := llm.Request{SessionID: runID, ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto}}
			if _, err := planner.BeginRun(context.Background(), provider, &req, runID); err != nil {
				t.Fatal(err)
			}
			defer planner.EndRun(runID)
			choice := llm.ToolChoice{Mode: llm.ToolChoiceName, Name: ToolSearchName}
			if tt.lastChoice {
				req.LastTurnToolChoice = &choice
			} else {
				req.ToolChoice = choice
			}
			if _, err := planner.PrepareTurn(context.Background(), provider, &req, runID, tt.attempt, tt.maxTurns); err == nil || !strings.Contains(err.Error(), "discovery cutoff") {
				t.Fatalf("PrepareTurn() error = %v, want forced-search cutoff rejection", err)
			}
		})
	}

	t.Run("engine last-turn choice", func(t *testing.T) {
		runProvider := llm.NewMockProvider("forced-last-turn")
		runProvider.AddToolCall("search", ToolSearchName, map[string]any{"tool_names": []string{"special_action"}})
		runProvider.AddToolCall("external", "federation__special_action", map[string]any{"project_id": "P-1"})
		runEngine := llm.NewEngine(runProvider, nil)
		runPlanner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, runEngine)
		if err != nil {
			t.Fatal(err)
		}
		lastChoice := llm.ToolChoice{Mode: llm.ToolChoiceName, Name: ToolSearchName}
		stream, err := runEngine.Stream(context.Background(), llm.Request{
			EnableToolDiscovery: true,
			SessionID:           "forced-last-turn",
			Messages:            []llm.Message{llm.UserText("perform the special action")},
			Tools:               []llm.ToolSpec{(&SearchTool{planner: runPlanner}).Spec()},
			LastTurnToolChoice:  &lastChoice,
			MaxTurns:            3,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		var cutoffErr error
		for {
			event, recvErr := stream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				cutoffErr = recvErr
				break
			}
			if event.Type == llm.EventError && event.Err != nil {
				cutoffErr = event.Err
				break
			}
		}
		if cutoffErr == nil || !strings.Contains(cutoffErr.Error(), "discovery cutoff") {
			t.Fatalf("last-turn forced search error = %v, want cutoff rejection", cutoffErr)
		}
		if got := len(runProvider.RecordedRequests()); got != 2 {
			t.Fatalf("provider requests before cutoff rejection = %d, want search and activated tool turns", got)
		}
	})
}

func TestPlannerRequestOptInAndToolChoicePayloadBoundary(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewMockProvider("opt-in").AddTextResponse("plain").AddTextResponse("discovery")
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, engine); err != nil {
		t.Fatal(err)
	}

	stream, err := engine.Stream(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("plain")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	first := provider.RecordedRequests()[0]
	if len(first.Tools) != 0 || first.ToolChoice.Mode != "" {
		t.Fatalf("non-opted request changed by planner: tools=%v choice=%#v", toolNames(first.Tools), first.ToolChoice)
	}

	stream, err = engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "opted", Messages: []llm.Message{llm.UserText("discover")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	second := provider.RecordedRequests()[1]
	if !hasTool(second.Tools, ToolSearchName) || second.ToolChoice.Mode != llm.ToolChoiceAuto {
		t.Fatalf("opted request did not receive planner-added auto surface: tools=%v choice=%#v", toolNames(second.Tools), second.ToolChoice)
	}

	emptyManager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{}})
	plainProvider := llm.NewMockProvider("no-add").AddTextResponse("done")
	plainEngine := llm.NewEngine(plainProvider, nil)
	local := &testLocalTool{name: "local_only"}
	plainEngine.RegisterTool(local)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, emptyManager, plainEngine); err != nil {
		t.Fatal(err)
	}
	stream, err = plainEngine.Stream(context.Background(), llm.Request{
		EnableToolDiscovery: true,
		Messages:            []llm.Message{llm.UserText("local")},
		Tools:               []llm.ToolSpec{local.Spec()},
		MaxTurns:            1,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if got := plainProvider.RecordedRequests()[0].ToolChoice.Mode; got != "" {
		t.Fatalf("unchanged tool surface changed empty ToolChoice to %q", got)
	}
}

type testLocalTool struct {
	name     string
	executed bool
}

func (t *testLocalTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: t.name, Schema: map[string]any{"type": "object"}}
}
func (t *testLocalTool) Preview(json.RawMessage) string { return t.name }
func (t *testLocalTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	t.executed = true
	return llm.TextOutput("executed"), nil
}

func TestAttachEngineCleansOldPlannerOwnership(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	oldEngine := llm.NewEngine(llm.NewMockProvider("old"), nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, oldEngine)
	if err != nil {
		t.Fatal(err)
	}
	newEngine := llm.NewEngine(llm.NewMockProvider("new"), nil)
	planner.AttachEngine(newEngine)
	if _, ok := oldEngine.ToolDiscoveryDiagnostics("session"); ok {
		t.Fatal("old engine retained planner ownership")
	}
	if got := len(oldEngine.Tools().AllSpecsIncludingDeferred()); got != 0 {
		t.Fatalf("old engine retained %d planner-owned wrappers", got)
	}
	if _, ok := newEngine.ToolDiscoveryDiagnostics("session"); !ok {
		t.Fatal("new engine did not acquire planner ownership")
	}
	if got := len(newEngine.Tools().AllSpecsIncludingDeferred()); got != 25 {
		t.Fatalf("new engine wrappers = %d, want 25", got)
	}

	sharedEngine := llm.NewEngine(llm.NewMockProvider("shared-old"), nil)
	oldPlanner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, sharedEngine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, sharedEngine); err != nil {
		t.Fatal(err)
	}
	oldPlanner.AttachEngine(llm.NewEngine(llm.NewMockProvider("shared-new"), nil))
	if _, ok := sharedEngine.ToolDiscoveryDiagnostics("session"); !ok {
		t.Fatal("old planner detached a newer planner owner")
	}
	if got := len(sharedEngine.Tools().AllSpecsIncludingDeferred()); got != 25 {
		t.Fatalf("old planner removed newer owner's wrappers: got %d", got)
	}
}

func TestPlannerSessionStateIsBounded(t *testing.T) {
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{}})
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, llm.NewEngine(llm.NewMockProvider("bounded"), nil))
	if err != nil {
		t.Fatal(err)
	}
	planner.mu.Lock()
	for i := 0; i < maxPlannerSessionStates+50; i++ {
		planner.stateLocked(fmt.Sprintf("session:%03d", i))
	}
	count := 0
	for key := range planner.sessions {
		if strings.HasPrefix(key, "session:") {
			count++
		}
	}
	_, newest := planner.sessions[fmt.Sprintf("session:%03d", maxPlannerSessionStates+49)]
	planner.mu.Unlock()
	if count != maxPlannerSessionStates || !newest {
		t.Fatalf("persistent session states = %d, newest=%v; want %d and retained newest", count, newest, maxPlannerSessionStates)
	}
}

func TestAlwaysLoadUsesCallerCatalogueSnapshot(t *testing.T) {
	manager := internalmcp.NewManagerWithConfig(&internalmcp.Config{Servers: map[string]internalmcp.ServerConfig{
		"server": {AlwaysLoad: []string{"pinned"}},
	}})
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, llm.NewEngine(llm.NewMockProvider("snapshot"), nil))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &internalmcp.CatalogueSnapshot{Generation: 7, Tools: []internalmcp.CatalogTool{{Name: "server__pinned"}}}
	if got := planner.alwaysLoadSet(snapshot); !got["server__pinned"] || len(got) != 1 {
		t.Fatalf("alwaysLoadSet(snapshot) = %#v, want caller generation tool", got)
	}
}
