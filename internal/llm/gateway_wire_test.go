package llm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

func TestGatewayWireRequestRoundTripAndOmitsLocalState(t *testing.T) {
	req := Request{
		Model: "vision-model", SessionID: "session-1", WorkingDir: "/satellite/private",
		ApprovalTranscriptPrefix: []Message{UserText("private approval")},
		AllowedTools:             []string{"read_file"}, AllowedToolsPresent: true, Debug: true, DebugRaw: true,
		Messages: []Message{
			{Role: RoleUser, ApprovalRole: "private-approval-role", ClientMessageID: "private-client-id", ResponseID: "private-response-id", AssistantSegmentOrdinal: 2, SegmentStartSequence: 3, SegmentEndSequence: 4, Parts: []Part{
				{Type: PartText, Text: "hello", ReasoningContent: "summary", ReasoningKind: ReasoningKindSummary},
				{Type: PartImage, ImagePath: "/private/image.png", ImageData: &ToolImageData{MediaType: "image/png", Base64: "aW1hZ2U="}},
				{Type: PartFile, FilePath: "/private/file.pdf", FileData: &ToolFileData{MediaType: "application/pdf", Base64: "ZmlsZQ==", Filename: "file.pdf"}},
			}},
			{Role: RoleAssistant, Parts: []Part{
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "view", Arguments: json.RawMessage(`{"x":1}`), Caller: "programmatic", ToolInfo: "private-tool-info", ThoughtSig: []byte("sig")}},
				{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(`{"type":"opaque"}`)}},
			}},
			{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ID: "c1", Name: "view", Content: "ok", ContentParts: []ToolContentPart{{Type: ToolContentPartText, Text: "ok"}, {Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aQ=="}}}, Display: "private-result-display", Images: []string{"/private/result.png"}}}}},
		},
		Tools:      []ToolSpec{{Name: "view", Description: "view", Schema: map[string]any{"type": "object"}, Strict: true, AllowedCallers: []string{"programmatic"}, OutputSchema: map[string]any{"type": "string"}}},
		ToolChoice: ToolChoice{Mode: ToolChoiceName, Name: "view"}, ParallelToolCalls: true,
		Search: true, ForceExternalSearch: true, ReasoningEffort: "high", MaxOutputTokens: 42,
		Temperature: 0, TemperatureSet: true, TopP: .5, TopPSet: true,
		Responses: &ResponsesOptions{ReasoningMode: "summary", MultiAgent: MultiAgentOptions{Enabled: true, EnabledSet: true, MaxConcurrentSubagents: 2}},
		MaxTurns:  9, ToolMap: map[string]string{"view": "local_view"},
	}
	wire, err := EncodeGatewayRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, forbidden := range []string{"WorkingDir", "working_dir", "/satellite/private", "/private/image.png", "/private/file.pdf", "/private/result.png", "private approval", "private-approval-role", "private-client-id", "private-response-id", "private-tool-info", "private-result-display", "AllowedTools", "max_turns", "tool_map", "DebugRaw"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("wire request leaked forbidden %q: %s", forbidden, text)
		}
	}
	got, err := DecodeGatewayRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != req.Model || got.SessionID != req.SessionID || len(got.Messages) != 3 || !got.Search || !got.TemperatureSet || got.WorkingDir != "" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Messages[0].Parts[1].ImageData == nil || got.Messages[0].Parts[2].FileData == nil {
		t.Fatalf("vision/file data lost: %+v", got.Messages[0].Parts)
	}
	if got.Messages[1].Parts[0].ToolCall.ToolInfo != "" || got.Messages[2].Parts[0].ToolResult.Display != "" || got.Messages[0].ApprovalRole != "" || got.Messages[0].ClientMessageID != "" || got.Messages[0].ResponseID != "" {
		t.Fatalf("local/display metadata crossed wire: %+v", got.Messages)
	}
	if got.Messages[2].Parts[0].ToolResult.Images != nil {
		t.Fatalf("local result paths crossed wire: %+v", got.Messages[2].Parts[0].ToolResult)
	}
}

func TestGatewayWireEventRoundTripAllFields(t *testing.T) {
	event := Event{
		Type: EventReasoningDelta, Text: "reason", Model: "m", ReasoningEffort: "high",
		ReasoningItemID: "r1", ReasoningEncryptedContent: "sealed", ReasoningKind: ReasoningKindSummary,
		ReasoningSummaryParts: []string{"a", "b"}, ReasoningIndex: 2, ReasoningFinal: true,
		Tool:       &ToolCall{ID: "c", Name: "tool", Arguments: json.RawMessage(`{"a":1}`), ThoughtSig: []byte("sig")},
		ToolCallID: "c", ToolName: "tool", ToolInfo: "info", ToolArgs: json.RawMessage(`{"a":1}`),
		ToolSuccess: true, ToolOutput: "out", ToolDiffs: []DiffData{{File: "f", Old: "a", New: "b", Line: 1}},
		ToolFileChanges: []FileChange{{Path: "f", Kind: "modify", Adds: 1}}, ToolImages: []string{"image"},
		Use:          &Usage{InputTokens: 1, OutputTokens: 2, CachedInputTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5},
		RetryAttempt: 1, RetryMaxAttempts: 3, RetryWaitSecs: .25,
		ToolActivity:   &ToolActivity{ID: "a", Name: "search", Status: ToolActivityCompleted},
		ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(`{"opaque":true}`)},
		ImageData:      []byte("image"), ImageMimeType: "image/png", RevisedPrompt: "better",
	}
	wire, err := EncodeGatewayEvent(event, &protocol.Error{Code: "provider_upstream_failure", Message: "safe error"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"message":{}`) {
		t.Fatalf("empty event message was serialized: %s", wire)
	}
	got, err := DecodeGatewayEvent(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != event.Type || got.Text != event.Text || got.Tool == nil || got.Tool.Name != "tool" || got.Use == nil || got.Use.ReasoningTokens != 5 || got.ProviderReplay == nil || got.Err == nil || !strings.Contains(got.Err.Error(), "safe error") || string(got.ImageData) != "image" {
		t.Fatalf("event round trip mismatch: %+v", got)
	}
	var gatewayErr *GatewayError
	if !errors.As(got.Err, &gatewayErr) || gatewayErr.Code != "provider_upstream_failure" {
		t.Fatalf("structured gateway error = %#v", got.Err)
	}
}

func TestDecodeGatewayRequestRejectsUnknownTopLevelField(t *testing.T) {
	_, err := DecodeGatewayRequest([]byte(`{"model":"m","working_dir":"/forged"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("got %v, want controlled unknown-field error", err)
	}
}
