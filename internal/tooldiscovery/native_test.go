package tooldiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/llm"
)

type nativeScriptTurn struct {
	events []llm.Event
	err    error
}

type nativeScriptProvider struct {
	mu        sync.Mutex
	supported bool
	turns     []nativeScriptTurn
	requests  []llm.Request
	resets    int
}

func (p *nativeScriptProvider) Name() string       { return "native-script" }
func (p *nativeScriptProvider) Credential() string { return "mock" }
func (p *nativeScriptProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}
func (p *nativeScriptProvider) NativeToolDiscoverySupport(string) llm.NativeToolDiscoverySupport {
	if p.supported {
		return llm.NativeToolDiscoverySupport{Supported: true, Name: "test-native", Reason: "test capability"}
	}
	return llm.NativeToolDiscoverySupport{Reason: "test provider does not support native discovery"}
}
func (p *nativeScriptProvider) ResetConversation() {
	p.mu.Lock()
	p.resets++
	p.mu.Unlock()
}
func (p *nativeScriptProvider) Stream(_ context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.turns) == 0 {
		return nil, errors.New("native script exhausted")
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	if turn.err != nil {
		return nil, turn.err
	}
	return &nativeScriptStream{events: append([]llm.Event(nil), turn.events...)}, nil
}

type nativeScriptStream struct {
	events []llm.Event
}

func (s *nativeScriptStream) Recv() (llm.Event, error) {
	if len(s.events) == 0 {
		return llm.Event{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *nativeScriptStream) Close() error { return nil }

func TestNativeStrategyOrchestratesDiscoveryWithoutTopLevelSchemaMutation(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true, turns: []nativeScriptTurn{
		{events: []llm.Event{{Type: llm.EventDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"tool_names":["special_action"]}`)}}}},
		{events: []llm.Event{{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "external-1", Namespace: "federation", ChildName: "special_action", Arguments: json.RawMessage(`{"project_id":"P-42"}`)}}}},
		{events: []llm.Event{{Type: llm.EventTextDelta, Text: "done"}}},
	}}
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "auto", Strategy: "auto", Threshold: 24}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{
		EnableToolDiscovery: true,
		SessionID:           "native",
		Model:               "native-model",
		Messages:            []llm.Message{llm.UserText("perform the special action")},
		MaxTurns:            3,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)

	if len(provider.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(provider.requests))
	}
	if provider.requests[0].NativeToolDiscovery == nil {
		t.Fatal("initial request omitted native discovery control")
	}
	for i, request := range provider.requests {
		if len(request.Tools) != 0 {
			t.Fatalf("request %d top-level tools = %v, want stable empty ordinary surface", i, toolNames(request.Tools))
		}
	}
	if provider.requests[1].NativeToolDiscovery != nil {
		t.Fatal("native discovery remained enabled after final-turn cutoff")
	}
	var callSeen, outputSeen bool
	for _, message := range provider.requests[1].Messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartDiscoveryCall && part.DiscoveryCall != nil {
				callSeen = true
			}
			if part.Type == llm.PartDiscoveryOutput && part.DiscoveryOutput != nil {
				outputSeen = true
				if len(part.DiscoveryOutput.Tools) != 1 || part.DiscoveryOutput.Tools[0].Spec.Name != "federation__special_action" {
					t.Fatalf("discovery output = %#v", part.DiscoveryOutput)
				}
			}
		}
	}
	if !callSeen || !outputSeen {
		t.Fatalf("native replay parts call=%v output=%v", callSeen, outputSeen)
	}
	var routedCall *llm.ToolCall
	for _, message := range provider.requests[2].Messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID == "external-1" {
				routedCall = part.ToolCall
			}
		}
	}
	if routedCall == nil || routedCall.Name != "federation__special_action" || routedCall.Namespace != "federation" || routedCall.ChildName != "special_action" {
		t.Fatalf("persisted native route = %#v", routedCall)
	}
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("native")
	if diagnostics.Strategy != string(StrategyNative) || diagnostics.DynamicActive != 1 || diagnostics.FallbackCount != 0 {
		t.Fatalf("native diagnostics = %+v", diagnostics)
	}
}

func TestNativeHistoryDoesNotRestoreEvictedTools(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true}
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native", MaxActiveTools: 1}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.CatalogueSnapshot()
	if snapshot == nil || len(snapshot.Tools) < 2 {
		t.Fatal("expected at least two catalogue tools")
	}
	first, second := snapshot.Tools[0], snapshot.Tools[1]
	req := llm.Request{SessionID: "native-eviction", Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-old"}},
		{Type: llm.PartDiscoveryOutput, DiscoveryOutput: &llm.ToolDiscoveryOutput{CallID: "search-old", Tools: []llm.DiscoveredTool{
			{Spec: first.ToolSpec(), SchemaHash: first.SchemaHash},
			{Spec: second.ToolSpec(), SchemaHash: second.SchemaHash},
		}}},
	}}}}
	key := planner.stateKey(req.SessionID, "")
	planner.mu.Lock()
	planner.stateLocked(key).evicted[first.Name] = true
	planner.mu.Unlock()

	originalOutput := req.Messages[0].Parts[1].DiscoveryOutput
	planner.restoreNativeHistory(&req, key)
	output := req.Messages[0].Parts[1].DiscoveryOutput
	if output == nil || len(output.Tools) != 1 || output.Tools[0].Spec.Name != second.Name {
		t.Fatalf("restored discovery output = %+v, want only %q", output, second.Name)
	}
	if len(originalOutput.Tools) != 2 {
		t.Fatalf("caller-owned discovery output was mutated: %+v", originalOutput)
	}
	got := toolNames(planner.ActiveToolSpecs(req.SessionID))
	if len(got) != 1 || got[0] != second.Name {
		t.Fatalf("restored active tools = %v, want [%s]", got, second.Name)
	}

	emptyOutput := &llm.ToolDiscoveryOutput{CallID: "search-empty", Tools: []llm.DiscoveredTool{
		{Spec: first.ToolSpec(), SchemaHash: first.SchemaHash},
		{Spec: second.ToolSpec(), SchemaHash: second.SchemaHash},
	}}
	emptyReq := llm.Request{SessionID: "native-all-evicted", Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-empty"}},
		{Type: llm.PartDiscoveryOutput, DiscoveryOutput: emptyOutput},
	}}}}
	emptyKey := planner.stateKey(emptyReq.SessionID, "")
	planner.mu.Lock()
	emptyState := planner.stateLocked(emptyKey)
	emptyState.evicted[first.Name] = true
	emptyState.evicted[second.Name] = true
	planner.mu.Unlock()
	planner.restoreNativeHistory(&emptyReq, emptyKey)
	if len(emptyReq.Messages[0].Parts) != 0 {
		t.Fatalf("empty discovery call/output pair was retained: %+v", emptyReq.Messages[0].Parts)
	}
	if len(emptyOutput.Tools) != 2 {
		t.Fatalf("caller-owned empty discovery output was mutated: %+v", emptyOutput)
	}
}

func TestNativeNamespaceCallRejectsUnselectedSibling(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true, turns: []nativeScriptTurn{
		{events: []llm.Event{{Type: llm.EventDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"tool_names":["special_action"]}`)}}}},
		{events: []llm.Event{{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "sibling-1", Namespace: "federation", ChildName: "realistic_operation_00", Arguments: json.RawMessage(`{}`)}}}},
	}}
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{
		EnableToolDiscovery: true,
		SessionID:           "unselected-sibling",
		Model:               "native-model",
		Messages:            []llm.Message{llm.UserText("act")},
		MaxTurns:            3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var gotErr error
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, io.EOF) {
				gotErr = recvErr
			}
			break
		}
		if event.Type == llm.EventError {
			gotErr = event.Err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "not loaded and authorised") {
		t.Fatalf("unselected sibling error = %v", gotErr)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider continued after rejected sibling: %d requests", len(provider.requests))
	}
}

func TestNativeAutoFallbackOccursOnceBeforeSideEffects(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true, turns: []nativeScriptTurn{
		{err: errors.New("native tool_search rejected")},
		{events: []llm.Event{{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "portable-search", Name: ToolSearchName, Arguments: json.RawMessage(`{"tool_names":["special_action"]}`)}}}},
		{events: []llm.Event{{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "external", Name: "federation__special_action", Arguments: json.RawMessage(`{"project_id":"P-1"}`)}}}},
		{events: []llm.Event{{Type: llm.EventTextDelta, Text: "done"}}},
	}}
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "auto"}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, SessionID: "fallback", Model: "native-model", Messages: []llm.Message{llm.UserText("act")}, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, stream)
	if len(provider.requests) != 4 || provider.requests[0].NativeToolDiscovery == nil || !hasTool(provider.requests[1].Tools, ToolSearchName) {
		t.Fatalf("fallback requests = %#v", provider.requests)
	}
	diagnostics, _ := engine.ToolDiscoveryDiagnostics("fallback")
	if diagnostics.Strategy != string(StrategyPortable) || diagnostics.FallbackCount != 1 || !strings.Contains(diagnostics.FallbackReason, "rejected") {
		t.Fatalf("fallback diagnostics = %+v", diagnostics)
	}
	if provider.resets != 1 {
		t.Fatalf("provider resets = %d, want 1", provider.resets)
	}
}

func TestAutoStrategyUsesPortableWithoutProvenNativeCapability(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: false}
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "auto"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{SessionID: "qwen-like", Model: "qwen"}
	if _, err := planner.BeginRun(context.Background(), provider, &req, "qwen-run"); err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("qwen-run")
	if req.NativeToolDiscovery != nil || !hasTool(req.Tools, ToolSearchName) {
		t.Fatalf("auto fallback surface native=%v tools=%v", req.NativeToolDiscovery != nil, toolNames(req.Tools))
	}
	diagnostics := planner.Diagnostics("qwen-like")
	if diagnostics.Strategy != string(StrategyPortable) || !strings.Contains(diagnostics.StrategyReason, "not proven") {
		t.Fatalf("portable diagnostics = %+v", diagnostics)
	}
}

func TestChatGPTLunaAutoStrategyResolvesNative(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := llm.NewChatGPTProviderWithCredsAndOptions(
		&credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		"gpt-5.6-luna-medium",
		llm.ChatGPTProviderOptions{UseWebSocket: true},
	)
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "auto", Strategy: "auto", Threshold: 24}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{SessionID: "luna", Model: "gpt-5.6-luna-medium"}
	if _, err := planner.BeginRun(context.Background(), provider, &req, "luna-run"); err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("luna-run")
	if req.NativeToolDiscovery == nil || hasTool(req.Tools, ToolSearchName) {
		t.Fatalf("Luna auto surface native=%v tools=%v", req.NativeToolDiscovery != nil, toolNames(req.Tools))
	}
	if got := planner.Diagnostics("luna").Strategy; got != string(StrategyNative) {
		t.Fatalf("Luna strategy = %q, want native", got)
	}
}

func TestNativeReplayStaleSchemaIsInvalidatedAndReset(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true}
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.CatalogueSnapshot()
	var spec llm.ToolSpec
	for _, tool := range snapshot.Tools {
		if tool.Name == "federation__special_action" {
			spec = tool.ToolSpec()
			break
		}
	}
	req := llm.Request{SessionID: "stale", Model: "native-model", Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-old", Arguments: json.RawMessage(`{"query":"special"}`)}},
		{Type: llm.PartDiscoveryOutput, DiscoveryOutput: &llm.ToolDiscoveryOutput{CallID: "search-old", CatalogueGen: snapshot.Generation - 1, Tools: []llm.DiscoveredTool{{Spec: spec, SchemaHash: "stale-hash"}}}},
	}}}}
	resetReason, err := planner.BeginRun(context.Background(), provider, &req, "stale-run")
	if err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("stale-run")
	if !strings.Contains(resetReason, "schema changed") {
		t.Fatalf("reset reason = %q", resetReason)
	}
	for _, message := range req.Messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartDiscoveryCall || part.Type == llm.PartDiscoveryOutput {
				t.Fatalf("stale discovery replay survived: %#v", part)
			}
		}
	}
}

func TestNativeReplayOldGenerationReResolvesCurrentCatalogue(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true}
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.CatalogueSnapshot()
	var selected llm.DiscoveredTool
	for _, tool := range snapshot.Tools {
		if tool.Name == "federation__special_action" {
			selected = llm.DiscoveredTool{Spec: tool.ToolSpec(), SchemaHash: tool.SchemaHash}
			break
		}
	}
	output := &llm.ToolDiscoveryOutput{CallID: "search-old", CatalogueHash: "old", CatalogueGen: 0, Tools: []llm.DiscoveredTool{selected}}
	req := llm.Request{SessionID: "old-generation", Model: "native-model", Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-old", Arguments: json.RawMessage(`{"query":"special"}`)}},
		{Type: llm.PartDiscoveryOutput, DiscoveryOutput: output},
	}}}}
	resetReason, err := planner.BeginRun(context.Background(), provider, &req, "old-generation-run")
	if err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("old-generation-run")
	if resetReason != "" {
		t.Fatalf("unchanged schema forced reset: %q", resetReason)
	}
	resolvedOutput := req.Messages[0].Parts[1].DiscoveryOutput
	if resolvedOutput == nil || resolvedOutput.CatalogueGen != snapshot.Generation || resolvedOutput.CatalogueHash != snapshot.Hash || resolvedOutput.Tools[0].Spec.Name != "federation__special_action" {
		t.Fatalf("re-resolved output = %#v", resolvedOutput)
	}
	if output.CatalogueGen != 0 || output.CatalogueHash != "old" {
		t.Fatalf("caller-owned output was mutated = %#v", output)
	}
}

func TestNativeSearchCannotLoadDeniedTool(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true}
	engine := llm.NewEngine(provider, nil)
	engine.SetAllowedToolsFilter([]string{})
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{SessionID: "denied", Model: "native-model"}
	if _, err := planner.BeginRun(context.Background(), provider, &req, "denied-run"); err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("denied-run")
	if _, err := planner.PrepareTurn(context.Background(), provider, &req, "denied-run", 0, 3); err != nil {
		t.Fatal(err)
	}
	_, err = planner.ResolveNativeToolDiscovery(context.Background(), "denied-run", llm.ToolDiscoveryCall{ID: "search", Arguments: json.RawMessage(`{"tool_names":["special_action"]}`)})
	if err == nil || !strings.Contains(err.Error(), "unavailable or denied") {
		t.Fatalf("denied search error = %v", err)
	}
}

func TestNativeFallbackRejectedAfterCommit(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: true}
	engine := llm.NewEngine(provider, nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "auto"}, manager, engine)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{SessionID: "committed", Model: "native-model"}
	if _, err := planner.BeginRun(context.Background(), provider, &req, "committed-run"); err != nil {
		t.Fatal(err)
	}
	defer planner.EndRun("committed-run")
	if fallback, _ := planner.FallbackNativeToolDiscovery("committed-run", errors.New("late failure"), true); fallback {
		t.Fatal("native fallback succeeded after a committed side effect")
	}
}

func TestForcedNativeUnsupportedProviderFailsClearly(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	provider := &nativeScriptProvider{supported: false, turns: []nativeScriptTurn{{events: []llm.Event{{Type: llm.EventTextDelta, Text: "unexpected"}}}}}
	engine := llm.NewEngine(provider, nil)
	if _, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, engine); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), llm.Request{EnableToolDiscovery: true, Model: "unsupported", Messages: []llm.Message{llm.UserText("act")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var gotErr error
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, io.EOF) {
				gotErr = recvErr
			}
			break
		}
		if event.Type == llm.EventError && event.Err != nil {
			gotErr = event.Err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "strategy native is unsupported") {
		t.Fatalf("error = %v", gotErr)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("unsupported forced native reached provider: %d requests", len(provider.requests))
	}
}
