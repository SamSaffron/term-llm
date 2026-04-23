package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	ollamaChatDefaultModel   = "qwen2.5-coder:7b"
	ollamaChatDefaultBaseURL = "http://localhost:11434"
)

// OllamaOptions holds Ollama-native generation knobs that have no equivalent
// in the shared Request struct.
type OllamaOptions struct {
	Think           *bool
	TopK            *int
	MinP            *float64
	PresencePenalty *float64
	NumCtx          *int
	NumPredict      *int
}

// OllamaProvider implements Provider using the native Ollama /api/chat endpoint.
// It supports the think flag (for extended reasoning models like Qwen3),
// tool calls, and Ollama-native sampling options.
type OllamaProvider struct {
	baseURL string
	model   string
	opts    OllamaOptions
}

// NewOllamaChatProvider creates a native Ollama chat provider.
// baseURL defaults to http://localhost:11434 and model defaults to qwen2.5-coder:7b.
func NewOllamaChatProvider(baseURL, model string, opts OllamaOptions) *OllamaProvider {
	if baseURL == "" {
		baseURL = ollamaChatDefaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if model == "" {
		model = ollamaChatDefaultModel
	}
	return &OllamaProvider{baseURL: baseURL, model: model, opts: opts}
}

func (p *OllamaProvider) Name() string {
	return fmt.Sprintf("Ollama (%s)", p.model)
}

func (p *OllamaProvider) Credential() string {
	return "free"
}

func (p *OllamaProvider) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:          true,
		SupportsToolChoice: false,
	}
}

// --- Ollama native API types ---

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaChatMsg `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Think    *bool           `json:"think,omitempty"`
	Options  *ollamaChatOpts `json:"options,omitempty"`
}

type ollamaChatOpts struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MinP            *float64 `json:"min_p,omitempty"`
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
	NumCtx          *int     `json:"num_ctx,omitempty"`
	NumPredict      *int     `json:"num_predict,omitempty"`
}

type ollamaChatMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"` // raw base64, no data: prefix
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string        `json:"type"`
	Function ollamaToolDef `json:"function"`
}

type ollamaToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ollamaChatChunk is one line of the NDJSON streaming response.
type ollamaChatChunk struct {
	Model           string         `json:"model"`
	Message         ollamaMsgChunk `json:"message"`
	Done            bool           `json:"done"`
	DoneReason      string         `json:"done_reason,omitempty"`
	PromptEvalCount int            `json:"prompt_eval_count,omitempty"`
	EvalCount       int            `json:"eval_count,omitempty"`
	Error           string         `json:"error,omitempty"`
}

type ollamaMsgChunk struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// buildOllamaMessages converts internal messages to Ollama's native format.
// Ollama tool results use role "tool" with plain text content; tool calls have
// no ID field. Developer-role messages are folded into the next user turn.
func buildOllamaMessages(messages []Message) []ollamaChatMsg {
	messages = sanitizeToolHistory(messages)

	var result []ollamaChatMsg
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case RoleDeveloper:
			text, _, _ := splitParts(msg.Parts)
			if text != "" {
				if pendingDev != "" {
					pendingDev += "\n\n"
				}
				pendingDev += text
			}
		case RoleSystem:
			text, _, _ := splitParts(msg.Parts)
			if text != "" {
				result = append(result, ollamaChatMsg{Role: "system", Content: text})
			}
		case RoleUser:
			text, _, _ := splitParts(msg.Parts)
			if pendingDev != "" {
				text = fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev) + text
				pendingDev = ""
			}
			var images []string
			for _, part := range msg.Parts {
				if part.Type == PartImage && part.ImageData != nil {
					images = append(images, part.ImageData.Base64)
				}
			}
			if text == "" && len(images) == 0 {
				continue
			}
			result = append(result, ollamaChatMsg{Role: "user", Content: text, Images: images})
		case RoleAssistant:
			text, oaiCalls, _ := splitParts(msg.Parts)
			if len(oaiCalls) > 0 {
				var calls []ollamaToolCall
				for _, tc := range oaiCalls {
					args := json.RawMessage(tc.Function.Arguments)
					if !json.Valid(args) {
						args = json.RawMessage("{}")
					}
					calls = append(calls, ollamaToolCall{
						Function: ollamaToolCallFn{Name: tc.Function.Name, Arguments: args},
					})
				}
				result = append(result, ollamaChatMsg{Role: "assistant", Content: text, ToolCalls: calls})
			} else if text != "" {
				result = append(result, ollamaChatMsg{Role: "assistant", Content: text})
			}
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				result = append(result, ollamaChatMsg{
					Role:    "tool",
					Content: toolResultTextContent(part.ToolResult),
				})
			}
		}
	}

	if pendingDev != "" {
		result = append(result, ollamaChatMsg{
			Role:    "user",
			Content: fmt.Sprintf("<developer>\n%s\n</developer>", pendingDev),
		})
	}
	return result
}

func buildOllamaTools(specs []ToolSpec) ([]ollamaTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]ollamaTool, 0, len(specs))
	for _, spec := range specs {
		schema, err := json.Marshal(spec.Schema)
		if err != nil {
			return nil, fmt.Errorf("marshal tool schema %s: %w", spec.Name, err)
		}
		tools = append(tools, ollamaTool{
			Type: "function",
			Function: ollamaToolDef{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  schema,
			},
		})
	}
	return tools, nil
}

func (p *OllamaProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	// Strip a -think suffix from the model name and enable thinking automatically.
	model := chooseModel(req.Model, p.model)
	think := p.opts.Think
	if strings.HasSuffix(model, "-think") {
		model = strings.TrimSuffix(model, "-think")
		t := true
		think = &t
	}

	messages := buildOllamaMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildOllamaTools(req.Tools)
	if err != nil {
		return nil, err
	}

	// Build options – merge provider-level defaults with per-request overrides.
	opts := &ollamaChatOpts{
		TopK:            p.opts.TopK,
		MinP:            p.opts.MinP,
		PresencePenalty: p.opts.PresencePenalty,
		NumCtx:          p.opts.NumCtx,
	}
	if req.TemperatureSet || req.Temperature != 0 {
		v := float64(req.Temperature)
		opts.Temperature = &v
	}
	if req.TopPSet || req.TopP != 0 {
		v := float64(req.TopP)
		opts.TopP = &v
	}
	// req.MaxOutputTokens takes precedence over provider-level NumPredict.
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		opts.NumPredict = &v
	} else if p.opts.NumPredict != nil {
		opts.NumPredict = p.opts.NumPredict
	}
	if ollamaOptsEmpty(opts) {
		opts = nil
	}

	chatReq := ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Think:    think,
		Options:  opts,
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: Ollama Stream Request ===\n")
		fmt.Fprintf(os.Stderr, "URL: %s/api/chat\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Model: %s\n", model)
		fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		if think != nil && *think {
			fmt.Fprintf(os.Stderr, "Think: enabled\n")
		}
		fmt.Fprintln(os.Stderr, "====================================")
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama request failed (is Ollama running at %s?): %w", p.baseURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("Ollama API error: %s", errBody.Error)
		}
		return nil, fmt.Errorf("Ollama API error (status %d): %s", resp.StatusCode, string(raw))
	}

	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)

		var pendingToolCalls []ollamaToolCall
		var lastUsage *Usage

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var chunk ollamaChatChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}

			if chunk.Error != "" {
				return fmt.Errorf("Ollama API error: %s", chunk.Error)
			}

			if chunk.Message.Thinking != "" {
				if err := send.Send(Event{Type: EventReasoningDelta, Text: chunk.Message.Thinking}); err != nil {
					return err
				}
			}

			if chunk.Message.Content != "" {
				if err := send.Send(Event{Type: EventTextDelta, Text: chunk.Message.Content}); err != nil {
					return err
				}
			}

			pendingToolCalls = append(pendingToolCalls, chunk.Message.ToolCalls...)

			if chunk.Done && (chunk.PromptEvalCount > 0 || chunk.EvalCount > 0) {
				lastUsage = &Usage{
					InputTokens:  chunk.PromptEvalCount,
					OutputTokens: chunk.EvalCount,
				}
			}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("Ollama streaming error: %w", err)
		}

		for i, tc := range pendingToolCalls {
			args := tc.Function.Arguments
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			call := ToolCall{
				ID:        fmt.Sprintf("call_%d", i),
				Name:      tc.Function.Name,
				Arguments: args,
			}
			if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
				return err
			}
		}

		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}

		return send.Send(Event{Type: EventDone})
	}), nil
}

// ListModels returns locally available Ollama models via /api/tags.
func (p *OllamaProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama request failed (is Ollama running at %s?): %w", p.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama API error (status %d): %s", resp.StatusCode, string(raw))
	}

	var tagsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &tagsResp); err != nil {
		return nil, fmt.Errorf("failed to parse tags response: %w", err)
	}

	models := make([]ModelInfo, len(tagsResp.Models))
	for i, m := range tagsResp.Models {
		models[i] = ModelInfo{ID: m.Name, OwnedBy: "ollama"}
	}
	return models, nil
}

func ollamaOptsEmpty(o *ollamaChatOpts) bool {
	return o.Temperature == nil && o.TopP == nil && o.TopK == nil &&
		o.MinP == nil && o.PresencePenalty == nil &&
		o.NumCtx == nil && o.NumPredict == nil
}
