package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

func buildAnthropicMessages(messages []core.Message) (string, []anthropicMessage) {
	messages = prepareAnthropicMessages(messages)

	var systemParts []string
	var out []anthropicMessage
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case core.RoleSystem:
			systemParts = append(systemParts, core.CollectTextParts(msg.Parts))
		case core.RoleDeveloper:
			// Anthropic has no native developer role. Buffer the text and prepend
			// it into the next user turn wrapped in <developer> tags.
			pendingDev = appendAnthropicDeveloperText(pendingDev, core.CollectTextParts(msg.Parts))
		case core.RoleUser, core.RoleTool:
			parts := msg.Parts
			if pendingDev != "" {
				parts = prependTextToParts(fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev), parts)
				pendingDev = ""
			}
			blocks := buildAnthropicBlocks(parts, false)
			if len(blocks) > 0 {
				if msg.Role == core.RoleUser && msg.CacheAnchor {
					applyCacheControlToLastBlock(blocks)
				}
				out = append(out, anthropicMessage{Role: "user", Content: blocks})
			}
		case core.RoleAssistant:
			out = flushAnthropicDeveloper(out, pendingDev)
			pendingDev = ""
			blocks := buildAnthropicBlocks(msg.Parts, true)
			if len(blocks) > 0 {
				out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
			}
		}
	}
	out = flushAnthropicDeveloper(out, pendingDev)

	return strings.Join(systemParts, "\n\n"), out
}

func appendAnthropicDeveloperText(pending, text string) string {
	if text == "" {
		return pending
	}
	if pending == "" {
		return text
	}
	return pending + "\n\n" + text
}

func flushAnthropicDeveloper(messages []anthropicMessage, pending string) []anthropicMessage {
	if pending == "" {
		return messages
	}
	block := newAnthropicTextBlock(fmt.Sprintf("<developer>\n%s\n</developer>", pending))
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, block)
		return messages
	}
	return append(messages, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
}

// prependTextToParts prepends prefix to the first PartText part in parts,
// or inserts a new text part at the front if none exists.
func prependTextToParts(prefix string, parts []core.Part) []core.Part {
	for i, p := range parts {
		if p.Type == core.PartText {
			out := make([]core.Part, len(parts))
			copy(out, parts)
			out[i].Text = prefix + out[i].Text
			return out
		}
	}
	return append([]core.Part{{Type: core.PartText, Text: prefix}}, parts...)
}

func prepareAnthropicMessages(messages []core.Message) []core.Message {
	messages = core.SanitizeToolHistory(messages)
	if len(messages) == 0 {
		return nil
	}

	lastRole := messages[len(messages)-1].Role
	if lastRole != core.RoleAssistant {
		return messages
	}

	normalized := append([]core.Message(nil), messages...)
	// Anthropic treats a trailing assistant turn as response prefill. That is
	// deprecated and unsupported on newer Claude models, so convert assistant-
	// ended histories into a normal assistant->user continuation turn.
	normalized = append(normalized, core.UserText("Continue from the conversation state above."))
	return normalized
}

func buildAnthropicBlocks(parts []core.Part, allowToolUse bool) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case core.PartText, core.PartFile:
			if allowToolUse && part.ReasoningEncryptedContent != "" {
				blocks = append(blocks, &anthropicThinkingBlock{Type: "thinking", Signature: part.ReasoningEncryptedContent, Thinking: part.ReasoningContent})
			}
			if part.Text != "" {
				blocks = append(blocks, newAnthropicTextBlock(part.Text))
			}
		case core.PartImage:
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				blocks = append(blocks, newAnthropicBase64ImageBlock(part.ImageData.MediaType, part.ImageData.Base64))
				if part.ImagePath != "" {
					blocks = append(blocks, newAnthropicTextBlock("[image saved at: "+part.ImagePath+"]"))
				}
			}
		case core.PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, &anthropicToolUseBlock{Type: "tool_use", ID: part.ToolCall.ID, Name: part.ToolCall.Name, Input: anthropicToolInput(part.ToolCall.Arguments)})
			}
		case core.PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, toolResultBlock(part.ToolResult))
			}
		}
	}
	return blocks
}

type anthropicResultContent struct {
	text       string
	mimeType   string
	base64Data string
}

func normalizedAnthropicResultContent(result *core.ToolResult) []anthropicResultContent {
	var content []anthropicResultContent
	for _, part := range core.ToolResultContentParts(result) {
		switch part.Type {
		case core.ToolContentPartText:
			if part.Text != "" {
				content = append(content, anthropicResultContent{text: part.Text})
			}
		case core.ToolContentPartImageData:
			mimeType, base64Data, ok := core.ToolResultImageData(part)
			if ok {
				content = append(content, anthropicResultContent{mimeType: mimeType, base64Data: base64Data})
			}
		}
	}
	if len(content) == 0 {
		content = append(content, anthropicResultContent{text: core.ToolResultTextContent(result)})
	}
	return content
}

// anthropicToolInput returns the replayed tool_use input; a call that streamed
// no (or null) arguments is sent as {} because the API rejects a null input.
func anthropicToolInput(args json.RawMessage) json.RawMessage {
	if trimmed := bytes.TrimSpace(args); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("{}")
	}
	return args
}

// toolResultBlock creates a tool result block with structured image support.
func toolResultBlock(result *core.ToolResult) *anthropicToolResultBlock {
	content := normalizedAnthropicResultContent(result)
	blocks := make([]anthropicBlock, 0, len(content))
	for _, part := range content {
		switch {
		case part.text != "":
			blocks = append(blocks, newAnthropicTextBlock(part.text))
		case part.base64Data != "":
			blocks = append(blocks, newAnthropicBase64ImageBlock(part.mimeType, part.base64Data))
		}
		// Empty output yields a tool_result without content; the API rejects
		// both empty text blocks and data-less image blocks.
	}
	if len(blocks) == 0 {
		blocks = nil // omit "content" entirely
	}
	return &anthropicToolResultBlock{Type: "tool_result", ToolUseID: result.ID, IsError: result.IsError, Content: blocks}
}

// buildAnthropicTools converts tool specs. The beta endpoint always sends a
// description (possibly empty); the standard endpoint omits empty ones.
func buildAnthropicTools(specs []core.ToolSpec, beta bool) []anthropicTool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]anthropicTool, 0, len(specs))
	for _, spec := range specs {
		tool := anthropicTool{
			Name: spec.Name,
			InputSchema: &anthropicInputSchema{
				Type:       "object",
				Properties: spec.Schema["properties"],
				Required:   schemaRequired(spec.Schema),
			},
		}
		if beta || spec.Description != "" {
			description := spec.Description
			tool.Description = &description
		}
		tools = append(tools, tool)
	}
	tools[len(tools)-1].CacheControl = newAnthropicEphemeralCache()
	return tools
}

// applyLastMessageCacheControl marks the last content block of the last message
// for caching. This enables incremental conversation caching: each turn, the
// prior conversation becomes a cache hit and only the new turn is processed fresh.
func applyLastMessageCacheControl(messages []anthropicMessage) {
	if len(messages) == 0 {
		return
	}
	applyCacheControlToLastBlock(messages[len(messages)-1].Content)
}

// applyCacheControlToLastBlock applies cache_control: ephemeral to the last block
// in a slice of Anthropic content blocks when that block type supports it. Used
// for both the rolling per-turn breakpoint and the stable summary anchor.
func applyCacheControlToLastBlock(blocks []anthropicBlock) {
	if len(blocks) == 0 {
		return
	}
	blocks[len(blocks)-1].setCacheControl(newAnthropicEphemeralCache())
}

func buildAnthropicToolChoice(choice core.ToolChoice, parallel bool) *anthropicToolChoice {
	switch choice.Mode {
	case core.ToolChoiceNone:
		return &anthropicToolChoice{Type: "none"}
	case core.ToolChoiceRequired:
		return &anthropicToolChoice{Type: "any"}
	case core.ToolChoiceName:
		return &anthropicToolChoice{Type: "tool", Name: choice.Name}
	default:
		disableParallel := !parallel
		return &anthropicToolChoice{Type: "auto", DisableParallelToolUse: &disableParallel}
	}
}

func emitAnthropicTextStart(send stream.Sender, text string) error {
	if text == "" {
		return nil
	}
	return send.Send(core.Event{Type: core.EventTextDelta, Text: text})
}

func handleAnthropicStartBlockContent(send stream.Sender, block anthropicResponseBlock, index int64, accumulator *toolCallAccumulator) error {
	switch block.Type {
	case "text":
		return emitAnthropicTextStart(send, block.Text)
	case "thinking":
		return emitReasoningDelta(send, block.Thinking, block.Signature)
	case "tool_use":
		accumulator.Start(index, anthropicToolCall(block))
	}
	return nil
}

func anthropicToolCall(block anthropicResponseBlock) core.ToolCall {
	return core.ToolCall{ID: block.ID, Name: block.Name, Arguments: toolInputToRaw(block.Input)}
}

func emitReasoningDelta(send stream.Sender, text, encrypted string) error {
	if text == "" && encrypted == "" {
		return nil
	}
	kind := core.ReasoningKindRaw
	if text == "" && encrypted != "" {
		kind = core.ReasoningKindEncrypted
	}
	return send.Send(core.Event{
		Type:                      core.EventReasoningDelta,
		Text:                      text,
		ReasoningKind:             kind,
		ReasoningEncryptedContent: encrypted,
	})
}

func toolInputToRaw(input any) json.RawMessage {
	switch v := input.(type) {
	case json.RawMessage:
		return v
	case []byte:
		return json.RawMessage(v)
	case string:
		return json.RawMessage(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return json.RawMessage(data)
	}
}

type toolCallAccumulator struct {
	calls    map[int64]core.ToolCall
	fallback map[int64]json.RawMessage
	partial  map[int64]*strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls:    make(map[int64]core.ToolCall),
		fallback: make(map[int64]json.RawMessage),
		partial:  make(map[int64]*strings.Builder),
	}
}

func (a *toolCallAccumulator) Start(index int64, call core.ToolCall) {
	if len(call.Arguments) > 0 {
		a.fallback[index] = call.Arguments
	}
	call.Arguments = nil
	a.calls[index] = call
}

func (a *toolCallAccumulator) Append(index int64, partial string) {
	if partial == "" {
		return
	}
	builder := a.partial[index]
	if builder == nil {
		builder = &strings.Builder{}
		a.partial[index] = builder
	}
	builder.WriteString(partial)
}

func (a *toolCallAccumulator) Finish(index int64) (core.ToolCall, bool) {
	call, ok := a.calls[index]
	if !ok {
		return core.ToolCall{}, false
	}
	if builder := a.partial[index]; builder != nil && builder.Len() > 0 {
		call.Arguments = json.RawMessage(builder.String())
	} else if fallback, ok := a.fallback[index]; ok {
		call.Arguments = fallback
	}
	delete(a.calls, index)
	delete(a.partial, index)
	delete(a.fallback, index)
	return call, true
}

func maxTokens(requested, fallback int) int64 {
	if requested > 0 {
		return int64(requested)
	}
	return int64(fallback)
}
