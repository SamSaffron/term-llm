package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

func TestParseModelThinking(t *testing.T) {
	tests := []struct {
		input        string
		wantModel    string
		wantBudget   int64
		wantAdaptive bool
	}{
		// 4.6+ models: -thinking triggers adaptive (no budget)
		{"claude-sonnet-4-6-thinking", "claude-sonnet-4-6", 0, true},
		{"claude-opus-4-6-thinking", "claude-opus-4-6", 0, true},
		{"claude-opus-4-7-thinking", "claude-opus-4-7", 0, true},
		// Older models: -thinking triggers budget_tokens
		{"claude-haiku-4-5-thinking", "claude-haiku-4-5", 10000, false},
		// No suffix: no thinking
		{"claude-sonnet-4-6", "claude-sonnet-4-6", 0, false},
		{"claude-opus-4-6", "claude-opus-4-6", 0, false},
		{"claude-opus-4-7", "claude-opus-4-7", 0, false},
		{"claude-haiku-4-5", "claude-haiku-4-5", 0, false},
		// -1m-thinking: -thinking is stripped first, leaving -1m suffix intact
		// isAdaptiveModel sees "claude-sonnet-4-6-1m" which HasPrefix "claude-sonnet-4-6" -> adaptive
		{"claude-sonnet-4-6-1m-thinking", "claude-sonnet-4-6-1m", 0, true},
		{"claude-opus-4-6-1m-thinking", "claude-opus-4-6-1m", 0, true},
		{"claude-opus-4-7-1m-thinking", "claude-opus-4-7-1m", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model, budget, adaptive := parseModelThinking(tt.input)
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if budget != tt.wantBudget {
				t.Errorf("budget = %d, want %d", budget, tt.wantBudget)
			}
			if adaptive != tt.wantAdaptive {
				t.Errorf("adaptive = %v, want %v", adaptive, tt.wantAdaptive)
			}
		})
	}
}

func TestParseModel1m(t *testing.T) {
	tests := []struct {
		input     string
		wantModel string
		want1m    bool
	}{
		{"claude-sonnet-4-6-1m", "claude-sonnet-4-6", true},
		{"claude-opus-4-6-1m", "claude-opus-4-6", true},
		{"claude-opus-4-7-1m", "claude-opus-4-7", true},
		{"claude-sonnet-4-5-1m", "claude-sonnet-4-5", true},
		{"claude-sonnet-4-1m", "claude-sonnet-4", true},
		// No -1m suffix: passthrough
		{"claude-sonnet-4-6", "claude-sonnet-4-6", false},
		{"claude-haiku-4-5", "claude-haiku-4-5", false},
		// After -thinking is stripped, -1m suffix is parsed here
		{"claude-sonnet-4-6-1m", "claude-sonnet-4-6", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model, use1m := parseModel1m(tt.input)
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if use1m != tt.want1m {
				t.Errorf("use1m = %v, want %v", use1m, tt.want1m)
			}
		})
	}
}

func TestParseModelCombined1mThinking(t *testing.T) {
	// Test the full chain as used in NewAnthropicProvider:
	// strip -thinking first, then strip -1m
	tests := []struct {
		input        string
		wantModel    string
		wantAdaptive bool
		want1m       bool
	}{
		// Both suffixes
		{"claude-sonnet-4-6-1m-thinking", "claude-sonnet-4-6", true, true},
		{"claude-opus-4-6-1m-thinking", "claude-opus-4-6", true, true},
		// Only -thinking
		{"claude-sonnet-4-6-thinking", "claude-sonnet-4-6", true, false},
		// Only -1m
		{"claude-sonnet-4-6-1m", "claude-sonnet-4-6", false, true},
		// Neither
		{"claude-sonnet-4-6", "claude-sonnet-4-6", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			afterThinking, _, adaptive := parseModelThinking(tt.input)
			model, use1m := parseModel1m(afterThinking)
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if adaptive != tt.wantAdaptive {
				t.Errorf("adaptive = %v, want %v", adaptive, tt.wantAdaptive)
			}
			if use1m != tt.want1m {
				t.Errorf("use1m = %v, want %v", use1m, tt.want1m)
			}
		})
	}
}

func TestNewAnthropicProviderWithExplicitAPIKey(t *testing.T) {
	// Clear env to isolate test
	t.Setenv("ANTHROPIC_API_KEY", "")

	provider, err := NewAnthropicProvider("sk-test-key-123", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "api_key" {
		t.Fatalf("credential=%q, want %q", provider.Credential(), "api_key")
	}
}

func TestNewAnthropicProviderWithEnvAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key-456")

	provider, err := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "env" {
		t.Fatalf("credential=%q, want %q", provider.Credential(), "env")
	}
}

func TestNewAnthropicProviderExplicitKeyOverridesEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key")

	provider, err := NewAnthropicProvider("sk-explicit-key", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "api_key" {
		t.Fatalf("credential=%q, want %q (explicit key should override env)", provider.Credential(), "api_key")
	}
}

func TestToolCallAccumulatorInputJSONDelta(t *testing.T) {
	acc := newToolCallAccumulator()
	start := ToolCall{
		ID:   "tool-1",
		Name: "edit",
	}
	acc.Start(0, start)

	acc.Append(0, `{"file_path":"main.go","old_string":"foo"`)
	acc.Append(0, `,"new_string":"bar"}`)

	final, ok := acc.Finish(0)
	if !ok {
		t.Fatalf("expected tool call")
	}

	var payload map[string]string
	if err := json.Unmarshal(final.Arguments, &payload); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if payload["file_path"] != "main.go" {
		t.Fatalf("file_path=%q", payload["file_path"])
	}
	if payload["old_string"] != "foo" {
		t.Fatalf("old_string=%q", payload["old_string"])
	}
	if payload["new_string"] != "bar" {
		t.Fatalf("new_string=%q", payload["new_string"])
	}
}

func TestToolCallAccumulatorFallbackArgs(t *testing.T) {
	acc := newToolCallAccumulator()
	start := ToolCall{
		ID:        "tool-2",
		Name:      "edit",
		Arguments: json.RawMessage(`{"file_path":"main.go","old_string":"a","new_string":"b"}`),
	}
	acc.Start(1, start)

	final, ok := acc.Finish(1)
	if !ok {
		t.Fatalf("expected tool call")
	}

	var payload map[string]string
	if err := json.Unmarshal(final.Arguments, &payload); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if payload["new_string"] != "b" {
		t.Fatalf("new_string=%q", payload["new_string"])
	}
}

func TestBuildAnthropicBlocks_AssistantReasoningReplay(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "Final answer",
			ReasoningContent:          "I should inspect configuration first.",
			ReasoningEncryptedContent: "anthropic-signature-1",
		},
	}

	blocks := buildAnthropicBlocks(parts, true)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (thinking + text), got %d", len(blocks))
	}

	if got := blocks[0].GetSignature(); got == nil || *got != "anthropic-signature-1" {
		t.Fatalf("expected thinking signature anthropic-signature-1, got %v", got)
	}
	if got := blocks[0].GetThinking(); got == nil || *got != "I should inspect configuration first." {
		t.Fatalf("expected thinking text to round-trip, got %v", got)
	}
	if got := blocks[1].GetText(); got == nil || *got != "Final answer" {
		t.Fatalf("expected second block text Final answer, got %v", got)
	}
	if got := blocks[1].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in text block, got %v", got)
	}
}

func TestBuildAnthropicBlocks_UserDoesNotReplayReasoning(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "User message",
			ReasoningContent:          "ignored",
			ReasoningEncryptedContent: "ignored-signature",
		},
	}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 text block for non-assistant replay, got %d", len(blocks))
	}
	if got := blocks[0].GetText(); got == nil || *got != "User message" {
		t.Fatalf("expected user text block, got %v", got)
	}
	if got := blocks[0].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in non-assistant block, got %v", got)
	}
}

func TestBuildAnthropicBlocks_UserImagePart(t *testing.T) {
	parts := []Part{
		{
			Type:      PartImage,
			ImageData: &ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="},
		},
		{
			Type: PartText,
			Text: "What is in this image?",
		},
	}
	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].OfImage == nil {
		t.Fatal("expected first block to be an image block")
	}
	if blocks[0].OfImage.Source.OfBase64 == nil {
		t.Fatal("expected base64 image source")
	}
	if blocks[0].OfImage.Source.OfBase64.Data != "dGVzdA==" {
		t.Fatalf("Base64 data = %q, want %q", blocks[0].OfImage.Source.OfBase64.Data, "dGVzdA==")
	}
	if string(blocks[0].OfImage.Source.OfBase64.MediaType) != "image/jpeg" {
		t.Fatalf("MediaType = %q, want %q", blocks[0].OfImage.Source.OfBase64.MediaType, "image/jpeg")
	}
	if blocks[1].OfText == nil || blocks[1].OfText.Text != "What is in this image?" {
		t.Fatalf("expected second block to be text, got %+v", blocks[1])
	}
}

func TestBuildAnthropicToolResult_NonViewImageToolDoesNotParseImageMarker(t *testing.T) {
	content := "644: \tconst prefix = \"[IMAGE_DATA:\"\n645: \tconst suffix = \"]\""
	parts := []Part{{
		Type: PartToolResult,
		ToolResult: &ToolResult{
			ID:      "call-1",
			Name:    "read_file",
			Content: content,
		},
	}}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := blocks[0].OfToolResult
	if tr == nil {
		t.Fatalf("expected tool_result block")
	}
	if len(tr.Content) != 1 {
		t.Fatalf("expected 1 tool_result content block, got %d", len(tr.Content))
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfImage != nil {
		t.Fatalf("expected only text content block, got %#v", tr.Content[0])
	}
	if got := tr.Content[0].OfText.Text; got != content {
		t.Fatalf("expected tool_result text to remain unchanged, got %q", got)
	}
}

func TestBuildAnthropicToolResult_ViewImageToolUsesStructuredContentParts(t *testing.T) {
	parts := []Part{{
		Type: PartToolResult,
		ToolResult: &ToolResult{
			ID:      "call-1",
			Name:    "view_image",
			Content: "Image loaded",
			ContentParts: []ToolContentPart{
				{Type: ToolContentPartText, Text: "Image loaded"},
				{
					Type: ToolContentPartImageData,
					ImageData: &ToolImageData{
						MediaType: "image/png",
						Base64:    "aGVsbG8=",
					},
				},
				{Type: ToolContentPartText, Text: "done"},
			},
		},
	}}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := blocks[0].OfToolResult
	if tr == nil {
		t.Fatalf("expected tool_result block")
	}
	if len(tr.Content) != 3 {
		t.Fatalf("expected 3 tool_result content blocks (text + image + text), got %d", len(tr.Content))
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfText.Text != "Image loaded" {
		t.Fatalf("expected first content block to be text 'Image loaded', got %#v", tr.Content[0])
	}
	if tr.Content[1].OfImage == nil {
		t.Fatalf("expected second content block to be image, got %#v", tr.Content[1])
	}
	if tr.Content[2].OfText == nil || tr.Content[2].OfText.Text != "done" {
		t.Fatalf("expected third content block to be text 'done', got %#v", tr.Content[2])
	}
}

func TestBuildAnthropicToolResult_ViewImageToolDoesNotParseImageMarkerText(t *testing.T) {
	content := "Image loaded\n[IMAGE_DATA:image/png:aGVsbG8=]"
	parts := []Part{{
		Type: PartToolResult,
		ToolResult: &ToolResult{
			ID:      "call-1",
			Name:    "view_image",
			Content: content,
		},
	}}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := blocks[0].OfToolResult
	if tr == nil {
		t.Fatalf("expected tool_result block")
	}
	if len(tr.Content) != 1 {
		t.Fatalf("expected 1 tool_result content block, got %d", len(tr.Content))
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfImage != nil {
		t.Fatalf("expected only text content block, got %#v", tr.Content[0])
	}
	if got := tr.Content[0].OfText.Text; got != content {
		t.Fatalf("expected tool_result text to remain unchanged, got %q", got)
	}
}

func TestBuildAnthropicBetaToolResult_NonViewImageToolDoesNotParseImageMarker(t *testing.T) {
	content := "644: \tconst prefix = \"[IMAGE_DATA:\"\n645: \tconst suffix = \"]\""
	parts := []Part{{
		Type: PartToolResult,
		ToolResult: &ToolResult{
			ID:      "call-1",
			Name:    "read_file",
			Content: content,
		},
	}}

	blocks := buildAnthropicBetaBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := blocks[0].OfToolResult
	if tr == nil {
		t.Fatalf("expected beta tool_result block")
	}
	if len(tr.Content) != 1 {
		t.Fatalf("expected 1 beta tool_result content block, got %d", len(tr.Content))
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfImage != nil {
		t.Fatalf("expected only beta text content block, got %#v", tr.Content[0])
	}
	if got := tr.Content[0].OfText.Text; got != content {
		t.Fatalf("expected beta tool_result text to remain unchanged, got %q", got)
	}
}

func TestBuildAnthropicBetaToolResult_UsesStructuredContentParts(t *testing.T) {
	parts := []Part{{
		Type: PartToolResult,
		ToolResult: &ToolResult{
			ID:      "call-1",
			Name:    "view_image",
			Content: "Image loaded",
			ContentParts: []ToolContentPart{
				{Type: ToolContentPartText, Text: "Image loaded"},
				{
					Type:      ToolContentPartImageData,
					ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
				},
			},
		},
	}}

	blocks := buildAnthropicBetaBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := blocks[0].OfToolResult
	if tr == nil {
		t.Fatalf("expected beta tool_result block")
	}
	if len(tr.Content) != 2 {
		t.Fatalf("expected 2 beta tool_result content blocks (text + image), got %d", len(tr.Content))
	}
	if tr.Content[0].OfText == nil {
		t.Fatalf("expected first beta tool_result content block to be text, got %#v", tr.Content[0])
	}
	if tr.Content[1].OfImage == nil {
		t.Fatalf("expected second beta tool_result content block to be image, got %#v", tr.Content[1])
	}
}

func TestBuildAnthropicBetaBlocks_AssistantReasoningReplay(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "Search answer",
			ReasoningContent:          "I should verify sources.",
			ReasoningEncryptedContent: "anthropic-signature-beta-1",
		},
	}

	blocks := buildAnthropicBetaBlocks(parts, true)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 beta blocks (thinking + text), got %d", len(blocks))
	}

	if got := blocks[0].GetSignature(); got == nil || *got != "anthropic-signature-beta-1" {
		t.Fatalf("expected beta thinking signature anthropic-signature-beta-1, got %v", got)
	}
	if got := blocks[0].GetThinking(); got == nil || *got != "I should verify sources." {
		t.Fatalf("expected beta thinking text to round-trip, got %v", got)
	}
	if got := blocks[1].GetText(); got == nil || *got != "Search answer" {
		t.Fatalf("expected second beta block text Search answer, got %v", got)
	}
	if got := blocks[1].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in beta text block, got %v", got)
	}
}

func TestBuildAnthropicMessages_ConvertsDanglingToolCalls(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		UserText("Run shell"),
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working"},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-1",
						Name:      "shell",
						Arguments: json.RawMessage(`{"command":"sleep 10"}`),
					},
				},
			},
		},
		UserText("new request"),
	})

	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	// Orphaned tool_use becomes a text stub — so assistant message now has 2 text blocks.
	if len(out[1].Content) != 2 {
		t.Fatalf("expected 2 blocks (text + interrupted stub), got %d", len(out[1].Content))
	}
	if out[1].Content[0].OfText == nil || out[1].Content[0].OfText.Text != "Working" {
		t.Fatalf("expected first block to be original text, got %#v", out[1].Content[0])
	}
	if out[1].Content[1].OfText == nil {
		t.Fatalf("expected second block to be text stub for interrupted tool call, got %#v", out[1].Content[1])
	}
	if out[1].Content[1].OfToolUse != nil {
		t.Fatalf("expected no tool_use block after sanitization, got %#v", out[1].Content[1])
	}
}

func TestBuildAnthropicMessages_AppendsSyntheticUserAfterTrailingAssistant(t *testing.T) {
	summary := Message{
		Role:        RoleUser,
		Parts:       []Part{{Type: PartText, Text: summaryPrefix + "Previous context"}},
		CacheAnchor: true,
	}
	ack := AssistantText("I've reviewed the context summary. I'll continue from where we left off.")

	_, out := buildAnthropicMessages([]Message{summary, ack})
	if len(out) != 3 {
		t.Fatalf("expected 3 anthropic messages after normalization, got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("expected trailing anthropic role user, got %v", last.Role)
	}
	if len(last.Content) != 1 || last.Content[0].OfText == nil {
		t.Fatalf("expected trailing anthropic message to be a text user block, got %#v", last.Content)
	}
	if got := last.Content[0].OfText.Text; got != "Continue from the conversation state above." {
		t.Fatalf("synthetic user text = %q, want %q", got, "Continue from the conversation state above.")
	}
}

func TestBuildAnthropicBetaMessages_AppendsSyntheticUserAfterTrailingAssistant(t *testing.T) {
	summary := Message{
		Role:        RoleUser,
		Parts:       []Part{{Type: PartText, Text: summaryPrefix + "Previous context"}},
		CacheAnchor: true,
	}
	ack := AssistantText("I've reviewed the context summary. I'll continue from where we left off.")

	_, out := buildAnthropicBetaMessages([]Message{summary, ack})
	if len(out) != 3 {
		t.Fatalf("expected 3 anthropic beta messages after normalization, got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("expected trailing anthropic beta role user, got %v", last.Role)
	}
	if len(last.Content) != 1 || last.Content[0].OfText == nil {
		t.Fatalf("expected trailing anthropic beta message to be a text user block, got %#v", last.Content)
	}
	if got := last.Content[0].OfText.Text; got != "Continue from the conversation state above." {
		t.Fatalf("synthetic beta user text = %q, want %q", got, "Continue from the conversation state above.")
	}
}

func TestHandleAnthropicStartBlockContent_TextBlockEmitsTextDelta(t *testing.T) {
	events := make(chan Event, 1)
	acc := newToolCallAccumulator()

	if err := handleAnthropicStartBlockContent(eventSender{ctx: context.Background(), ch: events}, anthropic.TextBlock{
		Text: "hello from start block",
	}, 0, acc); err != nil {
		t.Fatalf("handleAnthropicStartBlockContent error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %v", ev.Type)
		}
		if ev.Text != "hello from start block" {
			t.Fatalf("expected start-block text to be emitted, got %q", ev.Text)
		}
	default:
		t.Fatal("expected one EventTextDelta from start text block")
	}
}

func TestHandleAnthropicStartBlockContent_ToolUseStartsAccumulator(t *testing.T) {
	events := make(chan Event, 1)
	acc := newToolCallAccumulator()

	if err := handleAnthropicStartBlockContent(eventSender{ctx: context.Background(), ch: events}, anthropic.ToolUseBlock{
		ID:    "call-1",
		Name:  "read_file",
		Input: json.RawMessage(`{"path":"README.md"}`),
	}, 1, acc); err != nil {
		t.Fatalf("handleAnthropicStartBlockContent error: %v", err)
	}

	if _, ok := acc.Finish(1); !ok {
		t.Fatal("expected tool call to be started in accumulator")
	}
	select {
	case ev := <-events:
		t.Fatalf("did not expect direct event for tool_use start, got %+v", ev)
	default:
	}
}

func TestHandleAnthropicBetaStartBlockContent_TextBlockEmitsTextDelta(t *testing.T) {
	events := make(chan Event, 1)
	acc := newToolCallAccumulator()

	if err := handleAnthropicBetaStartBlockContent(eventSender{ctx: context.Background(), ch: events}, anthropic.BetaTextBlock{
		Text: "hello from beta start block",
	}, 0, acc); err != nil {
		t.Fatalf("handleAnthropicBetaStartBlockContent error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %v", ev.Type)
		}
		if ev.Text != "hello from beta start block" {
			t.Fatalf("expected beta start-block text to be emitted, got %q", ev.Text)
		}
	default:
		t.Fatal("expected one EventTextDelta from beta start text block")
	}
}

func TestEmitReasoningDelta_ProducesReasoningEvent(t *testing.T) {
	events := make(chan Event, 1)

	if err := emitReasoningDelta(eventSender{ctx: context.Background(), ch: events}, "thinking chunk", "sig-123"); err != nil {
		t.Fatalf("emitReasoningDelta error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventReasoningDelta {
			t.Fatalf("expected EventReasoningDelta, got %v", ev.Type)
		}
		if ev.Text != "thinking chunk" {
			t.Fatalf("expected reasoning text chunk, got %q", ev.Text)
		}
		if ev.ReasoningEncryptedContent != "sig-123" {
			t.Fatalf("expected reasoning signature sig-123, got %q", ev.ReasoningEncryptedContent)
		}
	default:
		t.Fatal("expected reasoning event")
	}
}

func TestAnthropicThinkingBlockHelpersExposeReplayFields(t *testing.T) {
	thinking := anthropic.NewThinkingBlock("sig-x", "reasoning text")
	if got := thinking.GetSignature(); got == nil || *got != "sig-x" {
		t.Fatalf("expected helper signature sig-x, got %v", got)
	}
	if got := thinking.GetThinking(); got == nil || *got != "reasoning text" {
		t.Fatalf("expected helper thinking text, got %v", got)
	}
}

func TestBuildAnthropicTools_LastToolHasCacheControl(t *testing.T) {
	specs := []ToolSpec{
		{Name: "tool_a", Description: "first tool", Schema: map[string]interface{}{}},
		{Name: "tool_b", Description: "last tool", Schema: map[string]interface{}{}},
	}
	tools := buildAnthropicTools(specs)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	// Only last tool should have cache_control set
	if tools[0].OfTool == nil {
		t.Fatalf("expected first tool to be OfTool")
	}
	first := tools[0].OfTool.CacheControl
	if first.Type != "" {
		t.Fatalf("first tool should not have cache_control, got type=%q", first.Type)
	}
	if tools[1].OfTool == nil {
		t.Fatalf("expected last tool to be OfTool")
	}
	last := tools[1].OfTool.CacheControl
	if last.Type != "ephemeral" {
		t.Fatalf("last tool should have cache_control type=ephemeral, got %q", last.Type)
	}
}

func TestBuildAnthropicTools_EmptySpecsReturnsNil(t *testing.T) {
	tools := buildAnthropicTools(nil)
	if tools != nil {
		t.Fatalf("expected nil for empty specs, got %v", tools)
	}
}

func TestBuildAnthropicBetaTools_LastToolHasCacheControl(t *testing.T) {
	specs := []ToolSpec{
		{Name: "tool_a", Description: "first tool", Schema: map[string]interface{}{}},
		{Name: "tool_b", Description: "last tool", Schema: map[string]interface{}{}},
	}
	tools := buildAnthropicBetaTools(specs)
	if len(tools) != 2 {
		t.Fatalf("expected 2 beta tools, got %d", len(tools))
	}
	if tools[0].OfTool == nil {
		t.Fatalf("expected first beta tool to be OfTool")
	}
	first := tools[0].OfTool.CacheControl
	if first.Type != "" {
		t.Fatalf("first beta tool should not have cache_control, got type=%q", first.Type)
	}
	if tools[1].OfTool == nil {
		t.Fatalf("expected last beta tool to be OfTool")
	}
	last := tools[1].OfTool.CacheControl
	if last.Type != "ephemeral" {
		t.Fatalf("last beta tool should have cache_control type=ephemeral, got %q", last.Type)
	}
}

func TestApplyLastMessageCacheControl_TextBlock(t *testing.T) {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("hello"),
		),
	}
	applyLastMessageCacheControl(messages)
	block := messages[0].Content[0]
	if block.OfText == nil {
		t.Fatalf("expected text block")
	}
	if block.OfText.CacheControl.Type != "ephemeral" {
		t.Fatalf("expected cache_control type=ephemeral, got %q", block.OfText.CacheControl.Type)
	}
}

func TestApplyLastMessageCacheControl_ToolResult(t *testing.T) {
	toolBlock := anthropic.ToolResultBlockParam{
		ToolUseID: "call-1",
	}
	messages := []anthropic.MessageParam{
		{
			Role: anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{
				{OfToolResult: &toolBlock},
			},
		},
	}
	applyLastMessageCacheControl(messages)
	block := messages[0].Content[0]
	if block.OfToolResult == nil {
		t.Fatalf("expected tool_result block")
	}
	if block.OfToolResult.CacheControl.Type != "ephemeral" {
		t.Fatalf("expected cache_control type=ephemeral, got %q", block.OfToolResult.CacheControl.Type)
	}
}

func TestApplyLastMessageCacheControl_EmptyMessages(t *testing.T) {
	// Should not panic on empty slice
	applyLastMessageCacheControl(nil)
	applyLastMessageCacheControl([]anthropic.MessageParam{})
}

func TestApplyLastMessageCacheControl_EmptyContent(t *testing.T) {
	messages := []anthropic.MessageParam{
		{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{}},
	}
	// Should not panic on empty content
	applyLastMessageCacheControl(messages)
}

func TestApplyLastMessageCacheControl_OnlyLastMessageAffected(t *testing.T) {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("first")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("second")),
	}
	applyLastMessageCacheControl(messages)
	first := messages[0].Content[0]
	if first.OfText != nil && first.OfText.CacheControl.Type == "ephemeral" {
		t.Fatalf("first message should not have cache_control")
	}
	last := messages[1].Content[0]
	if last.OfText == nil || last.OfText.CacheControl.Type != "ephemeral" {
		t.Fatalf("last message should have cache_control type=ephemeral")
	}
}

func TestBuildAnthropicBetaTools_EmptySpecsReturnsNil(t *testing.T) {
	tools := buildAnthropicBetaTools(nil)
	if tools != nil {
		t.Fatalf("expected nil for empty specs, got %v", tools)
	}
}

func TestApplyBetaLastMessageCacheControl_TextBlock(t *testing.T) {
	messages := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hello")),
	}
	applyBetaLastMessageCacheControl(messages)
	block := messages[0].Content[0]
	if block.OfText == nil {
		t.Fatalf("expected text block")
	}
	if block.OfText.CacheControl.Type != "ephemeral" {
		t.Fatalf("expected cache_control type=ephemeral, got %q", block.OfText.CacheControl.Type)
	}
}

func TestApplyBetaLastMessageCacheControl_EmptyMessages(t *testing.T) {
	applyBetaLastMessageCacheControl(nil)
	applyBetaLastMessageCacheControl([]anthropic.BetaMessageParam{})
}

func TestBuildAnthropicTools_SingleTool(t *testing.T) {
	specs := []ToolSpec{
		{
			Name:        "only_tool",
			Description: "the only tool",
			Schema:      map[string]interface{}{},
		},
	}
	tools := buildAnthropicTools(specs)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].OfTool == nil {
		t.Fatalf("expected OfTool")
	}
	if tools[0].OfTool.CacheControl.Type != "ephemeral" {
		t.Fatalf("single tool should have cache_control type=ephemeral, got %q", tools[0].OfTool.CacheControl.Type)
	}
}

func TestBuildAnthropicTools_ToolHasSchema(t *testing.T) {
	specs := []ToolSpec{
		{
			Name:        "read_file",
			Description: "reads a file",
			Schema:      map[string]interface{}{},
		},
	}
	tools := buildAnthropicTools(specs)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	schema := tools[0].OfTool.InputSchema
	if schema.Type != constant.Object("object") {
		t.Fatalf("expected schema type=object")
	}
}

func TestBuildAnthropicMessages_DeveloperRole(t *testing.T) {
	messages := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Be concise"}}},
		UserText("Hello"),
	}

	_, out := buildAnthropicMessages(messages)

	if len(out) != 1 {
		t.Fatalf("expected 1 message (developer merged into user), got %d", len(out))
	}
	if out[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("expected user role, got %v", out[0].Role)
	}
	if len(out[0].Content) != 1 || out[0].Content[0].OfText == nil {
		t.Fatalf("expected single text block, got %#v", out[0].Content)
	}
	got := out[0].Content[0].OfText.Text
	want := "<developer>\nBe concise\n</developer>\n\nHello"
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestBuildAnthropicMessages_DeveloperRoleNotMergedWithoutUser(t *testing.T) {
	// Developer message at end with no following user message — should be dropped
	messages := []Message{
		UserText("Hello"),
		AssistantText("Hi"),
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Be concise"}}},
	}

	_, out := buildAnthropicMessages(messages)

	// Should have user + assistant + synthetic user (trailing assistant normalization)
	// but no developer content since there's no user turn to merge into
	for _, msg := range out {
		if msg.Role == anthropic.MessageParamRoleUser {
			for _, block := range msg.Content {
				if block.OfText != nil && strings.Contains(block.OfText.Text, "<developer>") {
					t.Fatalf("developer tag should not appear without a following user message, got %q", block.OfText.Text)
				}
			}
		}
	}
}

func TestBuildAnthropicBetaMessages_DeveloperRole(t *testing.T) {
	messages := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Be concise"}}},
		UserText("Hello"),
	}

	_, out := buildAnthropicBetaMessages(messages)

	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("expected user role, got %v", out[0].Role)
	}
	if len(out[0].Content) != 1 {
		t.Fatalf("expected single content block, got %d", len(out[0].Content))
	}
	got := out[0].Content[0].GetText()
	if got == nil {
		t.Fatalf("expected text block, got %#v", out[0].Content[0])
	}
	want := "<developer>\nBe concise\n</developer>\n\nHello"
	if *got != want {
		t.Errorf("content = %q, want %q", *got, want)
	}
}

func TestStreamWithSearchServerToolExecEnd(t *testing.T) {
	msgStart := `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}

`
	serverToolStart := `event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

`
	blockStop1 := `event: content_block_stop
data: {"type":"content_block_stop","index":1}

`
	textBlockStart := `event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

`
	textDelta := `event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"hello"}}

`
	blockStop2 := `event: content_block_stop
data: {"type":"content_block_stop","index":2}

`
	msgDelta := `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

`
	msgStop := `event: message_stop
data: {"type":"message_stop"}

`

	tests := []struct {
		name           string
		sse            string
		wantExecStarts int
		wantExecEnds   int
		wantText       string
	}{
		{
			name:           "block_stop_ends_server_tool",
			sse:            msgStart + serverToolStart + blockStop1 + msgDelta + msgStop,
			wantExecStarts: 1,
			wantExecEnds:   1,
		},
		{
			name:           "server_tool_then_text_no_double_end",
			sse:            msgStart + serverToolStart + blockStop1 + textBlockStart + textDelta + blockStop2 + msgDelta + msgStop,
			wantExecStarts: 1,
			wantExecEnds:   1,
			wantText:       "hello",
		},
		{
			name:           "stream_end_fallback",
			sse:            msgStart + serverToolStart + msgStop,
			wantExecStarts: 1,
			wantExecEnds:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tt.sse)
			}))
			defer ts.Close()

			client := anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(ts.URL))
			provider := &AnthropicProvider{client: &client, model: "claude-sonnet-4-6"}

			stream, err := provider.streamWithSearch(context.Background(), Request{
				Messages: []Message{UserText("test")},
			})
			if err != nil {
				t.Fatalf("streamWithSearch error: %v", err)
			}
			defer stream.Close()

			var execStarts, execEnds int
			var textContent string
			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv error: %v", err)
				}
				switch ev.Type {
				case EventToolExecStart:
					if ev.ToolName != "web_search" {
						t.Errorf("EventToolExecStart ToolName = %q, want %q", ev.ToolName, "web_search")
					}
					execStarts++
				case EventToolExecEnd:
					if ev.ToolName != "web_search" {
						t.Errorf("EventToolExecEnd ToolName = %q, want %q", ev.ToolName, "web_search")
					}
					if !ev.ToolSuccess {
						t.Error("EventToolExecEnd ToolSuccess = false, want true")
					}
					execEnds++
				case EventTextDelta:
					textContent += ev.Text
				case EventDone:
					goto done
				}
			}
		done:
			if execStarts != tt.wantExecStarts {
				t.Errorf("EventToolExecStart count = %d, want %d", execStarts, tt.wantExecStarts)
			}
			if execEnds != tt.wantExecEnds {
				t.Errorf("EventToolExecEnd count = %d, want %d", execEnds, tt.wantExecEnds)
			}
			if tt.wantText != "" && textContent != tt.wantText {
				t.Errorf("text content = %q, want %q", textContent, tt.wantText)
			}
		})
	}
}

func TestAnthropicStreamCloseDoesNotDeadlockWhenConsumerStopsDraining(t *testing.T) {
	var sse strings.Builder
	sse.WriteString("event: message_start\n")
	sse.WriteString("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
	sse.WriteString("event: content_block_start\n")
	sse.WriteString("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for i := 0; i < 64; i++ {
		sse.WriteString("event: content_block_delta\n")
		sse.WriteString(fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"chunk-%02d\"}}\n\n", i))
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse.String())
	}))
	defer ts.Close()

	client := anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(ts.URL))
	provider := &AnthropicProvider{
		client: &client,
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Read just one event, then stop draining.
	stream.Recv()

	// Close must return promptly even though the buffer is full.
	done := make(chan struct{})
	go func() {
		stream.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close() blocked for >2s — probable deadlock")
	}
}

// minimalAnthropicSSE returns a valid-but-empty SSE stream sufficient to let a
// Stream call complete without errors. Used by request-serialization tests
// that only care about the outbound HTTP body.
func minimalAnthropicSSE() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func TestAnthropicStreamSerializesReasoningEffort(t *testing.T) {
	tests := []struct {
		name       string
		reqEffort  string
		wantEffort string
	}{
		{"low", "low", "low"},
		{"medium", "medium", "medium"},
		{"high", "high", "high"},
		{"xhigh", "xhigh", "xhigh"},
		{"max", "max", "max"},
		{"uppercase_gets_lowercased", "HIGH", "high"},
		{"mixed_case_gets_lowercased", "XHigh", "xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]json.RawMessage

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, minimalAnthropicSSE())
			}))
			defer ts.Close()

			client := anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(ts.URL))
			provider := &AnthropicProvider{client: &client, model: "claude-sonnet-4-6"}

			stream, err := provider.Stream(context.Background(), Request{
				Messages:        []Message{UserText("hello")},
				ReasoningEffort: tt.reqEffort,
			})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			defer stream.Close()

			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv() error: %v", err)
				}
				if ev.Type == EventDone {
					break
				}
			}

			rawOutput, ok := body["output_config"]
			if !ok {
				t.Fatalf("request missing output_config; body keys = %v", keys(body))
			}
			var outputConfig struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(rawOutput, &outputConfig); err != nil {
				t.Fatalf("decode output_config: %v", err)
			}
			if outputConfig.Effort != tt.wantEffort {
				t.Errorf("output_config.effort = %q, want %q", outputConfig.Effort, tt.wantEffort)
			}
		})
	}
}

func TestAnthropicStreamOmitsOutputConfigWhenEffortEmpty(t *testing.T) {
	var body map[string]json.RawMessage

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, minimalAnthropicSSE())
	}))
	defer ts.Close()

	client := anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(ts.URL))
	provider := &AnthropicProvider{client: &client, model: "claude-sonnet-4-6"}

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error: %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if _, present := body["output_config"]; present {
		t.Errorf("output_config should not be sent when ReasoningEffort is empty; body = %v", body)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
