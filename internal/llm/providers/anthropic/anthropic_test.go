package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	llmstream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newAnthropicTestProvider builds a provider whose client posts to a test
// server. model may be empty when the test does not assert on the request body.
func newAnthropicTestProvider(t *testing.T, baseURL, model string) *AnthropicProvider {
	t.Helper()
	client, err := newAnthropicClient("test-key", baseURL, nil)
	if err != nil {
		t.Fatalf("newAnthropicClient: %v", err)
	}
	return &AnthropicProvider{client: client, model: model}
}

func TestParseModelThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input        string
		wantModel    string
		wantBudget   int64
		wantAdaptive bool
	}{
		// 4.6+ models: -thinking triggers adaptive (no budget)
		{"claude-fable-5-thinking", "claude-fable-5", 0, true},
		{"claude-opus-4-8-thinking", "claude-opus-4-8", 0, true},
		{"claude-sonnet-4-6-thinking", "claude-sonnet-4-6", 0, true},
		{"claude-opus-4-6-thinking", "claude-opus-4-6", 0, true},
		{"claude-opus-4-7-thinking", "claude-opus-4-7", 0, true},
		// Older models: -thinking triggers budget_tokens
		{"claude-haiku-4-5-thinking", "claude-haiku-4-5", 10000, false},
		// No suffix: no thinking
		{"claude-fable-5", "claude-fable-5", 0, false},
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
	t.Parallel()

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
	t.Parallel()

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

func TestAnthropicRequestModelAndEffort(t *testing.T) {
	t.Parallel()

	provider := &AnthropicProvider{model: "claude-sonnet-4-6"}
	tests := []struct {
		name       string
		provider   *AnthropicProvider
		req        core.Request
		wantModel  string
		wantEffort string
	}{
		{
			name:       "request opus max suffix",
			req:        core.Request{Model: "claude-opus-4-8-max"},
			wantModel:  "claude-opus-4-8",
			wantEffort: "max",
		},
		{
			name:       "request sonnet high suffix",
			req:        core.Request{Model: "claude-sonnet-4-6-high"},
			wantModel:  "claude-sonnet-4-6",
			wantEffort: "high",
		},
		{
			name:       "invalid sonnet xhigh suffix is preserved",
			req:        core.Request{Model: "claude-sonnet-4-6-xhigh"},
			wantModel:  "claude-sonnet-4-6-xhigh",
			wantEffort: "",
		},
		{
			name:       "explicit request effort overrides model suffix",
			req:        core.Request{Model: "claude-opus-4-8-max", ReasoningEffort: "low"},
			wantModel:  "claude-opus-4-8",
			wantEffort: "low",
		},
		{
			name:       "configured suffix is default effort",
			provider:   &AnthropicProvider{model: "claude-opus-4-8", reasoningEffort: "high"},
			req:        core.Request{},
			wantModel:  "claude-opus-4-8",
			wantEffort: "high",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := provider
			if tt.provider != nil {
				p = tt.provider
			}
			gotModel, gotEffort := p.requestModelAndEffort(tt.req)
			if gotModel != tt.wantModel || gotEffort != tt.wantEffort {
				t.Fatalf("requestModelAndEffort = (%q, %q), want (%q, %q)", gotModel, gotEffort, tt.wantModel, tt.wantEffort)
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
	start := core.ToolCall{
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
	start := core.ToolCall{
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

// TestBuildAnthropicBlocks_AssistantReasoningReplay checks that assistant
// reasoning is replayed ahead of the text block as a signed thinking block.
// The standard and beta builders were merged, so the former beta variant is
// covered here as a second case with its own values.
func TestBuildAnthropicBlocks_AssistantReasoningReplay(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		reasoning string
		signature string
	}{
		{
			name:      "assistant reasoning",
			text:      "Final answer",
			reasoning: "I should inspect configuration first.",
			signature: "anthropic-signature-1",
		},
		{
			name:      "beta search answer reasoning",
			text:      "Search answer",
			reasoning: "I should verify sources.",
			signature: "anthropic-signature-beta-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := []core.Part{
				{
					Type:                      core.PartText,
					Text:                      tc.text,
					ReasoningContent:          tc.reasoning,
					ReasoningEncryptedContent: tc.signature,
				},
			}

			blocks := buildAnthropicBlocks(parts, true)
			if len(blocks) != 2 {
				t.Fatalf("expected 2 blocks (thinking + text), got %d", len(blocks))
			}

			thinking, ok := blocks[0].(*anthropicThinkingBlock)
			if !ok {
				t.Fatalf("expected thinking block first, got %T", blocks[0])
			}
			if thinking.Signature != tc.signature {
				t.Fatalf("expected thinking signature %s, got %q", tc.signature, thinking.Signature)
			}
			if thinking.Thinking != tc.reasoning {
				t.Fatalf("expected thinking text to round-trip, got %q", thinking.Thinking)
			}
			// The second block must be text: a text block has no signature field
			// to carry thinking metadata.
			text, ok := blocks[1].(*anthropicTextBlock)
			if !ok {
				t.Fatalf("expected text block second, got %T", blocks[1])
			}
			if text.Text != tc.text {
				t.Fatalf("expected second block text %s, got %q", tc.text, text.Text)
			}
		})
	}
}

func TestBuildAnthropicBlocks_UserDoesNotReplayReasoning(t *testing.T) {
	parts := []core.Part{
		{
			Type:                      core.PartText,
			Text:                      "User message",
			ReasoningContent:          "ignored",
			ReasoningEncryptedContent: "ignored-signature",
		},
	}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 text block for non-assistant replay, got %d", len(blocks))
	}
	text, ok := blocks[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("did not expect thinking metadata in non-assistant block, got %T", blocks[0])
	}
	if text.Text != "User message" {
		t.Fatalf("expected user text block, got %q", text.Text)
	}
}

func TestBuildAnthropicBlocks_UserImagePart(t *testing.T) {
	parts := []core.Part{
		{
			Type:      core.PartImage,
			ImageData: &core.ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="},
		},
		{
			Type: core.PartText,
			Text: "What is in this image?",
		},
	}
	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	image, ok := blocks[0].(*anthropicImageBlock)
	if !ok {
		t.Fatalf("expected first block to be an image block, got %T", blocks[0])
	}
	if image.Source.Type != "base64" {
		t.Fatalf("expected base64 image source, got %q", image.Source.Type)
	}
	if image.Source.Data != "dGVzdA==" {
		t.Fatalf("Base64 data = %q, want %q", image.Source.Data, "dGVzdA==")
	}
	if image.Source.MediaType != "image/jpeg" {
		t.Fatalf("MediaType = %q, want %q", image.Source.MediaType, "image/jpeg")
	}
	text, ok := blocks[1].(*anthropicTextBlock)
	if !ok || text.Text != "What is in this image?" {
		t.Fatalf("expected second block to be text, got %#v", blocks[1])
	}
}

func TestAnthropicToolResultMarkerTextRemainsText(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		content string
	}{
		{name: "normal non-image tool", tool: "read_file", content: "644: \tconst prefix = \"[IMAGE_DATA:\"\n645: \tconst suffix = \"]\""},
		{name: "normal image marker", tool: "view_image", content: "Image loaded\n[IMAGE_DATA:image/png:aGVsbG8=]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "call-1", Name: tc.tool, Content: tc.content}}}
			blocks := buildAnthropicBlocks(parts, false)
			if len(blocks) != 1 {
				t.Fatalf("unexpected tool result: %#v", blocks)
			}
			result, ok := blocks[0].(*anthropicToolResultBlock)
			if !ok || len(result.Content) != 1 {
				t.Fatalf("unexpected tool result: %#v", blocks)
			}
			content, ok := result.Content[0].(*anthropicTextBlock)
			if !ok || content.Text != tc.content {
				t.Fatalf("expected unchanged text content, got %#v", result.Content[0])
			}
		})
	}
}

// TestBuildAnthropicToolResult_ViewImageToolUsesStructuredContentParts covers
// both the text+image+text and text+image shapes. The latter was the former
// beta variant; both now exercise the shared block builder.
func TestBuildAnthropicToolResult_ViewImageToolUsesStructuredContentParts(t *testing.T) {
	cases := []struct {
		name  string
		parts []core.ToolContentPart
	}{
		{
			name: "text_image_text",
			parts: []core.ToolContentPart{
				{Type: core.ToolContentPartText, Text: "Image loaded"},
				{
					Type: core.ToolContentPartImageData,
					ImageData: &core.ToolImageData{
						MediaType: "image/png",
						Base64:    "aGVsbG8=",
					},
				},
				{Type: core.ToolContentPartText, Text: "done"},
			},
		},
		{
			name: "text_image",
			parts: []core.ToolContentPart{
				{Type: core.ToolContentPartText, Text: "Image loaded"},
				{
					Type:      core.ToolContentPartImageData,
					ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := []core.Part{{
				Type: core.PartToolResult,
				ToolResult: &core.ToolResult{
					ID:           "call-1",
					Name:         "view_image",
					Content:      "Image loaded",
					ContentParts: tc.parts,
				},
			}}

			blocks := buildAnthropicBlocks(parts, false)
			if len(blocks) != 1 {
				t.Fatalf("expected 1 block, got %d", len(blocks))
			}
			tr, ok := blocks[0].(*anthropicToolResultBlock)
			if !ok {
				t.Fatalf("expected tool_result block, got %T", blocks[0])
			}
			if len(tr.Content) != len(tc.parts) {
				t.Fatalf("expected %d tool_result content blocks, got %d", len(tc.parts), len(tr.Content))
			}
			for i, part := range tc.parts {
				switch part.Type {
				case core.ToolContentPartText:
					text, ok := tr.Content[i].(*anthropicTextBlock)
					if !ok || text.Text != part.Text {
						t.Fatalf("expected content block %d to be text %q, got %#v", i, part.Text, tr.Content[i])
					}
				case core.ToolContentPartImageData:
					image, ok := tr.Content[i].(*anthropicImageBlock)
					if !ok {
						t.Fatalf("expected content block %d to be image, got %#v", i, tr.Content[i])
					}
					if image.Source.MediaType != part.ImageData.MediaType || image.Source.Data != part.ImageData.Base64 {
						t.Fatalf("content block %d image source = %#v, want %#v", i, image.Source, part.ImageData)
					}
				default:
					t.Fatalf("unhandled content part %q", part.Type)
				}
			}
		})
	}
}

// TestAnthropicThinkingBlockExposesReplayFields pins the wire keys a replayed
// assistant thinking block must keep so the API accepts it back on the next
// request.
func TestAnthropicThinkingBlockExposesReplayFields(t *testing.T) {
	block := &anthropicThinkingBlock{Type: "thinking", Signature: "sig-x", Thinking: "reasoning text"}

	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal thinking block: %v", err)
	}
	var wire map[string]string
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal thinking block: %v", err)
	}
	if wire["type"] != "thinking" {
		t.Fatalf("type = %q, want %q", wire["type"], "thinking")
	}
	if wire["signature"] != "sig-x" {
		t.Fatalf("signature = %q, want %q", wire["signature"], "sig-x")
	}
	if wire["thinking"] != "reasoning text" {
		t.Fatalf("thinking = %q, want %q", wire["thinking"], "reasoning text")
	}
}

func TestBuildAnthropicMessages_ConvertsDanglingToolCalls(t *testing.T) {
	_, out := buildAnthropicMessages([]core.Message{
		core.UserText("Run shell"),
		{
			Role: core.RoleAssistant,
			Parts: []core.Part{
				{Type: core.PartText, Text: "Working"},
				{
					Type: core.PartToolCall,
					ToolCall: &core.ToolCall{
						ID:        "call-1",
						Name:      "shell",
						Arguments: json.RawMessage(`{"command":"sleep 10"}`),
					},
				},
			},
		},
		core.UserText("new request"),
	})

	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	// Orphaned tool_use becomes a text stub — so assistant message now has 2 text blocks.
	if len(out[1].Content) != 2 {
		t.Fatalf("expected 2 blocks (text + interrupted stub), got %d", len(out[1].Content))
	}
	first, ok := out[1].Content[0].(*anthropicTextBlock)
	if !ok || first.Text != "Working" {
		t.Fatalf("expected first block to be original text, got %#v", out[1].Content[0])
	}
	if _, ok := out[1].Content[1].(*anthropicToolUseBlock); ok {
		t.Fatalf("expected no tool_use block after sanitization, got %#v", out[1].Content[1])
	}
	stub, ok := out[1].Content[1].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected second block to be text stub for interrupted tool call, got %#v", out[1].Content[1])
	}
	for _, want := range []string{"tool call interrupted", "id:call-1", "name:shell"} {
		if !strings.Contains(stub.Text, want) {
			t.Fatalf("interrupted tool stub %q missing %q", stub.Text, want)
		}
	}
}

func TestBuildAnthropicMessages_AppendsSyntheticUserAfterTrailingAssistant(t *testing.T) {
	summary := core.Message{
		Role:        core.RoleUser,
		Parts:       []core.Part{{Type: core.PartText, Text: summaryPrefix + "Previous context"}},
		CacheAnchor: true,
	}
	ack := core.AssistantText("I've reviewed the context summary. I'll continue from where we left off.")

	_, out := buildAnthropicMessages([]core.Message{summary, ack})
	if len(out) != 3 {
		t.Fatalf("expected 3 anthropic messages after normalization, got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != "user" {
		t.Fatalf("expected trailing anthropic role user, got %v", last.Role)
	}
	if len(last.Content) != 1 {
		t.Fatalf("expected trailing anthropic message to be a single text user block, got %#v", last.Content)
	}
	block, ok := last.Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected trailing anthropic message to be a text user block, got %#v", last.Content)
	}
	if got := block.Text; got != "Continue from the conversation state above." {
		t.Fatalf("synthetic user text = %q, want %q", got, "Continue from the conversation state above.")
	}
}

func TestHandleAnthropicStartBlockContent_TextBlockEmitsTextDelta(t *testing.T) {
	events := make(chan core.Event, 1)
	acc := newToolCallAccumulator()

	if err := handleAnthropicStartBlockContent(llmstream.NewSender(context.Background(), events), anthropicResponseBlock{
		Type: "text",
		Text: "hello from start block",
	}, 0, acc); err != nil {
		t.Fatalf("handleAnthropicStartBlockContent error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != core.EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %v", ev.Type)
		}
		if ev.Text != "hello from start block" {
			t.Fatalf("expected start-block text to be emitted, got %q", ev.Text)
		}
	default:
		t.Fatal("expected one core.EventTextDelta from start text block")
	}
}

func TestHandleAnthropicStartBlockContent_ToolUseStartsAccumulator(t *testing.T) {
	events := make(chan core.Event, 1)
	acc := newToolCallAccumulator()

	if err := handleAnthropicStartBlockContent(llmstream.NewSender(context.Background(), events), anthropicResponseBlock{
		Type:  "tool_use",
		ID:    "call-1",
		Name:  "read_file",
		Input: json.RawMessage(`{"path":"README.md"}`),
	}, 1, acc); err != nil {
		t.Fatalf("handleAnthropicStartBlockContent error: %v", err)
	}

	call, ok := acc.Finish(1)
	if !ok {
		t.Fatal("expected tool call to be started in accumulator")
	}
	if call.ID != "call-1" || call.Name != "read_file" || string(call.Arguments) != `{"path":"README.md"}` {
		t.Fatalf("accumulated tool call = %#v", call)
	}
	select {
	case ev := <-events:
		t.Fatalf("did not expect direct event for tool_use start, got %+v", ev)
	default:
	}
}

func TestEmitReasoningDelta_ProducesReasoningEvent(t *testing.T) {
	events := make(chan core.Event, 1)

	if err := emitReasoningDelta(llmstream.NewSender(context.Background(), events), "thinking chunk", "sig-123"); err != nil {
		t.Fatalf("emitReasoningDelta error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != core.EventReasoningDelta {
			t.Fatalf("expected EventReasoningDelta, got %v", ev.Type)
		}
		if ev.Text != "thinking chunk" {
			t.Fatalf("expected reasoning text chunk, got %q", ev.Text)
		}
		if ev.ReasoningKind != core.ReasoningKindRaw {
			t.Fatalf("expected reasoning kind raw, got %q", ev.ReasoningKind)
		}
		if ev.ReasoningEncryptedContent != "sig-123" {
			t.Fatalf("expected reasoning signature sig-123, got %q", ev.ReasoningEncryptedContent)
		}
	default:
		t.Fatal("expected reasoning event")
	}
}

func TestEmitReasoningDelta_SignatureOnlyIsEncrypted(t *testing.T) {
	events := make(chan core.Event, 1)

	if err := emitReasoningDelta(llmstream.NewSender(context.Background(), events), "", "sig-only"); err != nil {
		t.Fatalf("emitReasoningDelta error: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != core.EventReasoningDelta {
			t.Fatalf("expected EventReasoningDelta, got %v", ev.Type)
		}
		if ev.Text != "" {
			t.Fatalf("expected no reasoning text, got %q", ev.Text)
		}
		if ev.ReasoningKind != core.ReasoningKindEncrypted {
			t.Fatalf("expected encrypted reasoning kind, got %q", ev.ReasoningKind)
		}
		if ev.ReasoningEncryptedContent != "sig-only" {
			t.Fatalf("expected encrypted signature sig-only, got %q", ev.ReasoningEncryptedContent)
		}
	default:
		t.Fatal("expected reasoning event")
	}
}

// TestBuildAnthropicTools_LastToolHasCacheControl covers the standard and beta
// flag; the former beta variant is the second subtest.
func TestBuildAnthropicTools_LastToolHasCacheControl(t *testing.T) {
	specs := []core.ToolSpec{
		{Name: "tool_a", Description: "first tool", Schema: map[string]interface{}{}},
		{Name: "tool_b", Description: "last tool", Schema: map[string]interface{}{}},
	}
	for _, beta := range []bool{false, true} {
		name := "standard"
		if beta {
			name = "beta"
		}
		t.Run(name, func(t *testing.T) {
			tools := buildAnthropicTools(specs, beta)
			if len(tools) != 2 {
				t.Fatalf("expected 2 tools, got %d", len(tools))
			}
			// Only last tool should have cache_control set
			if tools[0].CacheControl != nil {
				t.Fatalf("first tool should not have cache_control, got %+v", tools[0].CacheControl)
			}
			if tools[1].CacheControl == nil || tools[1].CacheControl.Type != "ephemeral" {
				t.Fatalf("last tool should have cache_control type=ephemeral, got %+v", tools[1].CacheControl)
			}
		})
	}
}

func TestBuildAnthropicTools_EmptySpecsReturnsNil(t *testing.T) {
	for _, beta := range []bool{false, true} {
		if tools := buildAnthropicTools(nil, beta); tools != nil {
			t.Fatalf("buildAnthropicTools(nil, %v) = %v, want nil", beta, tools)
		}
	}
}

// TestBuildAnthropicTools_BetaDescriptionAlwaysSet documents the one way the
// standard and beta flag still differ: beta always sends a description (an
// empty string keeps the field present), the standard endpoint omits empty ones.
func TestBuildAnthropicTools_BetaDescriptionAlwaysSet(t *testing.T) {
	specs := []core.ToolSpec{{Name: "tool_a", Schema: map[string]interface{}{}}}

	standard := buildAnthropicTools(specs, false)
	if standard[0].Description != nil {
		t.Fatalf("standard tool should omit an empty description, got %q", *standard[0].Description)
	}

	beta := buildAnthropicTools(specs, true)
	if beta[0].Description == nil {
		t.Fatalf("beta tool should always carry a description")
	}
	if *beta[0].Description != "" {
		t.Fatalf("beta tool description = %q, want empty", *beta[0].Description)
	}

	described := buildAnthropicTools([]core.ToolSpec{{Name: "tool_b", Description: "described", Schema: map[string]interface{}{}}}, false)
	if described[0].Description == nil || *described[0].Description != "described" {
		t.Fatalf("standard tool should keep a non-empty description, got %+v", described[0].Description)
	}
}

func TestApplyLastMessageCacheControl_TextBlock(t *testing.T) {
	messages := []anthropicMessage{
		{Role: "user", Content: []anthropicBlock{newAnthropicTextBlock("hello")}},
	}
	applyLastMessageCacheControl(messages)
	block, ok := messages[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected text block, got %T", messages[0].Content[0])
	}
	if block.CacheControl == nil || block.CacheControl.Type != "ephemeral" {
		t.Fatalf("expected cache_control type=ephemeral, got %+v", block.CacheControl)
	}
}

func TestApplyLastMessageCacheControl_ToolResult(t *testing.T) {
	toolBlock := &anthropicToolResultBlock{
		Type:      "tool_result",
		ToolUseID: "call-1",
	}
	messages := []anthropicMessage{
		{
			Role:    "user",
			Content: []anthropicBlock{toolBlock},
		},
	}
	applyLastMessageCacheControl(messages)
	block, ok := messages[0].Content[0].(*anthropicToolResultBlock)
	if !ok {
		t.Fatalf("expected tool_result block, got %T", messages[0].Content[0])
	}
	if block.CacheControl == nil || block.CacheControl.Type != "ephemeral" {
		t.Fatalf("expected cache_control type=ephemeral, got %+v", block.CacheControl)
	}
}

func TestApplyLastMessageCacheControl_EmptyMessages(t *testing.T) {
	// Should not panic on empty slice
	applyLastMessageCacheControl(nil)
	applyLastMessageCacheControl([]anthropicMessage{})
}

func TestApplyLastMessageCacheControl_EmptyContent(t *testing.T) {
	messages := []anthropicMessage{
		{Role: "user", Content: []anthropicBlock{}},
	}
	// Should not panic on empty content
	applyLastMessageCacheControl(messages)
}

func TestApplyLastMessageCacheControl_OnlyLastMessageAffected(t *testing.T) {
	messages := []anthropicMessage{
		{Role: "user", Content: []anthropicBlock{newAnthropicTextBlock("first")}},
		{Role: "user", Content: []anthropicBlock{newAnthropicTextBlock("second")}},
	}
	applyLastMessageCacheControl(messages)
	first, ok := messages[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected first text block, got %T", messages[0].Content[0])
	}
	if first.CacheControl != nil {
		t.Fatalf("first message should not have cache_control, got %+v", first.CacheControl)
	}
	last, ok := messages[1].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected last text block, got %T", messages[1].Content[0])
	}
	if last.CacheControl == nil || last.CacheControl.Type != "ephemeral" {
		t.Fatalf("last message should have cache_control type=ephemeral, got %+v", last.CacheControl)
	}
}

func TestBuildAnthropicTools_SingleTool(t *testing.T) {
	specs := []core.ToolSpec{
		{
			Name:        "only_tool",
			Description: "the only tool",
			Schema:      map[string]interface{}{},
		},
	}
	tools := buildAnthropicTools(specs, false)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].CacheControl == nil || tools[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("single tool should have cache_control type=ephemeral, got %+v", tools[0].CacheControl)
	}
}

func TestBuildAnthropicTools_ToolHasSchema(t *testing.T) {
	specs := []core.ToolSpec{
		{
			Name:        "read_file",
			Description: "reads a file",
			Schema:      map[string]interface{}{},
		},
	}
	tools := buildAnthropicTools(specs, false)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	schema := tools[0].InputSchema
	if schema == nil {
		t.Fatalf("expected input schema")
	}
	if schema.Type != "object" {
		t.Fatalf("expected schema type=object, got %q", schema.Type)
	}
}

func TestBuildAnthropicMessages_CollaborativeActivityEnvelopeWrappedOnce(t *testing.T) {
	activity := `<collaborative_shell_activity source="browser-terminal" id="sha256:test">data</collaborative_shell_activity>`
	messages := []core.Message{{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: activity}}}, core.UserText("continue")}
	_, standard := buildAnthropicMessages(messages)
	block, ok := standard[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected text block, got %T", standard[0].Content[0])
	}
	if got := block.Text; got != "<developer>\n"+activity+"\n</developer>\n\ncontinue" || strings.Count(got, "<developer>") != 1 {
		t.Fatalf("standard activity = %q", got)
	}
}

func TestBuildAnthropicMessages_DeveloperRole(t *testing.T) {
	messages := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "Be concise"}}},
		core.UserText("Hello"),
	}

	_, out := buildAnthropicMessages(messages)

	if len(out) != 1 {
		t.Fatalf("expected 1 message (developer merged into user), got %d", len(out))
	}
	if out[0].Role != "user" {
		t.Fatalf("expected user role, got %v", out[0].Role)
	}
	if len(out[0].Content) != 1 {
		t.Fatalf("expected single text block, got %#v", out[0].Content)
	}
	block, ok := out[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected single text block, got %#v", out[0].Content)
	}
	want := "<developer>\nBe concise\n</developer>\n\nHello"
	if block.Text != want {
		t.Errorf("content = %q, want %q", block.Text, want)
	}
}

func TestBuildAnthropicMessages_DeveloperBeforeAssistantPreservesBoundary(t *testing.T) {
	messages := []core.Message{
		core.UserText("question"),
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "before assistant"}}},
		core.AssistantText("answer"),
	}
	_, standard := buildAnthropicMessages(messages)
	if len(standard) < 2 || standard[0].Role != "user" || standard[1].Role != "assistant" || len(standard[0].Content) != 2 {
		t.Fatalf("standard ordering = %#v", standard)
	}
	block, ok := standard[0].Content[1].(*anthropicTextBlock)
	if !ok || block.Text != "<developer>\nbefore assistant\n</developer>" {
		t.Fatalf("standard developer boundary = %#v", standard[0].Content[1])
	}
}

func TestBuildAnthropicMessages_DeveloperBeforeToolPreservesBoundary(t *testing.T) {
	toolResult := core.Message{Role: core.RoleTool, Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "call-1", Name: "shell", Content: "ok"}}}}
	assistantCall := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "call-1", Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}}}}
	messages := []core.Message{
		core.UserText("question"),
		assistantCall,
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "before tool"}}},
		toolResult,
	}
	_, standard := buildAnthropicMessages(messages)
	if len(standard) != 3 || standard[2].Role != "user" || len(standard[2].Content) != 2 {
		t.Fatalf("standard tool ordering = %#v", standard)
	}
	block, ok := standard[2].Content[0].(*anthropicTextBlock)
	if !ok || block.Text != "<developer>\nbefore tool\n</developer>\n\n" {
		t.Fatalf("standard tool developer boundary = %#v", standard[2].Content[0])
	}
	if _, ok := standard[2].Content[1].(*anthropicToolResultBlock); !ok {
		t.Fatalf("standard tool result missing: %#v", standard[2].Content[1])
	}
}

func TestBuildAnthropicMessages_TrailingDeveloper(t *testing.T) {
	messages := []core.Message{
		core.UserText("Hello"),
		core.AssistantText("Hi"),
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "Be concise"}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "Use examples"}}},
	}

	_, out := buildAnthropicMessages(messages)
	if len(out) != 3 || out[2].Role != "user" || len(out[2].Content) != 1 {
		t.Fatalf("expected synthetic trailing user developer block, got %#v", out)
	}
	block, ok := out[2].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected synthetic trailing text block, got %#v", out[2].Content)
	}
	if got, want := block.Text, "<developer>\nBe concise\n\nUse examples\n</developer>"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestBuildAnthropicMessages_AdjacentAndEmptyDeveloper(t *testing.T) {
	messages := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "first"}}},
		{Role: core.RoleDeveloper},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "second"}}},
		core.UserText("hello"),
	}
	_, out := buildAnthropicMessages(messages)
	block, ok := out[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected text block, got %T", out[0].Content[0])
	}
	if got, want := block.Text, "<developer>\nfirst\n\nsecond\n</developer>\n\nhello"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestBuildAnthropicMessages_DeveloperOnlyHistory checks the synthetic-user
// shape for a developer-only history, formerly asserted by the beta variant.
func TestBuildAnthropicMessages_DeveloperOnlyHistory(t *testing.T) {
	messages := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "first"}}},
		{Role: core.RoleDeveloper},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "second"}}},
	}
	_, out := buildAnthropicMessages(messages)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("expected one synthetic user, got %#v", out)
	}
	block, ok := out[0].Content[0].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected synthetic user text block, got %#v", out[0].Content)
	}
	if got, want := block.Text, "<developer>\nfirst\n\nsecond\n</developer>"; got != want {
		t.Fatalf("developer-only content = %q, want %q", got, want)
	}
}

func TestBuildAnthropicMessages_TrailingDeveloperAppendsToUser(t *testing.T) {
	_, out := buildAnthropicMessages([]core.Message{core.UserText("hello"), {Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "note"}}}})
	if len(out) != 1 || len(out[0].Content) != 2 {
		t.Fatalf("expected trailing block on existing user, got %#v", out)
	}
	block, ok := out[0].Content[1].(*anthropicTextBlock)
	if !ok {
		t.Fatalf("expected trailing text block, got %#v", out[0].Content[1])
	}
	if got := block.Text; got != "<developer>\nnote\n</developer>" {
		t.Fatalf("trailing developer = %q", got)
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

			provider := newAnthropicTestProvider(t, ts.URL, "claude-sonnet-4-6")

			stream, err := provider.Stream(context.Background(), core.Request{
				Messages: []core.Message{core.UserText("test")},
				Search:   true,
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
				case core.EventToolExecStart:
					if ev.ToolName != "web_search" {
						t.Errorf("EventToolExecStart ToolName = %q, want %q", ev.ToolName, "web_search")
					}
					execStarts++
				case core.EventToolExecEnd:
					if ev.ToolName != "web_search" {
						t.Errorf("EventToolExecEnd ToolName = %q, want %q", ev.ToolName, "web_search")
					}
					if !ev.ToolSuccess {
						t.Error("EventToolExecEnd ToolSuccess = false, want true")
					}
					execEnds++
				case core.EventTextDelta:
					textContent += ev.Text
				case core.EventDone:
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

func TestAnthropicStreamsRejectMissingMessageStop(t *testing.T) {
	tests := []struct {
		name      string
		search    bool
		sse       string
		wantEvent core.EventType
	}{
		{
			name:      "standard partial text",
			wantEvent: core.EventTextDelta,
			sse: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n",
		},
		{
			name:      "beta active server tool",
			search:    true,
			wantEvent: core.EventToolExecStart,
			sse: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}` + "\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tt.sse)
			}))
			defer ts.Close()

			provider := newAnthropicTestProvider(t, ts.URL, "claude-sonnet-4-6")
			stream, err := provider.Stream(context.Background(), core.Request{
				Messages: []core.Message{core.UserText("test")},
				Search:   tt.search,
			})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			defer stream.Close()

			var sawPartial, sawError, sawDone, sawSuccessfulToolEnd, sawFailedToolEnd bool
			for {
				event, recvErr := stream.Recv()
				if errors.Is(recvErr, io.EOF) {
					break
				}
				if recvErr != nil {
					t.Fatalf("Recv() error: %v", recvErr)
				}
				if event.Type == tt.wantEvent {
					sawPartial = true
				}
				switch event.Type {
				case core.EventError:
					sawError = true
					var incomplete *llmstream.StreamIncompleteError
					if !errors.As(event.Err, &incomplete) {
						t.Fatalf("error = %T %v, want llmstream.StreamIncompleteError", event.Err, event.Err)
					}
					if incomplete.Transport != "Anthropic SSE" || incomplete.Terminal != "message_stop" {
						t.Errorf("incomplete error = %+v, want Anthropic SSE message_stop", incomplete)
					}
				case core.EventDone:
					sawDone = true
				case core.EventToolExecEnd:
					if event.ToolSuccess {
						sawSuccessfulToolEnd = true
					} else {
						sawFailedToolEnd = true
					}
				}
			}

			if !sawPartial {
				t.Fatalf("missing partial stream event %v", tt.wantEvent)
			}
			if !sawError {
				t.Fatal("missing core.EventError")
			}
			if sawDone {
				t.Fatal("truncated stream emitted EventDone")
			}
			if sawSuccessfulToolEnd {
				t.Fatal("truncated stream reported server tool success")
			}
			if tt.search && !sawFailedToolEnd {
				t.Fatal("truncated server tool did not emit failed end event")
			}
		})
	}
}

func TestValidateAnthropicStreamEnd(t *testing.T) {
	tests := []struct {
		name           string
		streamErr      error
		sawMessageStop bool
		wantIncomplete bool
		wantOtherError bool
	}{
		{name: "direct HTTP terminal", sawMessageStop: true},
		{name: "Bedrock terminal EOF", streamErr: io.EOF, sawMessageStop: true},
		{name: "direct HTTP truncated", wantIncomplete: true},
		{name: "Bedrock truncated EOF", streamErr: io.EOF, wantIncomplete: true},
		{name: "other stream error", streamErr: errors.New("decode failed"), wantOtherError: true},
		{name: "Bedrock frame cut mid-message", streamErr: fmt.Errorf("event stream: incomplete frame: %w", io.ErrUnexpectedEOF), wantIncomplete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAnthropicStreamEnd(tt.streamErr, tt.sawMessageStop)
			var incomplete *llmstream.StreamIncompleteError
			if got := errors.As(err, &incomplete); got != tt.wantIncomplete {
				t.Fatalf("incomplete = %v, error = %v, want %v", got, err, tt.wantIncomplete)
			}
			if tt.wantOtherError && (err == nil || !strings.Contains(err.Error(), "decode failed")) {
				t.Fatalf("error = %v, want original stream error", err)
			}
			if !tt.wantIncomplete && !tt.wantOtherError && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}

func TestFinishAnthropicServerToolFailurePreservesBothErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	streamErr := &llmstream.StreamIncompleteError{Transport: "Anthropic SSE", Terminal: "message_stop"}
	err := finishAnthropicServerToolFailure(llmstream.NewSender(ctx, make(chan core.Event)), "web_search", streamErr)
	if !errors.Is(err, streamErr) {
		t.Fatalf("error = %v, want original stream error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want send cancellation", err)
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

	provider := newAnthropicTestProvider(t, ts.URL, "")

	stream, err := provider.Stream(context.Background(), core.Request{
		Messages: []core.Message{{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hi"}}}},
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

			provider := newAnthropicTestProvider(t, ts.URL, "claude-sonnet-4-6")

			stream, err := provider.Stream(context.Background(), core.Request{
				Messages:        []core.Message{core.UserText("hello")},
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
				if ev.Type == core.EventDone {
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

	provider := newAnthropicTestProvider(t, ts.URL, "claude-sonnet-4-6")

	stream, err := provider.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("hello")},
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
		if ev.Type == core.EventDone {
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
