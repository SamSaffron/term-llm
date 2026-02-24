package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestNewAnthropicProviderWithExplicitAPIKey(t *testing.T) {
	// Clear env to isolate test
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

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
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	provider, err := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "env" {
		t.Fatalf("credential=%q, want %q", provider.Credential(), "env")
	}
}

func TestNewAnthropicProviderWithOAuthEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-test-token")

	provider, err := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "oauth_env" {
		t.Fatalf("credential=%q, want %q", provider.Credential(), "oauth_env")
	}
}

func TestNewAnthropicProviderWithSavedOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	// Save OAuth credentials to temp dir
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	creds := &credentials.AnthropicOAuthCredentials{
		AccessToken: "sk-ant-oat01-saved-token",
	}
	if err := credentials.SaveAnthropicOAuthCredentials(creds); err != nil {
		t.Fatalf("failed to save test credentials: %v", err)
	}

	provider, err := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "oauth" {
		t.Fatalf("credential=%q, want %q", provider.Credential(), "oauth")
	}
}

func TestNewAnthropicProviderAPIKeyOverridesOAuthEnv(t *testing.T) {
	// API key should take priority over OAuth env
	t.Setenv("ANTHROPIC_API_KEY", "sk-api-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-oauth-token")

	provider, err := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ANTHROPIC_API_KEY takes priority
	if provider.Credential() != "env" {
		t.Fatalf("credential=%q, want %q (API key should override OAuth)", provider.Credential(), "env")
	}
}

func TestNewAnthropicProviderExplicitKeyOverridesAll(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-oauth-token")

	provider, err := NewAnthropicProvider("sk-explicit-key", "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.Credential() != "api_key" {
		t.Fatalf("credential=%q, want %q (explicit key should override all)", provider.Credential(), "api_key")
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

func TestHandleAnthropicStartBlockContent_TextBlockEmitsTextDelta(t *testing.T) {
	events := make(chan Event, 1)
	acc := newToolCallAccumulator()

	handleAnthropicStartBlockContent(anthropic.TextBlock{
		Text: "hello from start block",
	}, 0, acc, events)

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

	handleAnthropicStartBlockContent(anthropic.ToolUseBlock{
		ID:    "call-1",
		Name:  "read_file",
		Input: json.RawMessage(`{"path":"README.md"}`),
	}, 1, acc, events)

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

	handleAnthropicBetaStartBlockContent(anthropic.BetaTextBlock{
		Text: "hello from beta start block",
	}, 0, acc, events)

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

	emitReasoningDelta(events, "thinking chunk", "sig-123")

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
