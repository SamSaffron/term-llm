package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

type equivalenceProviderRecord struct {
	Request  llm.Request
	Imported string
}

type equivalenceProviderRecorder struct {
	mu      sync.Mutex
	records []equivalenceProviderRecord
}

func (r *equivalenceProviderRecorder) add(req llm.Request, imported string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, equivalenceProviderRecord{Request: req, Imported: imported})
}

func (r *equivalenceProviderRecorder) snapshot() []equivalenceProviderRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]equivalenceProviderRecord(nil), r.records...)
}

type equivalenceStateProvider struct {
	recorder *equivalenceProviderRecorder

	mu       sync.Mutex
	imported string
	exported string
}

func (*equivalenceStateProvider) Name() string       { return "equivalence-state" }
func (*equivalenceStateProvider) Credential() string { return "mock" }
func (*equivalenceStateProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *equivalenceStateProvider) ImportProviderState(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.imported = string(data)
	return nil
}

func (p *equivalenceStateProvider) ExportProviderState() ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exported == "" {
		return nil, false
	}
	return []byte(p.exported), true
}

func (p *equivalenceStateProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	imported := p.imported
	wantState := "state:" + req.SessionID
	if imported != "" && imported != wantState {
		p.mu.Unlock()
		return nil, fmt.Errorf("provider state %q does not match session %q", imported, req.SessionID)
	}
	p.exported = wantState
	p.mu.Unlock()
	p.recorder.add(req, imported)
	return &oneEventStream{ctx: ctx, event: llm.Event{Type: llm.EventTextDelta, Text: "ok"}}, nil
}

func installReconstructedProviderFactory(fixture *gatewayFixture, recorder *equivalenceProviderRecorder) {
	fixture.gateway.cfg.ProviderFactory = func(*config.Config, string, string) (llm.Provider, error) {
		return &equivalenceStateProvider{recorder: recorder}, nil
	}
}

func TestGatewayMultiTurnTranscriptToolReasoningAndPersistedState(t *testing.T) {
	recorder := &equivalenceProviderRecorder{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, &equivalenceStateProvider{recorder: recorder}, time.Second)
	installReconstructedProviderFactory(fixture, recorder)

	firstSatellite, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	firstMessages := []llm.Message{llm.UserText("first question")}
	stream, err := firstSatellite.Stream(t.Context(), llm.Request{
		Model: "model-a", SessionID: "session-a", Messages: firstMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)

	persisted, ok := firstSatellite.ExportProviderState()
	if !ok || strings.Contains(string(persisted), "state:session-a") {
		t.Fatalf("exported gateway state = %q, %t; want opaque sealed state", persisted, ok)
	}

	// Reconstruct the satellite provider, as a resumed runtime does after loading
	// ProviderState from its session database.
	resumedSatellite, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := resumedSatellite.ImportProviderState(persisted); err != nil {
		t.Fatal(err)
	}

	replayRaw := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"sealed-reasoning"}`)
	secondMessages := []llm.Message{
		llm.UserText("first question"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartText, Text: "I need a tool", ReasoningContent: "summary", ReasoningSummaryParts: []string{"summary"}, ReasoningItemID: "rs_1", ReasoningEncryptedContent: "sealed-reasoning", ReasoningKind: llm.ReasoningKindSummary},
			{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: replayRaw}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"term-llm"}`), Caller: "programmatic", ToolInfo: "(/satellite/private)", ThoughtSig: []byte("thought")}},
		}},
		llm.ToolResultMessageFromOutput("call_1", "lookup", llm.ToolOutput{Content: "tool answer", ContentParts: []llm.ToolContentPart{{Type: llm.ToolContentPartText, Text: "tool answer"}}, Diffs: []llm.DiffData{{File: "/satellite/private", New: "secret"}}, Images: []string{"/satellite/result.png"}}, nil),
		llm.UserText("second question"),
	}
	stream, err = resumedSatellite.Stream(t.Context(), llm.Request{
		Model: "model-a", SessionID: "session-a", Messages: secondMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)

	records := recorder.snapshot()
	if len(records) != 2 {
		t.Fatalf("central provider requests = %d, want 2", len(records))
	}
	if records[0].Request.SessionID != "session-a" || !reflect.DeepEqual(records[0].Request.Messages, firstMessages) {
		t.Fatalf("first central request lost session/transcript: %#v", records[0].Request)
	}
	if records[1].Request.SessionID != "session-a" || len(records[1].Request.Messages) != len(secondMessages) {
		t.Fatalf("second central request lost session/transcript: %#v", records[1].Request)
	}
	if records[1].Imported != "state:session-a" {
		t.Fatalf("reconstructed central provider imported %q, want session state", records[1].Imported)
	}
	assistant := records[1].Request.Messages[1]
	if len(assistant.Parts) != 3 || assistant.Parts[1].ProviderReplay == nil || string(assistant.Parts[1].ProviderReplay.Raw) != string(replayRaw) {
		t.Fatalf("reasoning/provider replay was not preserved: %#v", assistant.Parts)
	}
	if assistant.Parts[0].ReasoningEncryptedContent != "sealed-reasoning" || assistant.Parts[0].ReasoningContent != "summary" {
		t.Fatalf("reasoning fields were not preserved: %#v", assistant.Parts[0])
	}
	if assistant.Parts[2].ToolCall == nil || assistant.Parts[2].ToolCall.ID != "call_1" || assistant.Parts[2].ToolCall.ToolInfo != "" {
		t.Fatalf("tool call semantics/display sanitization mismatch: %#v", assistant.Parts[2])
	}
	result := records[1].Request.Messages[2].Parts[0].ToolResult
	if result == nil || result.ID != "call_1" || result.Content != "tool answer" || len(result.ContentParts) != 1 || len(result.Diffs) != 0 || len(result.Images) != 0 {
		t.Fatalf("tool-result continuation mismatch: %#v", result)
	}
}

func TestGatewayServerRestartRestoresSealedProviderStateWithSameKey(t *testing.T) {
	recorder := &equivalenceProviderRecorder{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, &equivalenceStateProvider{recorder: recorder}, time.Second)
	installReconstructedProviderFactory(fixture, recorder)

	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(t.Context(), llm.Request{Model: "model-a", SessionID: "restart-session", Messages: []llm.Message{llm.UserText("before restart")}})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)
	persisted, ok := provider.ExportProviderState()
	if !ok {
		t.Fatal("gateway provider did not export state before restart")
	}

	fixture.server.Close()
	sealer, err := OpenStateSealer(filepath.Join(fixture.stateDir, "state.key"))
	if err != nil {
		t.Fatal(err)
	}
	clients, err := OpenClientStore(filepath.Join(fixture.stateDir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewServer(ServerConfig{
		Config: fixture.central, Clients: clients, Sealer: sealer, Usage: fixture.usage,
		ProviderFactory: func(*config.Config, string, string) (llm.Provider, error) {
			return &equivalenceStateProvider{recorder: recorder}, nil
		},
		Policy: Policy{AllowCLI: true, AllowSearch: true, AllowFetch: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedHTTP := httptest.NewServer(restarted.Handler())
	defer restartedHTTP.Close()
	satelliteConfig := fixture.satelliteConfig()
	satelliteConfig.Gateway.URL = restartedHTTP.URL

	resumed, err := llm.NewGatewayProvider(satelliteConfig, "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.ImportProviderState(persisted); err != nil {
		t.Fatal(err)
	}
	stream, err = resumed.Stream(t.Context(), llm.Request{Model: "model-a", SessionID: "restart-session", Messages: []llm.Message{llm.UserText("before restart"), llm.AssistantText("ok"), llm.UserText("after restart")}})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)

	records := recorder.snapshot()
	if len(records) != 2 || records[1].Imported != "state:restart-session" || len(records[1].Request.Messages) != 3 {
		t.Fatalf("restart records = %#v, want imported state plus full transcript", records)
	}
}

func TestGatewaySimultaneousSessionsDoNotCrossBindState(t *testing.T) {
	recorder := &equivalenceProviderRecorder{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, &equivalenceStateProvider{recorder: recorder}, time.Second)
	installReconstructedProviderFactory(fixture, recorder)

	providers := make(map[string]*llm.GatewayProvider)
	for _, sessionID := range []string{"session-a", "session-b"} {
		provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
		if err != nil {
			t.Fatal(err)
		}
		providers[sessionID] = provider
		stream, err := provider.Stream(t.Context(), llm.Request{Model: "model-a", SessionID: sessionID, Messages: []llm.Message{llm.UserText("first " + sessionID)}})
		if err != nil {
			t.Fatal(err)
		}
		collectStream(t, stream)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for sessionID, provider := range providers {
		sessionID, provider := sessionID, provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := provider.Stream(context.Background(), llm.Request{Model: "model-a", SessionID: sessionID, Messages: []llm.Message{llm.UserText("first " + sessionID), llm.AssistantText("ok"), llm.UserText("second " + sessionID)}})
			if err != nil {
				errs <- err
				return
			}
			defer stream.Close()
			for {
				_, err = stream.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						errs <- err
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	secondTurns := map[string]bool{}
	for _, record := range recorder.snapshot() {
		if len(record.Request.Messages) == 3 {
			want := "state:" + record.Request.SessionID
			if record.Imported != want {
				t.Fatalf("session %q imported %q, want %q", record.Request.SessionID, record.Imported, want)
			}
			secondTurns[record.Request.SessionID] = true
		}
	}
	if !secondTurns["session-a"] || !secondTurns["session-b"] {
		t.Fatalf("simultaneous second turns = %#v", secondTurns)
	}
}

func TestGatewayRejectsSealedStateAcrossSessions(t *testing.T) {
	recorder := &equivalenceProviderRecorder{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, &equivalenceStateProvider{recorder: recorder}, time.Second)
	installReconstructedProviderFactory(fixture, recorder)

	sessionA, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := sessionA.Stream(t.Context(), llm.Request{Model: "model-a", SessionID: "session-a", Messages: []llm.Message{llm.UserText("a")}})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)
	sealed, ok := sessionA.ExportProviderState()
	if !ok {
		t.Fatal("session A did not export sealed state")
	}

	sessionB, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionB.ImportProviderState(sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionB.Stream(t.Context(), llm.Request{Model: "model-a", SessionID: "session-b", Messages: []llm.Message{llm.UserText("b")}}); err == nil || !strings.Contains(err.Error(), "invalid_state") {
		t.Fatalf("cross-session state error = %v, want invalid_state", err)
	}
	if records := recorder.snapshot(); len(records) != 1 {
		t.Fatalf("cross-session state reached provider Stream: %#v", records)
	}
}

func TestGatewayStatelessRequestDoesNotRoundTripProviderState(t *testing.T) {
	recorder := &equivalenceProviderRecorder{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, &equivalenceStateProvider{recorder: recorder}, time.Second)
	installReconstructedProviderFactory(fixture, recorder)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(t.Context(), llm.Request{Model: "model-a", Messages: []llm.Message{llm.UserText("one shot")}})
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)
	if state, ok := provider.ExportProviderState(); ok || len(state) != 0 {
		t.Fatalf("stateless request exported provider state %q, %t", state, ok)
	}
}

func cloneRequestForComparison(t *testing.T, req llm.Request) llm.Request {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var cloned llm.Request
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

// normalizeDocumentedGatewayDifferences removes only fields that are deliberately
// satellite-local or display-only. Every remaining field can affect provider
// semantics and must compare exactly.
func normalizeDocumentedGatewayDifferences(req llm.Request) llm.Request {
	req.WorkingDir = ""
	req.ApprovalTranscriptPrefix = nil
	req.AllowedTools = nil
	req.AllowedToolsPresent = false
	req.MaxTurns = 0
	req.ToolMap = nil
	req.Debug = false
	req.DebugRaw = false
	for i := range req.Messages {
		message := &req.Messages[i]
		message.ApprovalRole = ""
		message.ClientMessageID = ""
		message.ResponseID = ""
		message.AssistantSegmentOrdinal = 0
		message.SegmentStartSequence = 0
		message.SegmentEndSequence = 0
		parts := message.Parts[:0]
		for _, part := range message.Parts {
			if part.Type == llm.PartSkillActivation || part.Type == llm.PartToolActivity {
				continue
			}
			part.ImagePath = ""
			part.FilePath = ""
			if part.ToolCall != nil {
				part.ToolCall.ToolInfo = ""
			}
			if part.ToolResult != nil {
				part.ToolResult.Display = ""
				part.ToolResult.Diffs = nil
				part.ToolResult.Images = nil
			}
			parts = append(parts, part)
		}
		message.Parts = parts
	}
	return req
}

func TestGatewayProviderRequestMatchesDirectAfterDocumentedNormalization(t *testing.T) {
	mock := llm.NewMockProvider("central").AddTextResponse("ok")
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, mock, time.Second)
	provider, err := llm.NewGatewayProvider(fixture.satelliteConfig(), "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	lastChoice := llm.ToolChoice{Mode: llm.ToolChoiceName, Name: "lookup"}
	req := llm.Request{
		Model: "model-a", SessionID: "differential-session", WorkingDir: "/satellite/private", Ephemeral: true,
		IncludeDeveloperInContinuation: true,
		Messages: []llm.Message{{
			Role: llm.RoleUser, CacheAnchor: true, ApprovalRole: "reviewer", ClientMessageID: "client-message", ResponseID: "response", AssistantSegmentOrdinal: 2, SegmentStartSequence: 3, SegmentEndSequence: 4,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "hello", ReasoningContent: "reason", ReasoningSummaryParts: []string{"one", "two"}, ReasoningItemID: "rs", ReasoningEncryptedContent: "encrypted", ReasoningKind: llm.ReasoningKindSummary},
				{Type: llm.PartImage, ImagePath: "/satellite/image.png", ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aQ==", Detail: "high"}},
				{Type: llm.PartFile, FilePath: "/satellite/file.pdf", FileData: &llm.ToolFileData{MediaType: "application/pdf", Base64: "Zg==", Filename: "file.pdf", SizeBytes: 1}},
				{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{"x":1}`), Caller: "programmatic", ToolInfo: "(private)", ThoughtSig: []byte("sig")}},
				{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: json.RawMessage(`{"type":"reasoning","id":"rs"}`)}},
				{Type: llm.PartToolActivity, ToolActivity: &llm.ToolActivity{Name: "search", Info: "private display", Status: llm.ToolActivityCompleted}},
				{Type: llm.PartSkillActivation, SkillActivation: &llm.SkillActivationProvenance{Name: "private-skill", SourcePath: "/satellite/skill"}},
			},
		}, {
			Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call", Name: "lookup", Content: "result", ContentParts: []llm.ToolContentPart{{Type: llm.ToolContentPartText, Text: "result"}}, Display: "private display", Diffs: []llm.DiffData{{File: "/satellite/private", New: "x"}}, Images: []string{"/satellite/result.png"}, IsError: true, Caller: "programmatic", ThoughtSig: []byte("sig")}}},
		}},
		ApprovalTranscriptPrefix: []llm.Message{llm.UserText("approval-only")},
		Tools:                    []llm.ToolSpec{{Name: "lookup", Description: "look up", Schema: map[string]any{"type": "object", "additionalProperties": false}, Strict: true, AllowedCallers: []string{"programmatic"}, OutputSchema: map[string]any{"type": "string"}}},
		ToolChoice:               llm.ToolChoice{Mode: llm.ToolChoiceRequired}, LastTurnToolChoice: &lastChoice, ParallelToolCalls: true,
		AllowedTools: []string{"lookup"}, AllowedToolsPresent: true,
		Search: true, ForceExternalSearch: true, DisableExternalWebFetch: true,
		ReasoningEffort: "high", Responses: &llm.ResponsesOptions{ReasoningMode: "summary", ReasoningContext: "preserve", MultiAgent: llm.MultiAgentOptions{Enabled: true, EnabledSet: true, MaxConcurrentSubagents: 2}, ProgrammaticToolCalling: llm.ProgrammaticToolCallingOptions{Enabled: true, EnabledSet: true, Tools: []string{"lookup"}}, PromptCache: llm.PromptCacheOptions{Mode: "memory", TTL: "1h"}},
		MaxOutputTokens: 321, Temperature: 0, TemperatureSet: true, TopP: .7, TopPSet: true,
		ServiceTier: "priority", ServiceTierSet: true, MaxTurns: 7, ToolMap: map[string]string{"lookup": "local_lookup"}, Debug: true, DebugRaw: true,
	}

	stream, err := provider.Stream(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, stream)
	recorded := mock.RecordedRequests()
	if len(recorded) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(recorded))
	}

	want := normalizeDocumentedGatewayDifferences(cloneRequestForComparison(t, req))
	got := normalizeDocumentedGatewayDifferences(cloneRequestForComparison(t, recorded[0]))
	if !reflect.DeepEqual(got, want) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("direct/gateway provider request semantic mismatch\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
}
