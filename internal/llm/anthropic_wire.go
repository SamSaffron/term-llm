package llm

import "encoding/json"

// Wire types for the subset of the Anthropic Messages API used by term-llm.
// The same JSON is used for the standard and beta (?beta=true) endpoints and,
// after a small envelope rewrite, for AWS Bedrock.

const anthropicCacheEphemeral = "ephemeral"

type anthropicCacheControl struct {
	Type string `json:"type"`
}

func newAnthropicEphemeralCache() *anthropicCacheControl {
	return &anthropicCacheControl{Type: anthropicCacheEphemeral}
}

// anthropicBlock is a request content block. setCacheControl reports whether
// the block type accepts a cache_control breakpoint.
type anthropicBlock interface {
	setCacheControl(*anthropicCacheControl) bool
}

type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func newAnthropicTextBlock(text string) *anthropicTextBlock {
	return &anthropicTextBlock{Type: "text", Text: text}
}

func (b *anthropicTextBlock) setCacheControl(cc *anthropicCacheControl) bool {
	b.CacheControl = cc
	return true
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicImageBlock struct {
	Type         string                 `json:"type"`
	Source       anthropicImageSource   `json:"source"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func newAnthropicBase64ImageBlock(mediaType, data string) *anthropicImageBlock {
	return &anthropicImageBlock{Type: "image", Source: anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}}
}

func (b *anthropicImageBlock) setCacheControl(cc *anthropicCacheControl) bool {
	b.CacheControl = cc
	return true
}

type anthropicThinkingBlock struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
	Thinking  string `json:"thinking"`
}

func (b *anthropicThinkingBlock) setCacheControl(*anthropicCacheControl) bool { return false }

type anthropicToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (b *anthropicToolUseBlock) setCacheControl(*anthropicCacheControl) bool { return false }

type anthropicToolResultBlock struct {
	Type         string                 `json:"type"`
	ToolUseID    string                 `json:"tool_use_id"`
	IsError      bool                   `json:"is_error"`
	Content      []anthropicBlock       `json:"content,omitzero"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func (b *anthropicToolResultBlock) setCacheControl(cc *anthropicCacheControl) bool {
	b.CacheControl = cc
	return true
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicInputSchema struct {
	Type       string   `json:"type"`
	Properties any      `json:"properties,omitempty"`
	Required   []string `json:"required,omitzero"`
}

// anthropicTool is either a client tool (InputSchema set) or a server tool
// (Type set, e.g. web_search_20250305).
type anthropicTool struct {
	Type         string                 `json:"type,omitempty"`
	Name         string                 `json:"name"`
	Description  *string                `json:"description,omitempty"`
	InputSchema  *anthropicInputSchema  `json:"input_schema,omitempty"`
	MaxUses      int64                  `json:"max_uses,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicMessagesRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int64                  `json:"max_tokens"`
	Messages     []anthropicMessage     `json:"messages"`
	System       []*anthropicTextBlock  `json:"system,omitempty"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   *anthropicToolChoice   `json:"tool_choice,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream       bool                   `json:"stream"`
}

// ---- streaming response ----

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type anthropicResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
}

type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int64  `json:"index"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock anthropicResponseBlock `json:"content_block"`
	Delta        anthropicDelta         `json:"delta"`
	Usage        anthropicUsage         `json:"usage"`
	Error        anthropicErrorDetail   `json:"error"`
}
